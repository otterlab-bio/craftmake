package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/otterlab-bio/craftmake/internal/dag"
	"github.com/otterlab-bio/craftmake/internal/spec"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func Compile(workflow *spec.WorkflowSpec, context *Context) (*Plan, error) {
	if err := workflow.Validate(); err != nil {
		return nil, err
	}
	if context.Workflow.Mode != "STANDALONE" && len(workflow.On.Otter.Modes) > 0 && !containsFold(workflow.On.Otter.Modes, context.Workflow.Mode) {
		return nil, fmt.Errorf("workflow %q does not support mode %q", workflow.Name, context.Workflow.Mode)
	}

	plan := &Plan{
		Workflow: workflow.On.Otter.Workflow,
		Phase:    workflow.On.Otter.Phase,
		Name:     workflow.Name,
		TaskByID: make(map[string]*Task),
		Source:   workflow,
	}
	jobTaskIDs := make(map[string][]string)
	jobSpecs := make(map[string]spec.JobSpec)
	jobIDs := sortedJobIDs(workflow.Jobs)

	for _, jobID := range jobIDs {
		job := workflow.Jobs[jobID]
		jobSpecs[jobID] = job
		dimensionSets, err := expandDimensions(job.Dimensions, context)
		if err != nil {
			return nil, fmt.Errorf("expand job %q: %w", jobID, err)
		}
		for _, dimensions := range dimensionSets {
			task, err := compileTaskSkeleton(workflow, jobID, job, context, dimensions)
			if err != nil {
				return nil, err
			}
			if _, exists := plan.TaskByID[task.ID]; exists {
				return nil, fmt.Errorf("duplicate task ID %q", task.ID)
			}
			plan.Tasks = append(plan.Tasks, task)
			plan.TaskByID[task.ID] = task
			jobTaskIDs[jobID] = append(jobTaskIDs[jobID], task.ID)
		}
	}

	outputOwners := make(map[string]string)
	for _, task := range plan.Tasks {
		for outputName, outputPath := range task.Outputs {
			cleanPath := filepath.Clean(outputPath)
			if owner, exists := outputOwners[cleanPath]; exists {
				return nil, fmt.Errorf("output conflict: %q is produced by %q and %q", cleanPath, owner, task.ID)
			}
			if outputPath == "" {
				return nil, fmt.Errorf("task %q output %q rendered empty", task.ID, outputName)
			}
			outputOwners[cleanPath] = task.ID
		}
	}

	for _, task := range plan.Tasks {
		job := jobSpecs[task.JobID]
		values := taskValues(context, task.Dimensions, task.Inputs, task.Outputs, task.Resources, task.Worker, "", "")
		for inputName, inputTemplate := range job.Inputs {
			if expression, complete := CompleteExpression(inputTemplate); complete && strings.HasPrefix(expression, "jobs.") {
				paths, dependencies, err := resolveJobOutputReference(expression, task, plan.TaskByID, jobTaskIDs)
				if err != nil {
					return nil, fmt.Errorf("task %q input %q: %w", task.ID, inputName, err)
				}
				task.Inputs[inputName] = paths
				task.Dependencies = append(task.Dependencies, dependencies...)
				continue
			}
			rendered, err := Render(inputTemplate, values)
			if err != nil {
				return nil, fmt.Errorf("task %q input %q: %w", task.ID, inputName, err)
			}
			task.Inputs[inputName] = []string{rendered}
			values["inputs"] = stringSlicesToAny(task.Inputs)
		}
		for _, neededJobID := range job.Needs {
			candidateIDs, exists := jobTaskIDs[neededJobID]
			if !exists {
				return nil, fmt.Errorf("job %q needs unknown job %q", task.JobID, neededJobID)
			}
			for _, candidateID := range candidateIDs {
				candidate := plan.TaskByID[candidateID]
				if dimensionsCompatible(task.Dimensions, candidate.Dimensions) || len(task.Dimensions) == 0 {
					task.Dependencies = append(task.Dependencies, candidateID)
				}
			}
		}
		task.Dependencies = uniqueSorted(task.Dependencies)
		values = taskValues(context, task.Dimensions, task.Inputs, task.Outputs, task.Resources, task.Worker, "", "")
		steps, err := compileSteps(workflow.Defaults, job, values)
		if err != nil {
			return nil, fmt.Errorf("task %q: %w", task.ID, err)
		}
		task.Steps = steps
		task.Fingerprint, err = taskDefinitionFingerprint(task)
		if err != nil {
			return nil, err
		}
	}

	graph := dag.New()
	for _, task := range plan.Tasks {
		graph.AddNode(task.ID)
	}
	for _, task := range plan.Tasks {
		for _, dependencyID := range task.Dependencies {
			if err := graph.AddEdge(dependencyID, task.ID); err != nil {
				return nil, err
			}
		}
	}
	order, err := graph.TopologicalOrder()
	if err != nil {
		return nil, err
	}
	plan.Order = order
	plan.Submissions, err = buildSubmissionGroups(plan, jobSpecs)
	if err != nil {
		return nil, err
	}
	if err := validatePhaseResourceEnvelope(plan, context); err != nil {
		return nil, err
	}
	return plan, nil
}

