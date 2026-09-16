package otter

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestLoadRunV1CompilesBeaverRNASEQPDXPublishIntoImmutableResultsRoot(t *testing.T) {
	configurationPath := writeRunSnapshot(
		t,
		runSnapshotYAML("rna-pdx", "craftmake", "local", "true", true),
	)
	context, err := Load(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot := filepath.Clean(filepath.Join("..", "..", ".."))
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "publish.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("expected one RNA-seq PDX publish task, got %#v", plan.Tasks)
	}
	publishTask := plan.Tasks[0]
	if publishTask.Outputs["classification_summary"] != "/project/runs/example/results/pdx/graft/classification.tsv" ||
		publishTask.Outputs["manifest"] != "/project/runs/example/results/artifacts.json" {
		t.Fatalf("RNA-seq PDX publish outputs escaped immutable results root: %#v", publishTask.Outputs)
	}
	expectedInputs := map[string]string{
		"filtered_bam_directory": "/project/runs/example/work/bsmap/Filtered_bams",
		"count_matrix":           "/project/runs/example/results/methylation/matrix_count.txt",
		"normalized_matrix":      "/project/runs/example/results/methylation/matrix_norm.txt",
		"qc_summary":             "/project/runs/example/results/qc/qc_summary.xlsx",
		"splicing_outcome":       "/project/runs/example/work/bsmap/RNASplicing/splicing-outcome.json",
	}
	for inputName, expectedPath := range expectedInputs {
		inputPaths := publishTask.Inputs[inputName]
		if len(inputPaths) != 1 || inputPaths[0] != expectedPath {
			t.Fatalf("RNA-seq PDX publish input %q = %#v, want %q", inputName, inputPaths, expectedPath)
		}
	}
	absoluteConfigurationPath, err := filepath.Abs(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(publishTask.Steps[0].Command, "'"+absoluteConfigurationPath+"'") ||
		!strings.Contains(publishTask.Steps[0].Command, "results_root='/project/runs/example/results'") ||
		!strings.Contains(publishTask.Steps[0].Command, ".publish-staging") {
		t.Fatalf("RNA-seq PDX publish command did not use immutable snapshot paths: %q", publishTask.Steps[0].Command)
	}
}

func TestLoadRunV1CompilesBeaverPDXPublishIntoImmutableResultsRoot(t *testing.T) {
	configurationPath := writeRunSnapshot(
		t,
		runSnapshotYAML("bs-pdx", "craftmake", "local", "true", true),
	)
	context, err := Load(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot := filepath.Clean(filepath.Join("..", "..", ".."))
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverPDX", "publish.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("expected one PDX publish task, got %#v", plan.Tasks)
	}
	publishTask := plan.Tasks[0]
	if publishTask.Outputs["classification_summary"] != "/project/runs/example/results/pdx/graft/classification.tsv" ||
		publishTask.Outputs["methrix_data"] != "/project/runs/example/results/methylation/methrix_data.h5" ||
		publishTask.Outputs["manifest"] != "/project/runs/example/results/artifacts.json" {
		t.Fatalf("PDX publish outputs escaped immutable results root: %#v", publishTask.Outputs)
	}
	expectedInputs := map[string]string{
		"filtered_bam_directory": "/project/runs/example/work/bsmap/Filtered_bams",
		"methrix_data":           "/project/runs/example/work/mCall/methrixh5/methrix_data.h5",
		"bismark_summary":        "/project/runs/example/work/bsmap/hg38/bismark_summary_report.html",
		"qc_summary":             "/project/runs/example/results/qc/qc_summary.xlsx",
	}
	for inputName, expectedPath := range expectedInputs {
		inputPaths := publishTask.Inputs[inputName]
		if len(inputPaths) != 1 || inputPaths[0] != expectedPath {
			t.Fatalf("PDX publish input %q = %#v, want %q", inputName, inputPaths, expectedPath)
		}
	}
	absoluteConfigurationPath, err := filepath.Abs(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(publishTask.Steps[0].Command, "'"+absoluteConfigurationPath+"'") ||
		!strings.Contains(publishTask.Steps[0].Command, "results_root='/project/runs/example/results'") ||
		!strings.Contains(publishTask.Steps[0].Command, ".publish-staging") {
		t.Fatalf("PDX publish command did not use immutable snapshot paths: %q", publishTask.Steps[0].Command)
	}
}

func TestLoadRunV1CompilesBeaverRNAPublishIntoImmutableResultsRoot(t *testing.T) {
	configurationPath := writeRunSnapshot(
		t,
		runSnapshotYAML("rnaseq", "craftmake", "local", "true", false),
	)
	context, err := Load(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot := filepath.Clean(filepath.Join("..", "..", ".."))
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNA", "publish.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("expected one RNA publish task, got %#v", plan.Tasks)
	}
	publishTask := plan.Tasks[0]
	if publishTask.Outputs["manifest"] != "/project/runs/example/results/artifacts.json" {
		t.Fatalf("publish manifest escaped immutable results root: %#v", publishTask.Outputs)
	}
	expectedInputs := map[string]string{
		"count_matrix":      "/project/runs/example/results/methylation/matrix_count.txt",
		"normalized_matrix": "/project/runs/example/results/methylation/matrix_norm.txt",
		"qc_summary":        "/project/runs/example/results/qc/qc_summary.xlsx",
		"splicing_outcome":  "/project/runs/example/work/bsmap/RNASplicing/splicing-outcome.json",
	}
	for inputName, expectedPath := range expectedInputs {
		inputPaths := publishTask.Inputs[inputName]
		if len(inputPaths) != 1 || inputPaths[0] != expectedPath {
			t.Fatalf("publish input %q = %#v, want %q", inputName, inputPaths, expectedPath)
		}
	}
	absoluteConfigurationPath, err := filepath.Abs(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(publishTask.Steps[0].Command, "'"+absoluteConfigurationPath+"'") ||
		!strings.Contains(publishTask.Steps[0].Command, "'/project/runs/example/results/artifacts.json'") ||
		!strings.Contains(publishTask.Steps[0].Command, "results_root='/project/runs/example/results'") ||
		!strings.Contains(publishTask.Steps[0].Command, ".publish-staging") {
		t.Fatalf("publish command did not use immutable snapshot paths: %q", publishTask.Steps[0].Command)
	}
}
