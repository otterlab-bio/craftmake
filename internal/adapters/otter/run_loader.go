package otter

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
	"gopkg.in/yaml.v3"
)

const defaultAdapterValue = "NO_ADAPTER_CAL_USE_DEFAULT"

var (
	runIDPattern     = regexp.MustCompile("^run-[0-9]{8}T[0-9]{6}Z-[a-z]{6}\\z")
	digestPattern    = regexp.MustCompile("^sha256:[a-f0-9]{64}\\z")
	slurmTimePattern = regexp.MustCompile("^(?:[0-9]+-)?[0-9]{1,2}:[0-5][0-9]:[0-5][0-9]\\z")
)

func Load(path string) (*compiler.Context, error) {
	data, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read otter run snapshot %q: %w", path, err)
	}
	defer data.Close()

	var snapshot runSnapshot
	decoder := yaml.NewDecoder(data)
	decoder.KnownFields(true)
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("parse otter run snapshot %q: %w", path, err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse otter run snapshot %q: multiple YAML documents are not supported", path)
		}
		return nil, fmt.Errorf("parse otter run snapshot %q: %w", path, err)
	}
	if err := validateRunSnapshot(&snapshot); err != nil {
		return nil, fmt.Errorf("validate otter run snapshot %q: %w", path, err)
	}

	rawDirectory := inferRawDirectory(snapshot.Samples)
	workflowName, mode, pdxMode, err := workflowIdentity(snapshot.Workflow.Scenario)
	if err != nil {
		return nil, err
	}
	primaryReference, graftReference, hostReference := selectReferences(snapshot.References.Resolved, pdxMode)
	if primaryReference == nil || graftReference == nil {
		return nil, fmt.Errorf("run snapshot does not resolve a primary reference")
	}
	if pdxMode && hostReference == nil {
		return nil, fmt.Errorf("run snapshot scenario %q requires graft and host references", snapshot.Workflow.Scenario)
	}

	samples := make([]compiler.SampleContext, 0, len(snapshot.Samples))
	for sampleIndex, sample := range snapshot.Samples {
		samples = append(samples, compiler.SampleContext{
			ID:       sample.ID,
			Index:    sampleIndex,
			Read1:    sample.R1,
			Read2:    sample.R2,
			Adapter1: adapterOrDefault(sample.AdapterR1),
			Adapter2: adapterOrDefault(sample.AdapterR2),
		})
	}

	species := make([]compiler.SpeciesContext, 0, len(snapshot.References.Resolved))
	for speciesIndex, reference := range snapshot.References.Resolved {
		species = append(species, compiler.SpeciesContext{
			Name:            reference.ID,
			Index:           speciesIndex,
			Role:            reference.Role,
			GenomeFasta:     reference.Fasta.Path,
			GenomeIndex:     selectIndexPath(reference, mode),
			RNASeqGTF:       selectAnnotationPath(reference, "gtf"),
			RNASeqReference: selectRNAReferencePath(reference),
		})
	}

	expressionReference := primaryReference
	if pdxMode {
		expressionReference = graftReference
	}
	expressionSpecies, err := seq2matSpeciesForReference(*expressionReference)
	if err != nil {
		return nil, err
	}

	raw := buildRunCompatibilityConfig(snapshot, rawDirectory, mode, pdxMode, primaryReference, graftReference, hostReference, expressionSpecies)
	phaseResources, err := phaseResourceRequests(snapshot.Execution.Resources)
	if err != nil {
		return nil, err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve run snapshot path: %w", err)
	}
	return &compiler.Context{
		Raw: raw,
		Workflow: compiler.WorkflowContext{
			Mode:             mode,
			WorkflowName:     workflowName,
			JobID:            snapshot.Run.ID,
			UserID:           snapshot.Project.ID,
			Executor:         snapshot.Execution.Executor.Value,
			Backend:          snapshot.Execution.Backend.Value,
			Toolchain:        snapshot.Workflow.Toolchain,
			LegacyExtensions: append([]string(nil), snapshot.Workflow.LegacyExtensions...),
			PDXMode:          pdxMode,
		},
		Execution: compiler.ExecutionContext{
			PhaseResources: phaseResources,
			Slurm: compiler.SlurmExecutionContext{
				Partition:   snapshot.Execution.Slurm.Partition.Value,
				Account:     snapshot.Execution.Slurm.Account.Value,
				QOS:         snapshot.Execution.Slurm.QOS.Value,
				MaxJobs:     snapshot.Execution.Slurm.MaxJobs.Value,
				DefaultTime: snapshot.Execution.Slurm.DefaultTime.Value,
				ScratchRoot: snapshot.Execution.Slurm.ScratchRoot.Value,
			},
		},
		Samples: samples,
		Species: species,
		Paths: map[string]string{
			"config":   absolutePath,
			"project":  snapshot.Project.Root,
			"run_root": snapshot.Paths.RunRoot,
			"work":     snapshot.Paths.Work,
			"results":  snapshot.Paths.Results,
			"logs":     snapshot.Paths.Logs,
			"state":    snapshot.Paths.State,
			"metrics":  snapshot.Paths.Metrics,
		},
	}, nil
}

