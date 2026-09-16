package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverRNAStep2CheckFixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNA", "step2-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverrna-step2-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 5 || len(plan.Submissions) != 5 {
		t.Fatalf("expected five tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	artifactTask := plan.TaskByID["BeaverRNA/step2-check/sample_artifacts/sample=sample-a/species=human"]
	if artifactTask == nil || len(artifactTask.Inputs) != 9 {
		t.Fatalf("unexpected BeaverRNA sample artifact task: %#v", artifactTask)
	}
	if artifactTask.Inputs["counts"][0] != filepath.Join("workflow", "expression", "sample-a_human.txt") {
		t.Fatalf("unexpected RNA-seq count input: %#v", artifactTask.Inputs["counts"])
	}
	if artifactTask.Inputs["fastqc_before_r1"][0] != filepath.Join("workflow", "fastqc_raw", "sample-a_R1_fastqcx", "fastqc_data.txt") ||
		artifactTask.Inputs["fastqc_after_r2"][0] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_2_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected BeaverRNA Fastqcx artifact inputs: %#v", artifactTask.Inputs)
	}
	for _, requiredFragment := range []string{
		"otter.sample-artifacts-validation/v1",
		"counts",
		"sorted_bam",
		"qualimap_report",
		"fastqc_after_r2",
		"sha256sum",
		"regular non-symlink file",
	} {
		if !strings.Contains(artifactTask.Steps[0].Command, requiredFragment) {
			t.Fatalf("sample validation manifest command does not contain %q:\n%s", requiredFragment, artifactTask.Steps[0].Command)
		}
	}

	matrixTask := plan.TaskByID["BeaverRNA/step2-check/construct_expression_matrix"]
	if matrixTask == nil || len(matrixTask.Inputs["sample_artifacts"]) != 2 || len(matrixTask.Dependencies) != 2 {
		t.Fatalf("unexpected expression matrix aggregation: %#v", matrixTask)
	}
	if !strings.Contains(matrixTask.Steps[0].Command, "--postfix '_human.txt'") {
		t.Fatalf("unexpected seq2mat command: %s", matrixTask.Steps[0].Command)
	}

	splicingTask := plan.TaskByID["BeaverRNA/step2-check/rnaseq_splicing"]
	if splicingTask == nil || len(splicingTask.Dependencies) != 2 {
		t.Fatalf("unexpected RNA splicing aggregation: %#v", splicingTask)
	}
	if !strings.Contains(splicingTask.Steps[0].Command, "matsrun run") || !strings.Contains(splicingTask.Steps[0].Command, "--pdxmode 0") {
		t.Fatalf("unexpected RNA splicing command: %s", splicingTask.Steps[0].Command)
	}

	qcSummaryTask := plan.TaskByID["BeaverRNA/step2-check/qc_summary"]
	if qcSummaryTask == nil || len(qcSummaryTask.Dependencies) != 2 {
		t.Fatalf("unexpected RNA QC summary aggregation: %#v", qcSummaryTask)
	}
	if plan.TaskByID["BeaverRNA/step2-check/step2_checker"] != nil {
		t.Fatal("step2-check must terminate in typed RNA artifacts, not a marker-only checker task")
	}
	if matrixTask.Outputs["count_matrix"] != filepath.Join("workflow", "expression", "matrix", "matrix_count.txt") ||
		matrixTask.Outputs["normalized_matrix"] != filepath.Join("workflow", "expression", "matrix", "matrix_norm.txt") {
		t.Fatalf("unexpected expression matrix terminal outputs: %#v", matrixTask.Outputs)
	}
	if splicingTask.Outputs["outcome"] != filepath.Join("workflow", "bsmap", "RNASplicing", "splicing-outcome.json") {
		t.Fatalf("unexpected typed splicing terminal output: %#v", splicingTask.Outputs)
	}
	if qcSummaryTask.Outputs["report"] != filepath.Join("workflow", "QC", "summary", "qc_summary.xlsx") {
		t.Fatalf("unexpected QC terminal output: %#v", qcSummaryTask.Outputs)
	}
}
