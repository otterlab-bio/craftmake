package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverRNASEQPDXStep2Fixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "step2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverrnaseqpdx-step2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 8 || len(plan.Submissions) != 6 {
		t.Fatalf("expected eight tasks and six submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	humanMappingTaskID := "BeaverRNASEQPDX/step2/map_and_sort/sample=sample-a/species=human"
	humanMappingTask := plan.TaskByID[humanMappingTaskID]
	if humanMappingTask == nil {
		t.Fatalf("missing human mapping task %q", humanMappingTaskID)
	}
	expectedHumanReference := filepath.Join(repositoryRoot, "testdata", "configs", "references", "star-human")
	if humanMappingTask.Inputs["genome_directory"][0] != expectedHumanReference {
		t.Fatalf("unexpected human STAR reference: %#v", humanMappingTask.Inputs["genome_directory"])
	}
	if !strings.Contains(humanMappingTask.Steps[0].Command, "STAR --runThreadN '40'") {
		t.Fatalf("unexpected STAR worker command: %s", humanMappingTask.Steps[0].Command)
	}
	mouseMappingTask := plan.TaskByID["BeaverRNASEQPDX/step2/map_and_sort/sample=sample-b/species=mouse"]
	if mouseMappingTask == nil || mouseMappingTask.Outputs["bam"] != filepath.Join("workflow", "bsmap", "sample-b_mouse.bam") {
		t.Fatalf("unexpected mouse mapping task: %#v", mouseMappingTask)
	}

	batchGroups := make(map[string]compiler.SubmissionGroup)
	independentSubmissions := 0
	for _, submission := range plan.Submissions {
		if submission.Scope == "batch" {
			batchGroups[submission.GroupKey] = submission
			continue
		}
		independentSubmissions++
	}
	if independentSubmissions != 4 {
		t.Fatalf("expected four independent Qualimap submissions, got %d", independentSubmissions)
	}
	for _, speciesName := range []string{"human", "mouse"} {
		groupKey := "map_and_sort/species=" + speciesName
		mappingSubmission, exists := batchGroups[groupKey]
		if !exists {
			t.Fatalf("missing STAR mapping batch group %q: %#v", groupKey, batchGroups)
		}
		if len(mappingSubmission.TaskIDs) != 2 {
			t.Fatalf("expected two sample workers in mapping batch %q: %#v", groupKey, mappingSubmission.TaskIDs)
		}
		if mappingSubmission.Resources.Cores != 80 || mappingSubmission.Resources.MemoryByte != 160<<30 {
			t.Fatalf("unexpected mapping allocation for %q: %#v", groupKey, mappingSubmission.Resources)
		}
		if mappingSubmission.Worker == nil || mappingSubmission.Worker.Resources.Cores != 40 || mappingSubmission.Worker.Resources.MemoryByte != 64<<30 || mappingSubmission.Worker.MaxParallel != 2 {
			t.Fatalf("unexpected mapping worker plan for %q: %#v", groupKey, mappingSubmission.Worker)
		}
	}

	qualimapTask := plan.TaskByID["BeaverRNASEQPDX/step2/qualimap/sample=sample-a/species=human"]
	if qualimapTask == nil || !containsTaskID(qualimapTask.Dependencies, humanMappingTaskID) {
		t.Fatalf("human Qualimap should depend on sample-a human mapping: %#v", qualimapTask)
	}
}
