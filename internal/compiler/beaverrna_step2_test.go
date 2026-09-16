package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverRNAStep2Fixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNA", "step2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverrna-step2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 6 || len(plan.Submissions) != 6 {
		t.Fatalf("expected six tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	mappingTaskID := "BeaverRNA/step2/map_and_sort/sample=sample-a/species=human"
	mappingTask := plan.TaskByID[mappingTaskID]
	if mappingTask == nil {
		t.Fatalf("missing mapping task %q", mappingTaskID)
	}
	expectedReferenceDirectory := filepath.Join(repositoryRoot, "testdata", "configs", "references", "star-human")
	if mappingTask.Inputs["genome_directory"][0] != expectedReferenceDirectory {
		t.Fatalf("unexpected normalized STAR reference: %#v", mappingTask.Inputs["genome_directory"])
	}
	if mappingTask.Outputs["bam"] != filepath.Join("workflow", "bsmap", "sample-a_human.bam") {
		t.Fatalf("unexpected RNA-seq BAM output %q", mappingTask.Outputs["bam"])
	}
	if len(mappingTask.Steps) != 2 || !strings.Contains(mappingTask.Steps[0].Command, "STAR --runThreadN '40'") || !strings.Contains(mappingTask.Steps[1].Command, "mv \"$aligned_bam\"") || !strings.Contains(mappingTask.Steps[1].Command, "samtools index -@ 8") {
		t.Fatalf("unexpected STAR mapping steps: %#v", mappingTask.Steps)
	}

	qualimapTask := plan.TaskByID["BeaverRNA/step2/qualimap/sample=sample-a/species=human"]
	if qualimapTask == nil || !containsTaskID(qualimapTask.Dependencies, mappingTaskID) {
		t.Fatalf("Qualimap should depend on sample-a STAR mapping: %#v", qualimapTask)
	}
	countTask := plan.TaskByID["BeaverRNA/step2/count_expression/sample=sample-a/species=human"]
	if countTask == nil || !containsTaskID(countTask.Dependencies, mappingTaskID) {
		t.Fatalf("HTSeq should depend on sample-a STAR mapping: %#v", countTask)
	}
	expectedAnnotation := filepath.Join(repositoryRoot, "testdata", "configs", "references", "human.gtf")
	if countTask.Inputs["annotation"][0] != expectedAnnotation {
		t.Fatalf("unexpected normalized RNA-seq annotation: %#v", countTask.Inputs["annotation"])
	}
	if countTask.Outputs["counts"] != filepath.Join("workflow", "expression", "sample-a_human.txt") {
		t.Fatalf("unexpected HTSeq count output %q", countTask.Outputs["counts"])
	}
	if !strings.Contains(countTask.Steps[0].Command, "htseq-count") {
		t.Fatalf("unexpected HTSeq command: %s", countTask.Steps[0].Command)
	}
}
