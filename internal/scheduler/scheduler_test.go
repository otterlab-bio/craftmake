package scheduler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/store"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type recordingBackend struct {
	mutex    sync.Mutex
	requests []backend.SubmissionRequest
}

func (recordingBackend *recordingBackend) Name() string { return "recording" }

func (recordingBackend *recordingBackend) RunSubmission(_ context.Context, _ string, request backend.SubmissionRequest) (*backend.SubmissionResult, error) {
	recordingBackend.mutex.Lock()
	recordingBackend.requests = append(recordingBackend.requests, request)
	recordingBackend.mutex.Unlock()

	now := time.Now().UTC()
	outcomes := make(map[string]backend.TaskOutcome, len(request.Manifests))
	for manifestIndex, manifest := range request.Manifests {
		status := "succeeded"
		exitCode := 0
		var taskErr error
		errorMessage := ""
		if manifestIndex == 1 {
			status = "failed"
			exitCode = 1
			errorMessage = "intentional worker failure"
			taskErr = errors.New(errorMessage)
		}
		outcomes[manifest.TaskID] = backend.TaskOutcome{
			Result: &backend.Result{
				BackendID: "allocation-42",
				TaskResult: &protocol.TaskResult{
					ProtocolVersion: protocol.Version,
					RunID:           manifest.RunID,
					TaskID:          manifest.TaskID,
					Attempt:         manifest.Attempt,
					Status:          status,
					StartedAt:       now,
					FinishedAt:      now.Add(time.Second),
					ExitCode:        exitCode,
					Error:           errorMessage,
				},
			},
			Err: taskErr,
		}
	}
	return &backend.SubmissionResult{BackendID: "allocation-42", Tasks: outcomes}, nil
}

func (recordingBackend *recordingBackend) CancelSubmission(context.Context, string, map[string]any) error {
	return nil
}
func (recordingBackend *recordingBackend) Cancel(context.Context) error { return nil }

type retryRecordingBackend struct {
	mutex    sync.Mutex
	requests []backend.SubmissionRequest
}

func (retryBackend *retryRecordingBackend) Name() string { return "retry-recording" }

func (retryBackend *retryRecordingBackend) RunSubmission(_ context.Context, _ string, request backend.SubmissionRequest) (*backend.SubmissionResult, error) {
	retryBackend.mutex.Lock()
	retryBackend.requests = append(retryBackend.requests, request)
	retryBackend.mutex.Unlock()

	now := time.Now().UTC()
	outcomes := make(map[string]backend.TaskOutcome, len(request.Manifests))
	for _, manifest := range request.Manifests {
		status := "succeeded"
		exitCode := 0
		errorMessage := ""
		var taskErr error
		if manifest.Dimensions["sample"] == "B" && manifest.Attempt == 1 {
			status = "failed"
			exitCode = 1
			errorMessage = "transient worker failure"
			taskErr = errors.New(errorMessage)
		}
		outcomes[manifest.TaskID] = backend.TaskOutcome{
			Result: &backend.Result{
				BackendID: "allocation-retry",
				TaskResult: &protocol.TaskResult{
					ProtocolVersion: protocol.Version,
					RunID:           manifest.RunID,
					TaskID:          manifest.TaskID,
					Attempt:         manifest.Attempt,
					Status:          status,
					StartedAt:       now,
					FinishedAt:      now.Add(time.Second),
					ExitCode:        exitCode,
					Error:           errorMessage,
				},
			},
			Err: taskErr,
		}
	}
	return &backend.SubmissionResult{BackendID: "allocation-retry", Tasks: outcomes}, nil
}

func (retryBackend *retryRecordingBackend) CancelSubmission(context.Context, string, map[string]any) error {
	return nil
}
func (retryBackend *retryRecordingBackend) Cancel(context.Context) error { return nil }

