package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverRNASEQPDXStep3Fixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "step3.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverrnaseqpdx-step3.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 3 || len(plan.Submissions) != 3 {
		t.Fatalf("expected three tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	countTaskID := "BeaverRNASEQPDX/step3/count_expression/sample=sample-a"
	countTask := plan.TaskByID[countTaskID]
	if countTask == nil {
		t.Fatalf("missing RNA-seq PDX expression task %q", countTaskID)
	}
	if countTask.Inputs["filtered_bam"][0] != filepath.Join("workflow", "bsmap", "Filtered_bams", "sample-a_fixed_human_Filtered.bam") {
		t.Fatalf("unexpected filtered graft BAM: %#v", countTask.Inputs["filtered_bam"])
	}
	expectedAnnotation := filepath.Join(repositoryRoot, "testdata", "configs", "references", "human.gtf")
	if countTask.Inputs["annotation"][0] != expectedAnnotation {
		t.Fatalf("unexpected graft annotation: %#v", countTask.Inputs["annotation"])
	}
	if countTask.Outputs["counts"] != filepath.Join("workflow", "expression", "sample-a_human.txt") {
		t.Fatalf("unexpected RNA-seq PDX count output %q", countTask.Outputs["counts"])
	}
	if countTask.Resources.Cores != 1 || countTask.Resources.MemoryByte != 16<<30 {
		t.Fatalf("unexpected HTSeq resources: %#v", countTask.Resources)
	}
	countCommand := countTask.Steps[0].Command
	if !strings.Contains(countCommand, "htseq-count -f bam -r pos -s yes -t exon -i gene_id -m intersection-nonempty") {
		t.Fatalf("unexpected RNA-seq PDX HTSeq command: %q", countCommand)
	}

	splicingTask := plan.TaskByID["BeaverRNASEQPDX/step3/rnaseq_splicing"]
	if splicingTask == nil || len(splicingTask.Inputs["sample_counts"]) != 2 || len(splicingTask.Dependencies) != 2 {
		t.Fatalf("unexpected RNA-seq PDX splicing aggregation: %#v", splicingTask)
	}
	if !containsTaskID(splicingTask.Dependencies, countTaskID) {
		t.Fatalf("splicing task should depend on sample expression tasks: %#v", splicingTask.Dependencies)
	}
	if splicingTask.Resources.Cores != 20 || splicingTask.Resources.MemoryByte != 32<<30 {
		t.Fatalf("unexpected splicing resources: %#v", splicingTask.Resources)
	}
	splicingCommand := splicingTask.Steps[0].Command
	for _, requiredFragment := range []string{
		"matsrun run",
		"--pdxmode 1",
		"--seqlengthQC 'workflow/fastqc_clean'",
		"--seqlength-qc-format fastqcx",
	} {
		if !strings.Contains(splicingCommand, requiredFragment) {
			t.Fatalf("RNA-seq PDX splicing command does not contain %q: %q", requiredFragment, splicingCommand)
		}
	}
	if !strings.Contains(splicingCommand, `"status": "produced"`) || !strings.Contains(splicingCommand, `"status": "not_applicable"`) {
		t.Fatalf("splicing command should emit a typed produced/not_applicable outcome: %q", splicingCommand)
	}
	if splicingTask.Outputs["outcome"] != filepath.Join("workflow", "bsmap", "RNASplicing", "splicing-outcome.json") {
		t.Fatalf("unexpected splicing outcome %q", splicingTask.Outputs["outcome"])
	}
}
