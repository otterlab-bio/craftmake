package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverRNASEQPDXStep3CheckFixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "step3-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverrnaseqpdx-step3-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 8 || len(plan.Submissions) != 8 {
		t.Fatalf("expected eight tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	sampleTask := plan.TaskByID["BeaverRNASEQPDX/step3-check/sample_artifacts/sample=sample-a"]
	if sampleTask == nil || len(sampleTask.Inputs) != 8 {
		t.Fatalf("unexpected RNA-seq PDX sample artifact task: %#v", sampleTask)
	}
	if sampleTask.Inputs["counts"][0] != filepath.Join("workflow", "expression", "sample-a_human.txt") {
		t.Fatalf("unexpected graft count input: %#v", sampleTask.Inputs["counts"])
	}
	if sampleTask.Inputs["filtered_bam"][0] != filepath.Join("workflow", "bsmap", "Filtered_bams", "sample-a_fixed_human_Filtered.bam") {
		t.Fatalf("unexpected filtered graft BAM input: %#v", sampleTask.Inputs["filtered_bam"])
	}
	if sampleTask.Inputs["fastqc_before_r1"][0] != filepath.Join("workflow", "fastqc_raw", "sample-a_R1_fastqcx", "fastqc_data.txt") ||
		sampleTask.Inputs["fastqc_after_r2"][0] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_2_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected RNA-seq PDX Fastqcx artifact inputs: %#v", sampleTask.Inputs)
	}
	for _, requiredFragment := range []string{
		"otter.sample-artifacts-validation/v1",
		"counts",
		"filtered_bam",
		"sha256sum",
		"regular non-symlink file",
	} {
		if !strings.Contains(sampleTask.Steps[0].Command, requiredFragment) {
			t.Fatalf("RNA-seq PDX sample validation command does not contain %q:\n%s", requiredFragment, sampleTask.Steps[0].Command)
		}
	}

	speciesTask := plan.TaskByID["BeaverRNASEQPDX/step3-check/species_qc_artifacts/sample=sample-b/species=mouse"]
	if speciesTask == nil || len(speciesTask.Inputs) != 2 {
		t.Fatalf("unexpected species QC task: %#v", speciesTask)
	}
	if speciesTask.Inputs["sorted_bam"][0] != filepath.Join("workflow", "bsmap", "sample-b_mouse.bam") {
		t.Fatalf("unexpected mouse mapping BAM input: %#v", speciesTask.Inputs["sorted_bam"])
	}
	for _, requiredFragment := range []string{
		"otter.sample-artifacts-validation/v1",
		"sorted_bam",
		"qualimap_report",
		"sha256sum",
	} {
		if !strings.Contains(speciesTask.Steps[0].Command, requiredFragment) {
			t.Fatalf("RNA-seq PDX species validation command does not contain %q:\n%s", requiredFragment, speciesTask.Steps[0].Command)
		}
	}

	matrixTask := plan.TaskByID["BeaverRNASEQPDX/step3-check/construct_expression_matrix"]
	if matrixTask == nil || len(matrixTask.Inputs["sample_artifacts"]) != 2 || len(matrixTask.Dependencies) != 2 {
		t.Fatalf("unexpected expression matrix aggregation: %#v", matrixTask)
	}
	if !strings.Contains(matrixTask.Steps[0].Command, "--postfix '_human.txt'") {
		t.Fatalf("unexpected graft matrix command: %q", matrixTask.Steps[0].Command)
	}

	qcSummaryTask := plan.TaskByID["BeaverRNASEQPDX/step3-check/qc_summary"]
	if qcSummaryTask == nil || len(qcSummaryTask.Inputs["species_qc_artifacts"]) != 4 || len(qcSummaryTask.Dependencies) != 6 {
		t.Fatalf("unexpected RNA-seq PDX QC summary aggregation: %#v", qcSummaryTask)
	}
	if !strings.Contains(qcSummaryTask.Steps[0].Command, "qctb") || !strings.Contains(qcSummaryTask.Steps[0].Command, "--rnaseq") {
		t.Fatalf("unexpected RNA-seq PDX QC command: %q", qcSummaryTask.Steps[0].Command)
	}
	if strings.Contains(qcSummaryTask.Steps[0].Command, "yaml.safe_load") ||
		!strings.Contains(qcSummaryTask.Steps[0].Command, "qctb --config '") {
		t.Fatalf("RNA-seq PDX QC summary must pass its immutable run configuration directly to QCTB: %q", qcSummaryTask.Steps[0].Command)
	}

	if plan.TaskByID["BeaverRNASEQPDX/step3-check/step3_checker"] != nil {
		t.Fatal("step3-check must terminate in RNA-seq PDX analysis artifacts, not a marker-only checker task")
	}
	if matrixTask.Outputs["count_matrix"] != filepath.Join("workflow", "expression", "matrix", "matrix_count.txt") ||
		matrixTask.Outputs["normalized_matrix"] != filepath.Join("workflow", "expression", "matrix", "matrix_norm.txt") {
		t.Fatalf("unexpected RNA-seq PDX matrix terminal outputs: %#v", matrixTask.Outputs)
	}
	if qcSummaryTask.Outputs["report"] != filepath.Join("workflow", "QC", "summary", "qc_summary.xlsx") {
		t.Fatalf("unexpected RNA-seq PDX QC terminal output: %#v", qcSummaryTask.Outputs)
	}
}