func phaseResourceRequests(resources projectResources) (map[string]protocol.ResourceRequest, error) {
	requests := make(map[string]protocol.ResourceRequest, len(resources.Phases))
	for phaseName, phaseResource := range resources.Phases {
		memoryBytes, err := compiler.ParseMemory(phaseResource.Memory)
		if err != nil {
			return nil, fmt.Errorf("execution.resources.phases.%s.memory: %w", phaseName, err)
		}
		requests[phaseName] = protocol.ResourceRequest{
			Cores:      phaseResource.Cores,
			MemoryByte: memoryBytes,
			Partition:  phaseResource.Partition,
			Time:       phaseResource.Time,
		}
	}
	return requests, nil
}

func validateRunSnapshot(snapshot *runSnapshot) error {
	if snapshot.SchemaVersion != runSchemaVersion {
		return fmt.Errorf("schema_version must be %q, got %q", runSchemaVersion, snapshot.SchemaVersion)
	}
	if snapshot.Run.ID == "" || runIDPattern.FindString(snapshot.Run.ID) != snapshot.Run.ID {
		return fmt.Errorf("run.id %q is invalid", snapshot.Run.ID)
	}
	if _, err := time.Parse(time.RFC3339, snapshot.Run.CreatedAt); err != nil {
		return fmt.Errorf("run.created_at must be RFC3339: %w", err)
	}
	if snapshot.Run.ParentRunID != "" && runIDPattern.FindString(snapshot.Run.ParentRunID) != snapshot.Run.ParentRunID {
		return fmt.Errorf("run.parent_run_id %q is invalid", snapshot.Run.ParentRunID)
	}
	if !snapshot.Run.Immutable {
		return fmt.Errorf("run.immutable must be true")
	}
	if strings.TrimSpace(snapshot.Project.ID) == "" || !filepath.IsAbs(snapshot.Project.Root) {
		return fmt.Errorf("project.id and absolute project.root are required")
	}
	if !filepath.IsAbs(snapshot.Workflow.AssetRoot) {
		return fmt.Errorf("workflow.asset_root must be absolute")
	}
	if snapshot.Workflow.Toolchain != "modern" && snapshot.Workflow.Toolchain != "legacy-equivalent" {
		return fmt.Errorf("workflow.toolchain %q is invalid", snapshot.Workflow.Toolchain)
	}
	if err := validateLegacyExtensions(snapshot.Workflow.LegacyExtensions); err != nil {
		return err
	}
	if strings.TrimSpace(snapshot.Execution.Executor.Value) != "craftmake" {
		return fmt.Errorf("execution.executor.value must be craftmake, got %q", snapshot.Execution.Executor.Value)
	}
	if snapshot.Execution.Backend.Value != "local" && snapshot.Execution.Backend.Value != "slurm" {
		return fmt.Errorf("execution.backend.value must be local or slurm, got %q", snapshot.Execution.Backend.Value)
	}
	if snapshot.Execution.Backend.Value == "slurm" {
		if err := validateResolvedSlurmResources(snapshot.Execution.Slurm); err != nil {
			return fmt.Errorf("execution.slurm: %w", err)
		}
	}
	for name, path := range map[string]string{
		"run_root": snapshot.Paths.RunRoot,
		"work":     snapshot.Paths.Work,
		"results":  snapshot.Paths.Results,
		"logs":     snapshot.Paths.Logs,
		"state":    snapshot.Paths.State,
		"metrics":  snapshot.Paths.Metrics,
	} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("paths.%s must be absolute", name)
		}
	}
	for name, digest := range map[string]string{
		"project":         snapshot.Digests.Project,
		"samples":         snapshot.Digests.Samples,
		"workflow_assets": snapshot.Digests.WorkflowAssets,
	} {
		if digest == "" || digestPattern.FindString(digest) != digest {
			return fmt.Errorf("digests.%s is invalid", name)
		}
	}
	if len(snapshot.Samples) == 0 {
		return fmt.Errorf("samples must not be empty")
	}
	seenSampleIDs := make(map[string]bool, len(snapshot.Samples))
	for sampleIndex, sample := range snapshot.Samples {
		if strings.TrimSpace(sample.ID) == "" {
			return fmt.Errorf("samples[%d].id is required", sampleIndex)
		}
		if seenSampleIDs[sample.ID] {
			return fmt.Errorf("samples[%d].id %q is duplicated", sampleIndex, sample.ID)
		}
		seenSampleIDs[sample.ID] = true
		if strings.TrimSpace(sample.R1) == "" || strings.TrimSpace(sample.R2) == "" {
			return fmt.Errorf("samples[%d] requires non-empty r1 and r2", sampleIndex)
		}
		if !filepath.IsAbs(sample.R1) || !filepath.IsAbs(sample.R2) {
			return fmt.Errorf("samples[%d].r1 and r2 must be absolute paths", sampleIndex)
		}
	}
	if len(snapshot.References.Resolved) == 0 {
		return fmt.Errorf("references.resolved must not be empty")
	}
	for referenceIndex, reference := range snapshot.References.Resolved {
		if strings.TrimSpace(reference.ID) == "" || strings.TrimSpace(reference.Role) == "" {
			return fmt.Errorf("references.resolved[%d] requires id and role", referenceIndex)
		}
		if strings.TrimSpace(reference.Fasta.Path) == "" {
			return fmt.Errorf("references.resolved[%d].fasta.path is required", referenceIndex)
		}
	}
	_, _, _, err := workflowIdentity(snapshot.Workflow.Scenario)
	return err
}