func compileTaskSkeleton(workflow *spec.WorkflowSpec, jobID string, job spec.JobSpec, context *Context, dimensions map[string]string) (*Task, error) {
	memoryBytes, err := ParseMemory(job.Resources.Memory)
	if err != nil {
		return nil, fmt.Errorf("job %q resources: %w", jobID, err)
	}
	resources := protocol.ResourceRequest{Cores: job.Resources.Cores, MemoryByte: memoryBytes, Partition: job.Resources.Partition, Time: job.Resources.Time}
	if phaseEnvelope, declared := context.Execution.PhaseResources[workflow.On.Otter.Phase]; declared && resources.Time == "" {
		resources.Time = phaseEnvelope.Time
	}
	if resources.Cores <= 0 {
		resources.Cores = 1
	}
	var workerPlan *WorkerPlan
	if job.Worker != nil {
		workerMemory, err := ParseMemory(job.Worker.Resources.Memory)
		if err != nil {
			return nil, fmt.Errorf("job %q worker resources: %w", jobID, err)
		}
		workerPlan = &WorkerPlan{Resources: protocol.ResourceRequest{Cores: job.Worker.Resources.Cores, MemoryByte: workerMemory, Partition: job.Worker.Resources.Partition, Time: job.Worker.Resources.Time}, MaxParallel: job.Worker.MaxParallel}
		if phaseEnvelope, declared := context.Execution.PhaseResources[workflow.On.Otter.Phase]; declared && workerPlan.Resources.Time == "" {
			workerPlan.Resources.Time = phaseEnvelope.Time
		}
		if workerPlan.MaxParallel <= 0 {
			workerPlan.MaxParallel = 1
		}
		if workerPlan.Resources.Cores <= 0 {
			return nil, fmt.Errorf("batch job %q worker cores must be positive", jobID)
		}
		if resources.Cores < workerPlan.Resources.Cores || resources.MemoryByte < workerPlan.Resources.MemoryByte {
			return nil, fmt.Errorf("batch job %q allocation cannot fit one worker", jobID)
		}
	}
	values := taskValues(context, dimensions, nil, nil, resources, workerPlan, "", "")
	outputs := make(map[string]string, len(job.Outputs))
	for outputName, outputTemplate := range job.Outputs {
		rendered, err := Render(outputTemplate, values)
		if err != nil {
			return nil, fmt.Errorf("job %q output %q: %w", jobID, outputName, err)
		}
		outputs[outputName] = rendered
	}
	name := job.Name
	if name == "" {
		name = jobID
	}
	maxAttempts := job.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	compressSuccessLogs := resolveBoolean(job.Observability.CompressSuccessLogs, workflow.Defaults.Observability.CompressSuccessLogs, true)
	return &Task{ID: stableTaskID(workflow.On.Otter.Workflow, workflow.On.Otter.Phase, jobID, dimensions), JobID: jobID, JobName: name, Workflow: workflow.On.Otter.Workflow, Phase: workflow.On.Otter.Phase, Scope: job.Scope, Dimensions: dimensions, Inputs: make(map[string][]string), Outputs: outputs, Resources: resources, Worker: workerPlan, Environment: choose(job.Environment, workflow.Defaults.Environment), Env: mergeMaps(workflow.Defaults.Env, job.Env), MaxAttempts: maxAttempts, CompressSuccessLogs: compressSuccessLogs}, nil
}

