package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverBSStep3CheckFixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step3-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverbs-step3-check.yaml"))
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

	firstArtifactTask := plan.TaskByID["BeaverBS/step3-check/sample_artifacts/sample=sample-a"]
	if firstArtifactTask == nil || len(firstArtifactTask.Inputs) != 10 {
		t.Fatalf("unexpected sample artifact validation task: %#v", firstArtifactTask)
	}
	if firstArtifactTask.Inputs["coverage"][0] != filepath.Join("workflow", "mCall", "sample-a_nsort.bismark.cov.gz") {
		t.Fatalf("unexpected sample coverage input: %#v", firstArtifactTask.Inputs["coverage"])
	}
	if firstArtifactTask.Inputs["fastqc_before_r1"][0] != filepath.Join("workflow", "fastqc_raw", "sample-a_R1_fastqcx", "fastqc_data.txt") ||
		firstArtifactTask.Inputs["fastqc_after_r2"][0] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_2_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected Fastqcx artifact inputs: %#v", firstArtifactTask.Inputs)
	}
	validationCommand := firstArtifactTask.Steps[0].Command
	for _, requiredFragment := range []string{
		"otter.sample-artifacts-validation/v1",
		"sha256sum",
		"mktemp",
		"regular non-symlink file",
	} {
		if !strings.Contains(validationCommand, requiredFragment) {
			t.Fatalf("sample validation manifest command does not contain %q:\n%s", requiredFragment, validationCommand)
		}
	}

	prepareReferenceTask := plan.TaskByID["BeaverBS/step3-check/prepare_methrix_reference"]
	if prepareReferenceTask == nil {
		t.Fatal("missing Methrix reference preparation task")
	}
	expectedReference := filepath.Join(repositoryRoot, "testdata", "configs", "references", "human.fasta")
	if prepareReferenceTask.Inputs["genome"][0] != expectedReference {
		t.Fatalf("unexpected canonical graft reference: %#v", prepareReferenceTask.Inputs["genome"])
	}
	if prepareReferenceTask.Inputs["annotation"][0] != filepath.Join(repositoryRoot, "testdata", "configs", "references", "human.gtf") {
		t.Fatalf("unexpected resolved Methrix annotation input: %#v", prepareReferenceTask.Inputs["annotation"])
	}
	for _, requiredFragment := range []string{
		"annotation_path='" + filepath.Join(repositoryRoot, "testdata", "configs", "references", "human.gtf") + "'",
		"Methrix annotation must be a regular resolved GTF",
		"$genome_key.gtf\"",
	} {
		if !strings.Contains(prepareReferenceTask.Steps[0].Command, requiredFragment) {
			t.Fatalf("Methrix reference preparation does not stage resolved annotation %q: %s", requiredFragment, prepareReferenceTask.Steps[0].Command)
		}
	}
	if !strings.Contains(prepareReferenceTask.Steps[0].Command, "--contigs") || !strings.Contains(prepareReferenceTask.Steps[0].Command, `contig_arguments+=(--contigs "$contig")`) || !strings.Contains(prepareReferenceTask.Steps[0].Command, "methx_extract_command") {
		t.Fatalf("Methrix reference preparation lacks repeated nonstandard contig flags: %s", prepareReferenceTask.Steps[0].Command)
	}
	if !strings.Contains(prepareReferenceTask.Steps[0].Command, "METHX:-methx") {
		t.Fatalf("Methrix reference preparation should support an explicit executable override: %s", prepareReferenceTask.Steps[0].Command)
	}

	methrixTask := plan.TaskByID["BeaverBS/step3-check/create_methrix_object"]
	if methrixTask == nil || len(methrixTask.Inputs["sample_artifacts"]) != 2 || len(methrixTask.Dependencies) != 3 {
		t.Fatalf("Methrix task should depend on both sample artifacts and reference preparation: %#v", methrixTask)
	}
	if methrixTask.Outputs["methrix_data"] != filepath.Join("workflow", "mCall", "methrixh5", "methrix_data.h5") {
		t.Fatalf("unexpected Methrix HDF5 output %q", methrixTask.Outputs["methrix_data"])
	}
	if !strings.Contains(methrixTask.Steps[0].Command, "--annotation-dir 'workflow/mCall/methrixh5'") {
		t.Fatalf("Methrix process must provide the staged annotation directory: %s", methrixTask.Steps[0].Command)
	}
	if !strings.Contains(methrixTask.Steps[0].Command, "METHX:-methx") {
		t.Fatalf("Methrix process should support an explicit executable override: %s", methrixTask.Steps[0].Command)
	}

	bismarkSummaryTask := plan.TaskByID["BeaverBS/step3-check/bismark_summary"]
	if bismarkSummaryTask == nil || len(bismarkSummaryTask.Inputs["sample_reports"]) != 2 || len(bismarkSummaryTask.Dependencies) != 2 {
		t.Fatalf("Bismark summary should aggregate both sample reports: %#v", bismarkSummaryTask)
	}

	qcSummaryTask := plan.TaskByID["BeaverBS/step3-check/qc_summary"]
	if qcSummaryTask == nil || len(qcSummaryTask.Inputs["sample_artifacts"]) != 2 || len(qcSummaryTask.Dependencies) != 3 {
		t.Fatalf("unexpected QC summary aggregation: %#v", qcSummaryTask)
	}
	if plan.TaskByID["BeaverBS/step3-check/step3_checker"] != nil {
		t.Fatal("step3-check must terminate in analysis artifacts, not a marker-only checker task")
	}
	if bismarkSummaryTask.Outputs["report"] != filepath.Join("workflow", "bsmap", "human", "bismark_summary_report.html") {
		t.Fatalf("unexpected Bismark summary terminal output: %#v", bismarkSummaryTask.Outputs)
	}
	if qcSummaryTask.Outputs["report"] != filepath.Join("workflow", "QC", "summary", "qc_summary.xlsx") {
		t.Fatalf("unexpected QC terminal output: %#v", qcSummaryTask.Outputs)
	}
}