func validateLegacyExtensions(extensions []string) error {
	allowedExtensions := map[string]bool{
		"clubcpg":       true,
		"mhap":          true,
		"ccgg":          true,
		"insert-length": true,
	}
	seenExtensions := make(map[string]bool, len(extensions))
	for _, extension := range extensions {
		if !allowedExtensions[extension] {
			return fmt.Errorf("workflow legacy extension %q is invalid", extension)
		}
		if seenExtensions[extension] {
			return fmt.Errorf("workflow legacy extension %q is duplicated", extension)
		}
		seenExtensions[extension] = true
	}
	return nil
}

func validateResolvedSlurmResources(resources resolvedSlurmResources) error {
	if strings.TrimSpace(resources.Partition.Value) == "" || !isSnapshotValueSource(resources.Partition.Source) {
		return fmt.Errorf("partition value and source are required")
	}
	if strings.TrimSpace(resources.Account.Value) == "" || !isSnapshotValueSource(resources.Account.Source) {
		return fmt.Errorf("account value and source are required")
	}
	if resources.QOS.Value != "" && !isSnapshotValueSource(resources.QOS.Source) {
		return fmt.Errorf("qos source is invalid")
	}
	if resources.MaxJobs.Value < 0 || !isSnapshotValueSource(resources.MaxJobs.Source) {
		return fmt.Errorf("max_jobs value/source is invalid")
	}
	if resources.DefaultTime.Value != "" && !slurmTimePattern.MatchString(resources.DefaultTime.Value) {
		return fmt.Errorf("default_time %q is invalid", resources.DefaultTime.Value)
	}
	if resources.DefaultTime.Value != "" && !isSnapshotValueSource(resources.DefaultTime.Source) {
		return fmt.Errorf("default_time source is invalid")
	}
	if resources.ScratchRoot.Value != "" && (!filepath.IsAbs(resources.ScratchRoot.Value) || !isSnapshotValueSource(resources.ScratchRoot.Source)) {
		return fmt.Errorf("scratch_root must be absolute and have a valid source")
	}
	return nil
}

func isSnapshotValueSource(source string) bool {
	switch source {
	case "default", "project", "cli", "profile", "detection":
		return true
	default:
		return false
	}
}

func workflowIdentity(scenario string) (string, string, bool, error) {
	switch scenario {
	case "rrbs":
		return "BeaverBS", "RRBS", false, nil
	case "wgbs":
		return "BeaverBS", "WGBS", false, nil
	case "rnaseq":
		return "BeaverRNA", "RNASEQ", false, nil
	case "bs-pdx":
		return "BeaverPDX", "RRBS", true, nil
	case "rna-pdx":
		return "BeaverRNASEQPDX", "RNASEQ", true, nil
	default:
		return "", "", false, fmt.Errorf("workflow.scenario %q is not supported", scenario)
	}
}

func inferRawDirectory(samples []sampleRecord) string {
	if len(samples) == 0 {
		return ""
	}
	return filepath.Clean(filepath.Dir(samples[0].R1))
}