func TestSchedulerExecutesBatchAsOneSubmissionAndKeepsTaskOutcomes(t *testing.T) {
	ctx := context.Background()
	stateDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(stateDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	workerPlan := &compiler.WorkerPlan{Resources: protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20}, MaxParallel: 2}
	firstTask := batchTask("workflow/phase/align/sample=A", "A", workerPlan)
	secondTask := batchTask("workflow/phase/align/sample=B", "B", workerPlan)
	plan := &compiler.Plan{
		Workflow: "workflow",
		Phase:    "phase",
		Tasks:    []*compiler.Task{firstTask, secondTask},
		TaskByID: map[string]*compiler.Task{firstTask.ID: firstTask, secondTask.ID: secondTask},
		Order:    []string{firstTask.ID, secondTask.ID},
		Submissions: []compiler.SubmissionGroup{{
			ID:        "batch-human",
			Scope:     "batch",
			GroupKey:  "align/species=human",
			TaskIDs:   []string{firstTask.ID, secondTask.ID},
			Resources: protocol.ResourceRequest{Cores: 2, MemoryByte: 128 << 20},
			Worker:    workerPlan,
		}},
	}
	selectedBackend := &recordingBackend{}
	taskScheduler, err := New(plan, stateStore, Options{
		ProjectDirectory: stateDirectory,
		StateDirectory:   stateDirectory,
		ConfigPath:       filepath.Join(stateDirectory, "config.yaml"),
		WorkflowPath:     filepath.Join(stateDirectory, "workflow.yaml"),
		Backend:          selectedBackend,
		MaxParallel:      1,
		MaxCores:         2,
		MaxMemoryBytes:   128 << 20,
		RunID:            "batch-test-run",
	})
	if err != nil {
		t.Fatal(err)
	}

	runID, runErr := taskScheduler.Run(ctx)
	if runErr == nil {
		t.Fatal("expected workflow failure from one failed batch worker")
	}
	if runID != "batch-test-run" {
		t.Fatalf("unexpected run ID %q", runID)
	}
	if len(selectedBackend.requests) != 1 {
		t.Fatalf("expected one physical submission, received %d", len(selectedBackend.requests))
	}
	if manifestCount := len(selectedBackend.requests[0].Manifests); manifestCount != 2 {
		t.Fatalf("expected two logical task manifests, received %d", manifestCount)
	}
	if selectedBackend.requests[0].WorkerMaxParallel != 2 {
		t.Fatalf("unexpected worker parallelism: %d", selectedBackend.requests[0].WorkerMaxParallel)
	}

	_, counts, err := stateStore.RunSummary(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if counts["succeeded"] != 1 || counts["failed"] != 1 {
		t.Fatalf("unexpected task status counts: %#v", counts)
	}

	rows, err := stateStore.QueryRows(ctx, `SELECT COUNT(*) FROM physical_submissions WHERE run_id = ?`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("submission count query returned no row")
	}
	var submissionCount int
	if err := rows.Scan(&submissionCount); err != nil {
		t.Fatal(err)
	}
	if submissionCount != 1 {
		t.Fatalf("expected one persisted physical submission, received %d", submissionCount)
	}
}

func TestSchedulerRetriesOnlyFailedBatchWorkers(t *testing.T) {
	ctx := context.Background()
	stateDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(stateDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	workerPlan := &compiler.WorkerPlan{Resources: protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20}, MaxParallel: 2}
	firstTask := batchTask("workflow/phase/align/sample=A", "A", workerPlan)
	secondTask := batchTask("workflow/phase/align/sample=B", "B", workerPlan)
	firstTask.MaxAttempts = 2
	secondTask.MaxAttempts = 2
	plan := &compiler.Plan{
		Workflow: "workflow",
		Phase:    "phase",
		Tasks:    []*compiler.Task{firstTask, secondTask},
		TaskByID: map[string]*compiler.Task{firstTask.ID: firstTask, secondTask.ID: secondTask},
		Order:    []string{firstTask.ID, secondTask.ID},
		Submissions: []compiler.SubmissionGroup{{
			ID:        "batch-human",
			Scope:     "batch",
			GroupKey:  "align/species=human",
			TaskIDs:   []string{firstTask.ID, secondTask.ID},
			Resources: protocol.ResourceRequest{Cores: 2, MemoryByte: 128 << 20},
			Worker:    workerPlan,
		}},
	}
	selectedBackend := &retryRecordingBackend{}
	taskScheduler, err := New(plan, stateStore, Options{
		ProjectDirectory: stateDirectory,
		StateDirectory:   stateDirectory,
		ConfigPath:       filepath.Join(stateDirectory, "config.yaml"),
		WorkflowPath:     filepath.Join(stateDirectory, "workflow.yaml"),
		Backend:          selectedBackend,
		MaxParallel:      1,
		MaxCores:         2,
		MaxMemoryBytes:   128 << 20,
		RunID:            "batch-retry-run",
	})
	if err != nil {
		t.Fatal(err)
	}

	runID, runErr := taskScheduler.Run(ctx)
	if runErr != nil {
		t.Fatalf("expected retry to recover the run: %v", runErr)
	}
	if len(selectedBackend.requests) != 2 {
		t.Fatalf("expected two physical submission rounds, received %d", len(selectedBackend.requests))
	}
	if firstRoundCount := len(selectedBackend.requests[0].Manifests); firstRoundCount != 2 {
		t.Fatalf("expected two workers in first round, received %d", firstRoundCount)
	}
	if secondRoundCount := len(selectedBackend.requests[1].Manifests); secondRoundCount != 1 {
		t.Fatalf("expected one worker in retry round, received %d", secondRoundCount)
	}
	retryManifest := selectedBackend.requests[1].Manifests[0]
	if retryManifest.Dimensions["sample"] != "B" || retryManifest.Attempt != 2 {
		t.Fatalf("unexpected retry manifest: task=%s dimensions=%#v attempt=%d", retryManifest.TaskID, retryManifest.Dimensions, retryManifest.Attempt)
	}
	if selectedBackend.requests[1].WorkerMaxParallel != 1 {
		t.Fatalf("retry round should use one worker slot, received %d", selectedBackend.requests[1].WorkerMaxParallel)
	}

	_, counts, err := stateStore.RunSummary(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if counts["succeeded"] != 2 {
		t.Fatalf("unexpected task status counts: %#v", counts)
	}

	rows, err := stateStore.QueryRows(ctx, `
		SELECT task_id, COUNT(*), MAX(attempt_number)
		FROM task_attempts
		WHERE run_id = ?
		GROUP BY task_id
		ORDER BY task_id
	`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	attemptCounts := map[string]int{}
	maximumAttempts := map[string]int{}
	for rows.Next() {
		var taskID string
		var attemptCount int
		var maximumAttempt int
		if err := rows.Scan(&taskID, &attemptCount, &maximumAttempt); err != nil {
			t.Fatal(err)
		}
		attemptCounts[taskID] = attemptCount
		maximumAttempts[taskID] = maximumAttempt
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if attemptCounts[firstTask.ID] != 1 || maximumAttempts[firstTask.ID] != 1 {
		t.Fatalf("successful worker should not be retried: counts=%#v maximum=%#v", attemptCounts, maximumAttempts)
	}
	if attemptCounts[secondTask.ID] != 2 || maximumAttempts[secondTask.ID] != 2 {
		t.Fatalf("failed worker should have two attempts: counts=%#v maximum=%#v", attemptCounts, maximumAttempts)
	}

	submissionRows, err := stateStore.QueryRows(ctx, `SELECT COUNT(*) FROM physical_submissions WHERE run_id = ?`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer submissionRows.Close()
	if !submissionRows.Next() {
		t.Fatal("submission count query returned no row")
	}
	var submissionCount int
	if err := submissionRows.Scan(&submissionCount); err != nil {
		t.Fatal(err)
	}
	if submissionCount != 2 {
		t.Fatalf("expected two persisted physical submissions, received %d", submissionCount)
	}
}

func TestSchedulerRejectsOnlyBatchWorkersWithMissingInputs(t *testing.T) {
	ctx := context.Background()
	projectDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(projectDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	availableInputPath := filepath.Join("inputs", "sample-B.txt")
	if err := os.MkdirAll(filepath.Join(projectDirectory, "inputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDirectory, availableInputPath), []byte("available\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workerPlan := &compiler.WorkerPlan{Resources: protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20}, MaxParallel: 2}
	missingInputTask := batchTask("workflow/phase/align/sample=A", "A", workerPlan)
	missingInputTask.Inputs = map[string][]string{"reads": {filepath.Join("inputs", "sample-A.txt")}}
	availableInputTask := batchTask("workflow/phase/align/sample=B", "B", workerPlan)
	availableInputTask.Inputs = map[string][]string{"reads": {availableInputPath}}
	plan := &compiler.Plan{
		Workflow: "workflow",
		Phase:    "phase",
		Tasks:    []*compiler.Task{missingInputTask, availableInputTask},
		TaskByID: map[string]*compiler.Task{missingInputTask.ID: missingInputTask, availableInputTask.ID: availableInputTask},
		Order:    []string{missingInputTask.ID, availableInputTask.ID},
		Submissions: []compiler.SubmissionGroup{{
			ID:        "batch-human",
			Scope:     "batch",
			GroupKey:  "align/species=human",
			TaskIDs:   []string{missingInputTask.ID, availableInputTask.ID},
			Resources: protocol.ResourceRequest{Cores: 2, MemoryByte: 128 << 20},
			Worker:    workerPlan,
		}},
	}
	selectedBackend := &recordingBackend{}
	taskScheduler, err := New(plan, stateStore, Options{
		ProjectDirectory: projectDirectory,
		StateDirectory:   projectDirectory,
		ConfigPath:       filepath.Join(projectDirectory, "config.yaml"),
		WorkflowPath:     filepath.Join(projectDirectory, "workflow.yaml"),
		Backend:          selectedBackend,
		MaxParallel:      1,
		MaxCores:         2,
		MaxMemoryBytes:   128 << 20,
		RunID:            "batch-input-preflight-run",
	})
	if err != nil {
		t.Fatal(err)
	}

	runID, runErr := taskScheduler.Run(ctx)
	if runErr == nil {
		t.Fatal("expected run failure from the worker with a missing input")
	}
	if len(selectedBackend.requests) != 1 {
		t.Fatalf("expected one physical submission for valid workers, received %d", len(selectedBackend.requests))
	}
	if manifestCount := len(selectedBackend.requests[0].Manifests); manifestCount != 1 {
		t.Fatalf("expected one valid worker manifest, received %d", manifestCount)
	}
	if submittedTaskID := selectedBackend.requests[0].Manifests[0].TaskID; submittedTaskID != availableInputTask.ID {
		t.Fatalf("expected only task %q to be submitted, received %q", availableInputTask.ID, submittedTaskID)
	}
	if selectedBackend.requests[0].WorkerMaxParallel != 1 {
		t.Fatalf("expected worker parallelism to shrink to one, received %d", selectedBackend.requests[0].WorkerMaxParallel)
	}

	_, counts, err := stateStore.RunSummary(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if counts["failed"] != 1 || counts["succeeded"] != 1 {
		t.Fatalf("unexpected task status counts: %#v", counts)
	}

	attemptRows, err := stateStore.QueryRows(ctx, `
		SELECT task_id, COUNT(*)
		FROM task_attempts
		WHERE run_id = ?
		GROUP BY task_id
	`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer attemptRows.Close()
	attemptCounts := map[string]int{}
	for attemptRows.Next() {
		var taskID string
		var attemptCount int
		if err := attemptRows.Scan(&taskID, &attemptCount); err != nil {
			t.Fatal(err)
		}
		attemptCounts[taskID] = attemptCount
	}
	if err := attemptRows.Err(); err != nil {
		t.Fatal(err)
	}
	if attemptCounts[missingInputTask.ID] != 0 {
		t.Fatalf("missing-input task should not have an attempt, received %#v", attemptCounts)
	}
	if attemptCounts[availableInputTask.ID] != 1 {
		t.Fatalf("valid task should have one attempt, received %#v", attemptCounts)
	}
}

type outputProducingBackend struct {
	mutex    sync.Mutex
	requests []backend.SubmissionRequest
}

func (outputBackend *outputProducingBackend) Name() string { return "output-producing" }

func (outputBackend *outputProducingBackend) RunSubmission(_ context.Context, _ string, request backend.SubmissionRequest) (*backend.SubmissionResult, error) {
	outputBackend.mutex.Lock()
	outputBackend.requests = append(outputBackend.requests, request)
	outputBackend.mutex.Unlock()

	now := time.Now().UTC()
	outcomes := make(map[string]backend.TaskOutcome, len(request.Manifests))
	for _, manifest := range request.Manifests {
		for _, outputPath := range manifest.Outputs {
			resolvedOutputPath := outputPath
			if !filepath.IsAbs(resolvedOutputPath) {
				resolvedOutputPath = filepath.Join(manifest.WorkDirectory, resolvedOutputPath)
			}
			if err := os.MkdirAll(filepath.Dir(resolvedOutputPath), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(resolvedOutputPath, []byte("generated\n"), 0o644); err != nil {
				return nil, err
			}
		}
		outcomes[manifest.TaskID] = backend.TaskOutcome{Result: &backend.Result{
			BackendID: "allocation-output-producing",
			TaskResult: &protocol.TaskResult{
				ProtocolVersion: protocol.Version,
				RunID:           manifest.RunID,
				TaskID:          manifest.TaskID,
				Attempt:         manifest.Attempt,
				Status:          "succeeded",
				StartedAt:       now,
				FinishedAt:      now.Add(time.Second),
				ExitCode:        0,
			},
		}}
	}
	return &backend.SubmissionResult{BackendID: "allocation-output-producing", Tasks: outcomes}, nil
}

func (outputBackend *outputProducingBackend) CancelSubmission(context.Context, string, map[string]any) error {
	return nil
}

func (outputBackend *outputProducingBackend) Cancel(context.Context) error { return nil }

func TestSchedulerChecksGeneratedInputsOnlyAfterDependenciesComplete(t *testing.T) {
	ctx := context.Background()
	projectDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(projectDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	generatedPath := filepath.Join("results", "generated.txt")
	upstreamTask := &compiler.Task{
		ID:          "workflow/phase/generate",
		JobID:       "generate",
		Workflow:    "workflow",
		Phase:       "phase",
		Scope:       "global",
		Dimensions:  map[string]string{},
		Inputs:      map[string][]string{},
		Outputs:     map[string]string{"generated": generatedPath},
		Resources:   protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20},
		MaxAttempts: 1,
		Fingerprint: "generate",
	}
	downstreamTask := &compiler.Task{
		ID:           "workflow/phase/consume",
		JobID:        "consume",
		Workflow:     "workflow",
		Phase:        "phase",
		Scope:        "global",
		Dimensions:   map[string]string{},
		Inputs:       map[string][]string{"generated": {generatedPath}},
		Outputs:      map[string]string{},
		Resources:    protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20},
		Dependencies: []string{upstreamTask.ID},
		MaxAttempts:  1,
		Fingerprint:  "consume",
	}
	plan := &compiler.Plan{
		Workflow: "workflow",
		Phase:    "phase",
		Tasks:    []*compiler.Task{upstreamTask, downstreamTask},
		TaskByID: map[string]*compiler.Task{upstreamTask.ID: upstreamTask, downstreamTask.ID: downstreamTask},
		Order:    []string{upstreamTask.ID, downstreamTask.ID},
		Submissions: []compiler.SubmissionGroup{
			{ID: "generate", Scope: "global", TaskIDs: []string{upstreamTask.ID}, Resources: upstreamTask.Resources},
			{ID: "consume", Scope: "global", TaskIDs: []string{downstreamTask.ID}, Resources: downstreamTask.Resources},
		},
	}
	selectedBackend := &outputProducingBackend{}
	taskScheduler, err := New(plan, stateStore, Options{
		ProjectDirectory: projectDirectory,
		StateDirectory:   projectDirectory,
		ConfigPath:       filepath.Join(projectDirectory, "config.yaml"),
		WorkflowPath:     filepath.Join(projectDirectory, "workflow.yaml"),
		Backend:          selectedBackend,
		MaxParallel:      1,
		MaxCores:         1,
		MaxMemoryBytes:   64 << 20,
		RunID:            "generated-input-preflight-run",
	})
	if err != nil {
		t.Fatal(err)
	}

	runID, runErr := taskScheduler.Run(ctx)
	if runErr != nil {
		t.Fatalf("expected generated input to exist before downstream submission: %v", runErr)
	}
	if len(selectedBackend.requests) != 2 {
		t.Fatalf("expected upstream and downstream submissions, received %d", len(selectedBackend.requests))
	}
	if selectedBackend.requests[0].Manifests[0].TaskID != upstreamTask.ID || selectedBackend.requests[1].Manifests[0].TaskID != downstreamTask.ID {
		t.Fatalf("unexpected submission order: first=%q second=%q", selectedBackend.requests[0].Manifests[0].TaskID, selectedBackend.requests[1].Manifests[0].TaskID)
	}
	if _, err := os.Stat(filepath.Join(projectDirectory, generatedPath)); err != nil {
		t.Fatalf("expected upstream output to exist: %v", err)
	}

	_, counts, err := stateStore.RunSummary(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if counts["succeeded"] != 2 {
		t.Fatalf("unexpected task status counts: %#v", counts)
	}

	artifactRows, err := stateStore.QueryRows(ctx, `
		SELECT role, name, path, size_bytes, mtime_ns, validation_status
		FROM artifacts
		WHERE attempt_id IN (SELECT attempt_id FROM task_attempts WHERE run_id = ?)
		ORDER BY role, name
	`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer artifactRows.Close()
	persistedArtifacts := map[string]struct {
		path             string
		sizeBytes        int64
		modificationTime int64
		status           string
	}{}
	for artifactRows.Next() {
		var role, name, artifactPath, validationStatus string
		var sizeBytes, modificationTime int64
		if err := artifactRows.Scan(&role, &name, &artifactPath, &sizeBytes, &modificationTime, &validationStatus); err != nil {
			t.Fatal(err)
		}
		persistedArtifacts[role+":"+name] = struct {
			path             string
			sizeBytes        int64
			modificationTime int64
			status           string
		}{path: artifactPath, sizeBytes: sizeBytes, modificationTime: modificationTime, status: validationStatus}
	}
	if err := artifactRows.Err(); err != nil {
		t.Fatal(err)
	}
	generatedArtifact, exists := persistedArtifacts["output:generated"]
	if !exists || generatedArtifact.path != filepath.Join(projectDirectory, generatedPath) || generatedArtifact.sizeBytes <= 0 || generatedArtifact.modificationTime <= 0 || generatedArtifact.status != "valid" {
		t.Fatalf("unexpected persisted generated artifact: %#v", persistedArtifacts)
	}
	consumedArtifact, exists := persistedArtifacts["input:generated"]
	if !exists || consumedArtifact.path != filepath.Join(projectDirectory, generatedPath) || consumedArtifact.sizeBytes != generatedArtifact.sizeBytes || consumedArtifact.status != "valid" {
		t.Fatalf("unexpected persisted consumed artifact: %#v", persistedArtifacts)
	}

	metricRows, err := stateStore.QueryRows(ctx, `
		SELECT task_attempts.task_id, task_metrics.input_artifact_bytes, task_metrics.output_artifact_bytes
		FROM task_metrics
		JOIN task_attempts ON task_attempts.attempt_id = task_metrics.attempt_id
		WHERE task_attempts.run_id = ?
		ORDER BY task_attempts.task_id
	`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer metricRows.Close()
	artifactByteMetrics := map[string][2]int64{}
	for metricRows.Next() {
		var taskID string
		var inputArtifactBytes, outputArtifactBytes int64
		if err := metricRows.Scan(&taskID, &inputArtifactBytes, &outputArtifactBytes); err != nil {
			t.Fatal(err)
		}
		artifactByteMetrics[taskID] = [2]int64{inputArtifactBytes, outputArtifactBytes}
	}
	if err := metricRows.Err(); err != nil {
		t.Fatal(err)
	}
	if artifactByteMetrics[upstreamTask.ID] != [2]int64{0, generatedArtifact.sizeBytes} {
		t.Fatalf("unexpected upstream artifact byte metrics: %#v", artifactByteMetrics)
	}
	if artifactByteMetrics[downstreamTask.ID] != [2]int64{generatedArtifact.sizeBytes, 0} {
		t.Fatalf("unexpected downstream artifact byte metrics: %#v", artifactByteMetrics)
	}
}

func TestSchedulerCachesGeneratedInputChainAcrossFreshPlans(t *testing.T) {
	ctx := context.Background()
	projectDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(projectDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	generatedPath := filepath.Join("results", "generated.txt")
	selectedBackend := &outputProducingBackend{}
	runScheduler := func(runID string) map[string]int {
		t.Helper()
		upstreamTask := &compiler.Task{
			ID:          "workflow/phase/generate",
			JobID:       "generate",
			Workflow:    "workflow",
			Phase:       "phase",
			Scope:       "global",
			Dimensions:  map[string]string{},
			Inputs:      map[string][]string{},
			Outputs:     map[string]string{"generated": generatedPath},
			Resources:   protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20},
			MaxAttempts: 1,
			Fingerprint: "generate-definition",
		}
		downstreamTask := &compiler.Task{
			ID:           "workflow/phase/consume",
			JobID:        "consume",
			Workflow:     "workflow",
			Phase:        "phase",
			Scope:        "global",
			Dimensions:   map[string]string{},
			Inputs:       map[string][]string{"generated": {generatedPath}},
			Outputs:      map[string]string{"consumed": filepath.Join("results", "consumed.txt")},
			Resources:    protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20},
			Dependencies: []string{upstreamTask.ID},
			MaxAttempts:  1,
			Fingerprint:  "consume-definition",
		}
		plan := &compiler.Plan{
			Workflow: "workflow",
			Phase:    "phase",
			Tasks:    []*compiler.Task{upstreamTask, downstreamTask},
			TaskByID: map[string]*compiler.Task{upstreamTask.ID: upstreamTask, downstreamTask.ID: downstreamTask},
			Order:    []string{upstreamTask.ID, downstreamTask.ID},
			Submissions: []compiler.SubmissionGroup{
				{ID: "generate", Scope: "global", TaskIDs: []string{upstreamTask.ID}, Resources: upstreamTask.Resources},
				{ID: "consume", Scope: "global", TaskIDs: []string{downstreamTask.ID}, Resources: downstreamTask.Resources},
			},
		}
		taskScheduler, schedulerErr := New(plan, stateStore, Options{
			ProjectDirectory: projectDirectory,
			StateDirectory:   projectDirectory,
			ConfigPath:       filepath.Join(projectDirectory, "config.yaml"),
			WorkflowPath:     filepath.Join(projectDirectory, "workflow.yaml"),
			Backend:          selectedBackend,
			MaxParallel:      1,
			MaxCores:         1,
			MaxMemoryBytes:   64 << 20,
			RunID:            runID,
		})
		if schedulerErr != nil {
			t.Fatal(schedulerErr)
		}
		actualRunID, runErr := taskScheduler.Run(ctx)
		if runErr != nil {
			t.Fatalf("run %s: %v", runID, runErr)
		}
		_, counts, summaryErr := stateStore.RunSummary(ctx, actualRunID)
		if summaryErr != nil {
			t.Fatal(summaryErr)
		}
		return counts
	}

	firstCounts := runScheduler("generated-chain-first")
	if firstCounts["succeeded"] != 2 || len(selectedBackend.requests) != 2 {
		t.Fatalf("unexpected first run: counts=%#v submissions=%d", firstCounts, len(selectedBackend.requests))
	}
	assertCacheDecision(t, stateStore, "generated-chain-first", "workflow/phase/generate", "miss", "no_prior_success")
	assertCacheDecision(t, stateStore, "generated-chain-first", "workflow/phase/consume", "miss", "dependency_not_cached")

	secondCounts := runScheduler("generated-chain-second")
	if secondCounts["cached"] != 2 || len(selectedBackend.requests) != 2 {
		t.Fatalf("expected generated input chain to use cache: counts=%#v submissions=%d", secondCounts, len(selectedBackend.requests))
	}
	assertCacheDecision(t, stateStore, "generated-chain-second", "workflow/phase/generate", "hit", "")
	assertCacheDecision(t, stateStore, "generated-chain-second", "workflow/phase/consume", "hit", "")
}

func TestSchedulerInvalidatesCacheWhenOutputMetadataChanges(t *testing.T) {
	ctx := context.Background()
	projectDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(projectDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	outputPath := filepath.Join("results", "cached.txt")
	definitionFingerprint := "cacheable-definition"
	selectedBackend := &outputProducingBackend{}
	runScheduler := func(runID string, forceCacheBypass ...bool) map[string]int {
		t.Helper()
		task := &compiler.Task{
			ID:          "workflow/phase/cacheable",
			JobID:       "cacheable",
			Workflow:    "workflow",
			Phase:       "phase",
			Scope:       "global",
			Dimensions:  map[string]string{},
			Inputs:      map[string][]string{},
			Outputs:     map[string]string{"result": outputPath},
			Resources:   protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20},
			MaxAttempts: 1,
			Fingerprint: definitionFingerprint,
		}
		plan := &compiler.Plan{
			Workflow:    "workflow",
			Phase:       "phase",
			Tasks:       []*compiler.Task{task},
			TaskByID:    map[string]*compiler.Task{task.ID: task},
			Order:       []string{task.ID},
			Submissions: []compiler.SubmissionGroup{{ID: "cacheable", Scope: "global", TaskIDs: []string{task.ID}, Resources: task.Resources}},
		}
		force := len(forceCacheBypass) > 0 && forceCacheBypass[0]
		taskScheduler, schedulerErr := New(plan, stateStore, Options{
			ProjectDirectory: projectDirectory,
			StateDirectory:   projectDirectory,
			ConfigPath:       filepath.Join(projectDirectory, "config.yaml"),
			WorkflowPath:     filepath.Join(projectDirectory, "workflow.yaml"),
			Backend:          selectedBackend,
			MaxParallel:      1,
			MaxCores:         1,
			MaxMemoryBytes:   64 << 20,
			Force:            force,
			RunID:            runID,
		})
		if schedulerErr != nil {
			t.Fatal(schedulerErr)
		}
		actualRunID, runErr := taskScheduler.Run(ctx)
		if runErr != nil {
			t.Fatalf("run %s: %v", runID, runErr)
		}
		_, counts, summaryErr := stateStore.RunSummary(ctx, actualRunID)
		if summaryErr != nil {
			t.Fatal(summaryErr)
		}
		return counts
	}

	firstCounts := runScheduler("artifact-cache-first")
	if firstCounts["succeeded"] != 1 || len(selectedBackend.requests) != 1 {
		t.Fatalf("unexpected first run: counts=%#v submissions=%d", firstCounts, len(selectedBackend.requests))
	}
	assertCacheDecision(t, stateStore, "artifact-cache-first", "workflow/phase/cacheable", "miss", "no_prior_success")

	secondCounts := runScheduler("artifact-cache-second")
	if secondCounts["cached"] != 1 || len(selectedBackend.requests) != 1 {
		t.Fatalf("expected second run to use cache: counts=%#v submissions=%d", secondCounts, len(selectedBackend.requests))
	}
	assertCacheDecision(t, stateStore, "artifact-cache-second", "workflow/phase/cacheable", "hit", "")

	absoluteOutputPath := filepath.Join(projectDirectory, outputPath)
	if err := os.WriteFile(absoluteOutputPath, []byte("externally modified output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	modifiedTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(absoluteOutputPath, modifiedTime, modifiedTime); err != nil {
		t.Fatal(err)
	}
	thirdCounts := runScheduler("artifact-cache-third")
	if thirdCounts["succeeded"] != 1 || len(selectedBackend.requests) != 2 {
		t.Fatalf("expected modified output to invalidate cache: counts=%#v submissions=%d", thirdCounts, len(selectedBackend.requests))
	}
	assertCacheDecision(t, stateStore, "artifact-cache-third", "workflow/phase/cacheable", "miss", "output_size_changed")

	forcedCounts := runScheduler("artifact-cache-forced", true)
	if forcedCounts["succeeded"] != 1 || len(selectedBackend.requests) != 3 {
		t.Fatalf("expected forced run to bypass cache: counts=%#v submissions=%d", forcedCounts, len(selectedBackend.requests))
	}
	assertCacheDecision(t, stateStore, "artifact-cache-forced", "workflow/phase/cacheable", "bypass", "forced")

	definitionFingerprint = "cacheable-definition-v2"
	changedDefinitionCounts := runScheduler("artifact-cache-definition-changed")
	if changedDefinitionCounts["succeeded"] != 1 || len(selectedBackend.requests) != 4 {
		t.Fatalf("expected changed definition to invalidate cache: counts=%#v submissions=%d", changedDefinitionCounts, len(selectedBackend.requests))
	}
	assertCacheDecision(t, stateStore, "artifact-cache-definition-changed", "workflow/phase/cacheable", "miss", "definition_changed")
}

func TestSchedulerInvalidatesCacheWhenInputMetadataChanges(t *testing.T) {
	ctx := context.Background()
	projectDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(projectDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	inputPath := filepath.Join("inputs", "source.txt")
	absoluteInputPath := filepath.Join(projectDirectory, inputPath)
	if err := os.MkdirAll(filepath.Dir(absoluteInputPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absoluteInputPath, []byte("initial input\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	selectedBackend := &outputProducingBackend{}
	runScheduler := func(runID string) map[string]int {
		t.Helper()
		task := &compiler.Task{
			ID:          "workflow/phase/input-cacheable",
			JobID:       "input-cacheable",
			Workflow:    "workflow",
			Phase:       "phase",
			Scope:       "global",
			Dimensions:  map[string]string{},
			Inputs:      map[string][]string{"source": {inputPath}},
			Outputs:     map[string]string{"result": filepath.Join("results", "input-cached.txt")},
			Resources:   protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20},
			MaxAttempts: 1,
			Fingerprint: "input-cacheable-definition",
		}
		plan := &compiler.Plan{
			Workflow:    "workflow",
			Phase:       "phase",
			Tasks:       []*compiler.Task{task},
			TaskByID:    map[string]*compiler.Task{task.ID: task},
			Order:       []string{task.ID},
			Submissions: []compiler.SubmissionGroup{{ID: "input-cacheable", Scope: "global", TaskIDs: []string{task.ID}, Resources: task.Resources}},
		}
		taskScheduler, schedulerErr := New(plan, stateStore, Options{
			ProjectDirectory: projectDirectory,
			StateDirectory:   projectDirectory,
			ConfigPath:       filepath.Join(projectDirectory, "config.yaml"),
			WorkflowPath:     filepath.Join(projectDirectory, "workflow.yaml"),
			Backend:          selectedBackend,
			MaxParallel:      1,
			MaxCores:         1,
			MaxMemoryBytes:   64 << 20,
			RunID:            runID,
		})
		if schedulerErr != nil {
			t.Fatal(schedulerErr)
		}
		actualRunID, runErr := taskScheduler.Run(ctx)
		if runErr != nil {
			t.Fatalf("run %s: %v", runID, runErr)
		}
		_, counts, summaryErr := stateStore.RunSummary(ctx, actualRunID)
		if summaryErr != nil {
			t.Fatal(summaryErr)
		}
		return counts
	}

	firstCounts := runScheduler("input-cache-first")
	if firstCounts["succeeded"] != 1 || len(selectedBackend.requests) != 1 {
		t.Fatalf("unexpected first run: counts=%#v submissions=%d", firstCounts, len(selectedBackend.requests))
	}
	secondCounts := runScheduler("input-cache-second")
	if secondCounts["cached"] != 1 || len(selectedBackend.requests) != 1 {
		t.Fatalf("expected unchanged input to use cache: counts=%#v submissions=%d", secondCounts, len(selectedBackend.requests))
	}

	if err := os.WriteFile(absoluteInputPath, []byte("modified external input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	modifiedTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(absoluteInputPath, modifiedTime, modifiedTime); err != nil {
		t.Fatal(err)
	}
	thirdCounts := runScheduler("input-cache-third")
	if thirdCounts["succeeded"] != 1 || len(selectedBackend.requests) != 2 {
		t.Fatalf("expected modified input to invalidate cache: counts=%#v submissions=%d", thirdCounts, len(selectedBackend.requests))
	}
}

func assertCacheDecision(
	t *testing.T,
	stateStore *store.Store,
	runID string,
	taskID string,
	expectedDecision string,
	expectedReasonCode string,
) {
	t.Helper()
	rows, err := stateStore.QueryRows(t.Context(), `
		SELECT cache_decision, COALESCE(cache_reason_code, '')
		FROM task_instances
		WHERE run_id = ? AND task_id = ?
	`, runID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("cache decision for task %s in run %s was not persisted", taskID, runID)
	}
	var actualDecision string
	var actualReasonCode string
	if err := rows.Scan(&actualDecision, &actualReasonCode); err != nil {
		t.Fatal(err)
	}
	if actualDecision != expectedDecision || actualReasonCode != expectedReasonCode {
		t.Fatalf(
			"unexpected cache decision for task %s in run %s: decision=%q reason=%q",
			taskID,
			runID,
			actualDecision,
			actualReasonCode,
		)
	}
}

func TestSchedulerWritesControllerEventsToJSONLAndStateDatabase(t *testing.T) {
	ctx := context.Background()
	stateDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(stateDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	task := &compiler.Task{
		ID:          "workflow/phase/task-a",
		JobID:       "task-a",
		Workflow:    "workflow",
		Phase:       "phase",
		Scope:       "global",
		Dimensions:  map[string]string{},
		Inputs:      map[string][]string{},
		Outputs:     map[string]string{},
		Resources:   protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20},
		MaxAttempts: 1,
		Fingerprint: "task-a-fingerprint",
	}
	plan := &compiler.Plan{
		Workflow: "workflow",
		Phase:    "phase",
		Tasks:    []*compiler.Task{task},
		TaskByID: map[string]*compiler.Task{task.ID: task},
		Order:    []string{task.ID},
		Submissions: []compiler.SubmissionGroup{{
			ID:        "global-task-a",
			Scope:     "global",
			TaskIDs:   []string{task.ID},
			Resources: task.Resources,
		}},
	}
	taskScheduler, err := New(plan, stateStore, Options{
		ProjectDirectory: stateDirectory,
		StateDirectory:   stateDirectory,
		ConfigPath:       filepath.Join(stateDirectory, "config.yaml"),
		WorkflowPath:     filepath.Join(stateDirectory, "workflow.yaml"),
		Backend:          &recordingBackend{},
		MaxParallel:      1,
		MaxCores:         1,
		MaxMemoryBytes:   64 << 20,
		RunID:            "controller-log-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taskScheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(taskScheduler.ControllerLogErrors()) != 0 {
		t.Fatalf("unexpected controller log errors: %v", taskScheduler.ControllerLogErrors())
	}

	logFile, err := os.Open(taskScheduler.ControllerLogPath())
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	requiredEvents := map[string]bool{
		"run.started":          false,
		"task.cache_evaluated": false,
		"submission.created":   false,
		"attempt.started":      false,
		"attempt.finished":     false,
		"submission.finished":  false,
		"run.finished":         false,
	}
	fileEventCount := 0
	scanner := bufio.NewScanner(logFile)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode controller log line %d: %v", fileEventCount+1, err)
		}
		eventName, _ := event["event"].(string)
		if _, required := requiredEvents[eventName]; required {
			requiredEvents[eventName] = true
		}
		if event["run_id"] != "controller-log-run" {
			t.Fatalf("event %q has unexpected run context %#v", eventName, event["run_id"])
		}
		fileEventCount++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for eventName, found := range requiredEvents {
		if !found {
			t.Errorf("controller log did not contain event %q", eventName)
		}
	}

	eventRows, err := stateStore.QueryRows(ctx, `SELECT COUNT(*) FROM events WHERE run_id = ?`, "controller-log-run")
	if err != nil {
		t.Fatal(err)
	}
	defer eventRows.Close()
	if !eventRows.Next() {
		t.Fatal("controller event count query returned no row")
	}
	var persistedEventCount int
	if err := eventRows.Scan(&persistedEventCount); err != nil {
		t.Fatal(err)
	}
	if persistedEventCount != fileEventCount {
		t.Fatalf("controller event sinks diverged: file=%d database=%d", fileEventCount, persistedEventCount)
	}
}

func TestSubmissionCapacityErrorRejectsOnlyExplicitlyOversizedRequests(t *testing.T) {
	submission := compiler.SubmissionGroup{
		ID:        "submission-large",
		Resources: protocol.ResourceRequest{Cores: 8, MemoryByte: 512 << 20},
	}
	if capacityErr := submissionCapacityError(submission, 4, 0); capacityErr == nil || !strings.Contains(capacityErr.Error(), "requires 8 cores") {
		t.Fatalf("expected CPU capacity error, got %v", capacityErr)
	}
	if capacityErr := submissionCapacityError(submission, 0, 256<<20); capacityErr == nil || !strings.Contains(capacityErr.Error(), "requires 536870912 bytes") {
		t.Fatalf("expected memory capacity error, got %v", capacityErr)
	}
	if capacityErr := submissionCapacityError(submission, 0, 0); capacityErr != nil {
		t.Fatalf("unlimited admission should accept submission, got %v", capacityErr)
	}
}

func batchTask(taskID, sampleID string, workerPlan *compiler.WorkerPlan) *compiler.Task {
	return &compiler.Task{
		ID:          taskID,
		JobID:       "align",
		Workflow:    "workflow",
		Phase:       "phase",
		Scope:       "batch",
		Dimensions:  map[string]string{"sample": sampleID, "species": "human"},
		Inputs:      map[string][]string{},
		Outputs:     map[string]string{},
		Resources:   protocol.ResourceRequest{Cores: 2, MemoryByte: 128 << 20},
		Worker:      workerPlan,
		MaxAttempts: 1,
		Fingerprint: taskID,
	}
}