func compileSteps(defaults spec.DefaultsSpec, job spec.JobSpec, values map[string]any) ([]protocol.StepManifest, error) {
	steps := make([]protocol.StepManifest, 0, len(job.Steps))
	for stepIndex, step := range job.Steps {
		command, err := Render(step.Run, values)
		if err != nil {
			return nil, fmt.Errorf("step %d command: %w", stepIndex+1, err)
		}
		logs := make(map[string]string, len(step.Logs))
		for logName, logTemplate := range step.Logs {
			rendered, err := Render(logTemplate, values)
			if err != nil {
				return nil, fmt.Errorf("step %d log %q: %w", stepIndex+1, logName, err)
			}
			logs[logName] = rendered
		}
		stepName := step.Name
		if stepName == "" {
			stepName = fmt.Sprintf("step-%d", stepIndex+1)
		}
		steps = append(steps, protocol.StepManifest{Index: stepIndex + 1, Name: stepName, Shell: choose(step.Shell, defaults.Shell, "bash"), Environment: choose(step.Environment, job.Environment, defaults.Environment), Env: mergeMaps(defaults.Env, job.Env, step.Env), Command: command, Logs: logs})
	}
	return steps, nil
}

func resolveJobOutputReference(expression string, consumer *Task, tasks map[string]*Task, jobTaskIDs map[string][]string) ([]string, []string, error) {
	segments := strings.Split(expression, ".")
	if len(segments) != 4 || segments[0] != "jobs" || segments[2] != "outputs" {
		return nil, nil, fmt.Errorf("invalid job output reference %q", expression)
	}
	producerJobID, outputName := segments[1], segments[3]
	candidateIDs, exists := jobTaskIDs[producerJobID]
	if !exists {
		return nil, nil, fmt.Errorf("unknown producer job %q", producerJobID)
	}
	var paths []string
	var dependencies []string
	for _, candidateID := range candidateIDs {
		candidate := tasks[candidateID]
		if len(consumer.Dimensions) > 0 && !dimensionsCompatible(consumer.Dimensions, candidate.Dimensions) {
			continue
		}
		path, exists := candidate.Outputs[outputName]
		if !exists {
			return nil, nil, fmt.Errorf("job %q has no output %q", producerJobID, outputName)
		}
		paths = append(paths, path)
		dependencies = append(dependencies, candidateID)
	}
	if len(paths) == 0 {
		return nil, nil, fmt.Errorf("no producer instances match consumer dimensions")
	}
	return paths, dependencies, nil
}

func expandDimensions(dimensions []string, context *Context) ([]map[string]string, error) {
	if len(dimensions) == 0 {
		return []map[string]string{{}}, nil
	}
	result := []map[string]string{{}}
	for _, dimension := range dimensions {
		var values []string
		switch dimension {
		case "sample":
			for _, sample := range context.Samples {
				values = append(values, sample.ID)
			}
		case "species":
			for _, species := range context.Species {
				values = append(values, species.Name)
			}
		default:
			return nil, fmt.Errorf("unsupported dimension %q", dimension)
		}
		if len(values) == 0 {
			return nil, fmt.Errorf("dimension %q has no values", dimension)
		}
		var expanded []map[string]string
		for _, existing := range result {
			for _, value := range values {
				copyValues := cloneStrings(existing)
				copyValues[dimension] = value
				expanded = append(expanded, copyValues)
			}
		}
		result = expanded
	}
	return result, nil
}

func taskValues(context *Context, dimensions map[string]string, inputs map[string][]string, outputs map[string]string, resources protocol.ResourceRequest, worker *WorkerPlan, runnerTemp, runnerWork string) map[string]any {
	values := map[string]any{"config": context.Raw, "paths": stringsToAny(context.Paths), "inputs": stringSlicesToAny(inputs), "outputs": stringsToAny(outputs), "resources": map[string]any{"cores": resources.Cores, "memory_bytes": resources.MemoryByte, "partition": resources.Partition, "time": resources.Time}, "runner": map[string]any{"temp": "${CRAFTMAKE_TEMP}", "work": "${CRAFTMAKE_WORK}"}}
	if sampleID := dimensions["sample"]; sampleID != "" {
		for _, sample := range context.Samples {
			if sample.ID == sampleID {
				values["sample"] = map[string]any{"id": sample.ID, "index": sample.Index, "read1": sample.Read1, "read2": sample.Read2, "adapter1": sample.Adapter1, "adapter2": sample.Adapter2}
				break
			}
		}
	}
	if speciesName := dimensions["species"]; speciesName != "" {
		for _, species := range context.Species {
			if species.Name == speciesName {
				values["species"] = map[string]any{"name": species.Name, "index": species.Index, "role": species.Role, "genome_fasta": species.GenomeFasta, "genome_index": species.GenomeIndex, "rnaseq_gtf": species.RNASeqGTF, "rnaseq_reference": species.RNASeqReference}
				break
			}
		}
	}
	if worker != nil {
		values["worker"] = map[string]any{"resources": map[string]any{"cores": worker.Resources.Cores, "memory_bytes": worker.Resources.MemoryByte, "partition": worker.Resources.Partition}, "max_parallel": worker.MaxParallel}
	}
	return values
}