func selectReferences(references []resolvedReference, pdxMode bool) (*resolvedReference, *resolvedReference, *resolvedReference) {
	var primaryReference *resolvedReference
	var graftReference *resolvedReference
	var hostReference *resolvedReference
	for referenceIndex := range references {
		reference := &references[referenceIndex]
		switch reference.Role {
		case "primary":
			if primaryReference == nil {
				primaryReference = reference
			}
		case "graft":
			if graftReference == nil {
				graftReference = reference
			}
		case "host":
			if hostReference == nil {
				hostReference = reference
			}
		case "secondary":
			if hostReference == nil {
				hostReference = reference
			}
		}
	}
	if primaryReference == nil && len(references) > 0 {
		primaryReference = &references[0]
	}
	if graftReference == nil {
		graftReference = primaryReference
	}
	if pdxMode && hostReference == nil && len(references) > 1 {
		hostReference = &references[1]
	}
	return primaryReference, graftReference, hostReference
}

func adapterOrDefault(adapter string) string {
	if strings.TrimSpace(adapter) == "" {
		return defaultAdapterValue
	}
	return adapter
}

func selectIndexPath(reference resolvedReference, mode string) string {
	if mode == "RNASEQ" {
		if path := selectIndexByType(reference.Indexes, "star"); path != "" {
			return path
		}
	} else if path := selectIndexByType(reference.Indexes, "bismark"); path != "" {
		return bismarkExecutionGenomeDirectory(path)
	}
	if len(reference.Indexes) > 0 {
		return reference.Indexes[0].Path
	}
	return ""
}

func bismarkExecutionGenomeDirectory(indexRoot string) string {
	if strings.TrimSpace(indexRoot) == "" {
		return ""
	}
	return filepath.Join(indexRoot, "genome")
}

func selectIndexByType(indexes []resolvedAsset, indexType string) string {
	for _, index := range indexes {
		if strings.EqualFold(index.Type, indexType) {
			return index.Path
		}
	}
	return ""
}

func selectRNAReferencePath(reference resolvedReference) string {
	if path := selectIndexByType(reference.Indexes, "star"); path != "" {
		return path
	}
	if len(reference.Indexes) > 0 {
		return reference.Indexes[0].Path
	}
	return ""
}

func selectAnnotationPath(reference resolvedReference, annotationType string) string {
	for _, annotation := range reference.Annotations {
		if strings.EqualFold(annotation.Type, annotationType) {
			return annotation.Path
		}
	}
	return ""
}

func seq2matSpeciesForReference(reference resolvedReference) (string, error) {
	normalizedOrganism := strings.ToLower(strings.TrimSpace(reference.Organism))
	switch normalizedOrganism {
	case "human", "homo sapiens":
		return "human", nil
	case "mouse", "mus musculus":
		return "mouse", nil
	default:
		return "", fmt.Errorf(
			"reference %q has unsupported organism %q for seq2mat",
			reference.ID,
			reference.Organism,
		)
	}
}

