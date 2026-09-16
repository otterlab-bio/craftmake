package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverPDXStep3CheckFixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverPDX", "step3-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "fixtures", "BeaverPDX", "step3-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 12 || len(plan.Submissions) != 12 {
		t.Fatalf("expected twelve tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	artifactTask := plan.TaskByID["BeaverPDX/step3-check/sample_artifacts/sample=sample-a"]
	if artifactTask == nil || len(artifactTask.Inputs) != 9 {
		t.Fatalf("unexpected PDX step3 sample artifact task: %#v", artifactTask)
	}
	if artifactTask.Inputs["filtered_bam"][0] != filepath.Join("workflow", "bsmap", "Filtered_bams", "sample-a_fixed_human_Filtered.bam") {
		t.Fatalf("unexpected filtered graft BAM input: %#v", artifactTask.Inputs["filtered_bam"])
	}
	if artifactTask.Inputs["fastqc_before_r1"][0] != filepath.Join("workflow", "fastqc_raw", "sample-a_R1_fastqcx", "fastqc_data.txt") ||
		artifactTask.Inputs["fastqc_after_r2"][0] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_2_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected PDX Fastqcx artifact inputs: %#v", artifactTask.Inputs)
	}
	for _, requiredFragment := range []string{
		"otter.sample-artifacts-validation/v1",
		"coverage",
		"filtered_bam",
		"sha256sum",
		"regular non-symlink file",
	} {
		if !strings.Contains(artifactTask.Steps[0].Command, requiredFragment) {
			t.Fatalf("PDX sample validation command does not contain %q:\n%s", requiredFragment, artifactTask.Steps[0].Command)
		}
	}

	speciesTask := plan.TaskByID["BeaverPDX/step3-check/species_qc_artifacts/sample=sample-b/species=mouse"]
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
			t.Fatalf("PDX species validation command does not contain %q:\n%s", requiredFragment, speciesTask.Steps[0].Command)
		}
	}

	referenceTask := plan.TaskByID["BeaverPDX/step3-check/prepare_methrix_reference"]
	if referenceTask == nil || referenceTask.Inputs["genome"][0] != filepath.Join(repositoryRoot, "fixtures", "BeaverPDX", "references", "human.fasta") {
		t.Fatalf("unexpected PDX graft reference task: %#v", referenceTask)
	}
	if referenceTask.Inputs["annotation"][0] != filepath.Join(repositoryRoot, "fixtures", "BeaverPDX", "references", "human.gtf") {
		t.Fatalf("unexpected resolved PDX Methrix annotation input: %#v", referenceTask.Inputs["annotation"])
	}
	for _, requiredFragment := range []string{
		"annotation_path='" + filepath.Join(repositoryRoot, "fixtures", "BeaverPDX", "references", "human.gtf") + "'",
		"Methrix annotation must be a regular resolved GTF",
		"$genome_key.gtf\"",
	} {
		if !strings.Contains(referenceTask.Steps[0].Command, requiredFragment) {
			t.Fatalf("PDX Methrix reference preparation does not stage resolved annotation %q: %s", requiredFragment, referenceTask.Steps[0].Command)
		}
	}
	if !strings.Contains(referenceTask.Steps[0].Command, "--contigs") || !strings.Contains(referenceTask.Steps[0].Command, `contig_arguments+=(--contigs "$contig")`) || !strings.Contains(referenceTask.Steps[0].Command, "methx_extract_command") {
		t.Fatalf("PDX Methrix reference preparation lacks repeated nonstandard contig flags: %s", referenceTask.Steps[0].Command)
	}
	if !strings.Contains(referenceTask.Steps[0].Command, "METHX:-methx") {
		t.Fatalf("PDX Methrix reference preparation should support an explicit executable override: %s", referenceTask.Steps[0].Command)
	}

	methrixTask := plan.TaskByID["BeaverPDX/step3-check/create_methrix_object"]
	if methrixTask == nil || len(methrixTask.Inputs["sample_artifacts"]) != 2 || len(methrixTask.Dependencies) != 3 {
		t.Fatalf("unexpected PDX Methrix aggregation: %#v", methrixTask)
	}
	if methrixTask.Outputs["methrix_data"] != filepath.Join("workflow", "mCall", "methrixh5", "methrix_data.h5") ||
		methrixTask.Outputs["assays"] != filepath.Join("workflow", "mCall", "methrixh5", "assays.h5") ||
		methrixTask.Outputs["annotation_report"] != filepath.Join("workflow", "mCall", "methrixh5", "CpG_annotation_report.xlsx") ||
		methrixTask.Outputs["annotation_details"] != filepath.Join("workflow", "mCall", "methrixh5", "CpG_annotation_details.tsv.gz") {
		t.Fatalf("PDX Methrix output set must include all executor outputs: %#v", methrixTask.Outputs)
	}
	if !strings.Contains(methrixTask.Steps[0].Command, "--annotation-dir 'workflow/mCall/methrixh5'") {
		t.Fatalf("PDX Methrix process must provide the staged annotation directory: %s", methrixTask.Steps[0].Command)
	}
	if !strings.Contains(methrixTask.Steps[0].Command, "METHX:-methx") {
		t.Fatalf("PDX Methrix process should support an explicit executable override: %s", methrixTask.Steps[0].Command)
	}

	bismarkReportTask := plan.TaskByID["BeaverPDX/step3-check/bismark_report/sample=sample-a"]
	if bismarkReportTask == nil || !strings.Contains(bismarkReportTask.Steps[0].Command, "bismark2report") {
		t.Fatalf("unexpected PDX Bismark report task: %#v", bismarkReportTask)
	}
	bismarkSummaryTask := plan.TaskByID["BeaverPDX/step3-check/bismark_summary"]
	if bismarkSummaryTask == nil || len(bismarkSummaryTask.Inputs["sample_reports"]) != 2 || len(bismarkSummaryTask.Dependencies) != 2 {
		t.Fatalf("unexpected PDX Bismark summary aggregation: %#v", bismarkSummaryTask)
	}

	qcSummaryTask := plan.TaskByID["BeaverPDX/step3-check/qc_summary"]
	if qcSummaryTask == nil || len(qcSummaryTask.Inputs["species_qc_artifacts"]) != 4 || len(qcSummaryTask.Dependencies) != 7 {
		t.Fatalf("unexpected PDX QC summary aggregation: %#v", qcSummaryTask)
	}
	if !strings.Contains(qcSummaryTask.Steps[0].Command, "qctb --config '") ||
		strings.Contains(qcSummaryTask.Steps[0].Command, "yaml.safe_load") {
		t.Fatalf("PDX QC summary must pass its immutable run configuration directly to QCTB: %s", qcSummaryTask.Steps[0].Command)
	}
	if plan.TaskByID["BeaverPDX/step3-check/step3_checker"] != nil {
		t.Fatal("step3-check must terminate in typed PDX analysis artifacts, not a marker-only checker task")
	}
	if bismarkSummaryTask.Outputs["report"] != filepath.Join("workflow", "bsmap", "human", "bismark_summary_report.html") {
		t.Fatalf("unexpected Bismark summary terminal output: %#v", bismarkSummaryTask.Outputs)
	}
	if qcSummaryTask.Outputs["report"] != filepath.Join("workflow", "QC", "summary", "qc_summary.xlsx") {
		t.Fatalf("unexpected PDX QC terminal output: %#v", qcSummaryTask.Outputs)
	}
}