func buildSubmissionGroups(plan *Plan, jobSpecs map[string]spec.JobSpec) ([]SubmissionGroup, error) {
	var groups []SubmissionGroup
	batchGroups := make(map[string]*SubmissionGroup)
	for _, task := range plan.Tasks {
		job := jobSpecs[task.JobID]
		if task.Scope != "batch" {
			groups = append(groups, SubmissionGroup{ID: task.ID, Scope: task.Scope, TaskIDs: []string{task.ID}, Resources: task.Resources, Worker: task.Worker})
			continue
		}
		parts := []string{task.JobID}
		for _, dimension := range job.GroupBy {
			parts = append(parts, dimension+"="+task.Dimensions[dimension])
		}
		key := strings.Join(parts, "/")
		group := batchGroups[key]
		if group == nil {
			group = &SubmissionGroup{ID: stableName(key), Scope: "batch", GroupKey: key, Resources: task.Resources, Worker: task.Worker}
			batchGroups[key] = group
		}
		group.TaskIDs = append(group.TaskIDs, task.ID)
	}
	keys := make([]string, 0, len(batchGroups))
	for key := range batchGroups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := batchGroups[key]
		sort.Strings(group.TaskIDs)
		effectiveParallel := group.Worker.MaxParallel
		if effectiveParallel > len(group.TaskIDs) {
			effectiveParallel = len(group.TaskIDs)
		}
		if effectiveParallel <= 0 {
			effectiveParallel = 1
		}
		requiredCores := group.Worker.Resources.Cores * effectiveParallel
		requiredMemory := group.Worker.Resources.MemoryByte * int64(effectiveParallel)
		if group.Resources.Cores < requiredCores {
			return nil, fmt.Errorf("batch group %q allocation cores %d cannot fit %d workers requiring %d cores each", group.GroupKey, group.Resources.Cores, effectiveParallel, group.Worker.Resources.Cores)
		}
		if group.Resources.MemoryByte < requiredMemory {
			return nil, fmt.Errorf("batch group %q allocation memory %d cannot fit %d workers requiring %d bytes each", group.GroupKey, group.Resources.MemoryByte, effectiveParallel, group.Worker.Resources.MemoryByte)
		}
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(left, right int) bool { return groups[left].ID < groups[right].ID })
	return groups, nil
}

func taskDefinitionFingerprint(task *Task) (string, error) {
	data, err := json.Marshal(task)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
func stableTaskID(workflow, phase, jobID string, dimensions map[string]string) string {
	parts := []string{workflow, phase, jobID}
	keys := make([]string, 0, len(dimensions))
	for key := range dimensions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, key+"="+dimensions[key])
	}
	return strings.Join(parts, "/")
}
func stableName(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "batch-" + hex.EncodeToString(digest[:8])
}
func sortedJobIDs(jobs map[string]spec.JobSpec) []string {
	keys := make([]string, 0, len(jobs))
	for key := range jobs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func dimensionsCompatible(left, right map[string]string) bool {
	for key, leftValue := range left {
		if rightValue, exists := right[key]; exists && rightValue != leftValue {
			return false
		}
	}
	for key, rightValue := range right {
		if leftValue, exists := left[key]; exists && leftValue != rightValue {
			return false
		}
	}
	return true
}
func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
func containsFold(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(value, expected) {
			return true
		}
	}
	return false
}
func choose(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func resolveBoolean(primary, fallback *bool, defaultValue bool) bool {
	if primary != nil {
		return *primary
	}
	if fallback != nil {
		return *fallback
	}
	return defaultValue
}

func mergeMaps(maps ...map[string]string) map[string]string {
	result := map[string]string{}
	for _, values := range maps {
		for key, value := range values {
			result[key] = value
		}
	}
	return result
}
func cloneStrings(values map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range values {
		result[key] = value
	}
	return result
}
func stringsToAny(values map[string]string) map[string]any {
	result := map[string]any{}
	for key, value := range values {
		result[key] = value
	}
	return result
}
func stringSlicesToAny(values map[string][]string) map[string]any {
	result := map[string]any{}
	for key, value := range values {
		if len(value) == 1 {
			result[key] = value[0]
		} else {
			result[key] = strings.Join(value, " ")
		}
	}
	return result
}