func buildRunCompatibilityConfig(
	snapshot runSnapshot,
	rawDirectory string,
	mode string,
	pdxMode bool,
	primaryReference *resolvedReference,
	graftReference *resolvedReference,
	hostReference *resolvedReference,
	expressionSpecies string,
) map[string]any {
	workflowDirectory := snapshot.Paths.Work
	qualityControlDirectory := filepath.Join(workflowDirectory, "QC")
	methylationDirectory := filepath.Join(workflowDirectory, "mCall")
	if mode == "RNASEQ" {
		methylationDirectory = filepath.Join(workflowDirectory, "expression")
	}
	primaryName := primaryReference.ID
	graftName := graftReference.ID
	hostName := ""
	if hostReference != nil {
		hostName = hostReference.ID
	}

	raw := map[string]any{
		"schema_version": snapshot.SchemaVersion,
		"run": map[string]any{
			"id":         snapshot.Run.ID,
			"created_at": snapshot.Run.CreatedAt,
			"immutable":  snapshot.Run.Immutable,
		},
		"project": map[string]any{
			"id":   snapshot.Project.ID,
			"root": snapshot.Project.Root,
		},
		"input": map[string]any{
			"fastq_dir": rawDirectory,
		},
		"output": map[string]any{
			"raw_dir":      rawDirectory,
			"trim_dir":     filepath.Join(workflowDirectory, "trim"),
			"workflow_dir": workflowDirectory,
			"analysis_dir": snapshot.Paths.Results,
			"log_dir":      snapshot.Paths.Logs,
		},
		"directories": map[string]any{
			"work":             workflowDirectory,
			"selfconfig":       snapshot.Project.Root,
			"qctb_config":      filepath.Join(snapshot.Paths.State, "config.yaml"),
			"sid_log":          snapshot.Paths.Logs,
			"methylation_call": methylationDirectory,
			"qualimap":         filepath.Join(qualityControlDirectory, "qualimap"),
			"qc_summary":       filepath.Join(snapshot.Paths.Results, "qc"),
			"beta_matrix":      filepath.Join(snapshot.Paths.Results, "methylation"),
			"qc": map[string]any{
				"main":   qualityControlDirectory,
				"before": filepath.Join(workflowDirectory, "fastqc_raw"),
				"after":  filepath.Join(workflowDirectory, "fastqc_clean"),
			},
			"bsmap": map[string]any{
				"main": filepath.Join(workflowDirectory, "bsmap"),
			},
		},
		"workflow": map[string]any{
			"scenario":  snapshot.Workflow.Scenario,
			"toolchain": snapshot.Workflow.Toolchain,
			"jobid":     snapshot.Run.ID,
			"userid":    snapshot.Project.ID,
			"mode":      mode,
			"species": map[string]any{
				"name":       speciesNames(snapshot.References.Resolved),
				"primary":    primaryName,
				"secondary":  hostName,
				"graft":      graftName,
				"host":       hostName,
				"expression": expressionSpecies,
			},
			"adapters": map[string]any{
				"error": float64(0.2),
				"seq1":  sampleAdapters(snapshot.Samples, true),
				"seq2":  sampleAdapters(snapshot.Samples, false),
			},
			"alignment": map[string]any{
				"c1": 0,
				"c2": 0,
				"t1": 0,
				"t2": 0,
			},
		},
		"reference": buildReferenceConfig(mode, primaryReference, graftReference, hostReference),
		"metadata": map[string]any{
			"sids":         sampleIDs(snapshot.Samples),
			"group_levels": sampleGroupLevelCount(snapshot.Samples),
		},
		"execution": map[string]any{
			"executor": snapshot.Execution.Executor.Value,
			"backend":  snapshot.Execution.Backend.Value,
		},
	}
	if pdxMode {
		raw["metadata"].(map[string]any)["pdx_pipeline"] = true
	}
	return raw
}

func buildReferenceConfig(mode string, primaryReference, graftReference, hostReference *resolvedReference) map[string]any {
	reference := map[string]any{
		"graft_fasta":  graftReference.Fasta.Path,
		"graft_index":  selectIndexPath(*graftReference, mode),
		"genome_fasta": []string{primaryReference.Fasta.Path},
		"genome_index": []string{selectIndexPath(*primaryReference, mode)},
	}
	rnaseq := map[string]any{
		"primary_gtf":       selectAnnotationPath(*primaryReference, "gtf"),
		"primary_reference": selectRNAReferencePath(*primaryReference),
		"graft_gtf":         selectAnnotationPath(*graftReference, "gtf"),
		"graft_reference":   selectRNAReferencePath(*graftReference),
	}
	reference["rnaseq"] = rnaseq
	if hostReference != nil {
		reference["host_fasta"] = hostReference.Fasta.Path
		reference["host_index"] = selectIndexPath(*hostReference, mode)
		rnaseq["host_gtf"] = selectAnnotationPath(*hostReference, "gtf")
		rnaseq["host_reference"] = selectRNAReferencePath(*hostReference)
	}
	return reference
}

func speciesNames(references []resolvedReference) []string {
	names := make([]string, 0, len(references))
	for _, reference := range references {
		names = append(names, reference.ID)
	}
	return names
}

func sampleIDs(samples []sampleRecord) []string {
	ids := make([]string, 0, len(samples))
	for _, sample := range samples {
		ids = append(ids, sample.ID)
	}
	return ids
}

func sampleGroupLevelCount(samples []sampleRecord) int {
	uniqueGroups := make(map[string]struct{}, len(samples))
	for _, sample := range samples {
		groupName := strings.TrimSpace(sample.Group)
		if groupName == "" {
			continue
		}
		uniqueGroups[groupName] = struct{}{}
	}
	return len(uniqueGroups)
}

func sampleAdapters(samples []sampleRecord, readOne bool) []string {
	adapters := make([]string, 0, len(samples))
	for _, sample := range samples {
		adapter := sample.AdapterR2
		if readOne {
			adapter = sample.AdapterR1
		}
		adapters = append(adapters, adapterOrDefault(adapter))
	}
	return adapters
}
