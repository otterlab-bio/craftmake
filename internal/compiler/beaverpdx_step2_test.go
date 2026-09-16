package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverPDXStep2Fixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverPDX", "step2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "fixtures", "BeaverPDX", "step2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 12 || len(plan.Submissions) != 10 {
		t.Fatalf("expected twelve tasks and ten submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
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
	if independentSubmissions != 8 {
		t.Fatalf("expected eight independent quality-control submissions, got %d", independentSubmissions)
	}
	for _, speciesName := range []string{"human", "mouse"} {
		groupKey := "map_and_sort/species=" + speciesName
		mappingSubmission, exists := batchGroups[groupKey]
		if !exists {
			t.Fatalf("missing mapping batch group %q: %#v", groupKey, batchGroups)
		}
		if len(mappingSubmission.TaskIDs) != 2 {
			t.Fatalf("mapping batch %q should contain two sample workers: %#v", groupKey, mappingSubmission.TaskIDs)
		}
		if mappingSubmission.Resources.Cores != 80 || mappingSubmission.Resources.MemoryByte != 320<<30 {
			t.Fatalf("unexpected mapping allocation for %q: %#v", groupKey, mappingSubmission.Resources)
		}
		if mappingSubmission.Worker == nil || mappingSubmission.Worker.Resources.Cores != 40 || mappingSubmission.Worker.Resources.MemoryByte != 160<<30 || mappingSubmission.Worker.MaxParallel != 2 {
			t.Fatalf("unexpected worker plan for %q: %#v", groupKey, mappingSubmission.Worker)
		}
	}

	mappingTaskID := "BeaverPDX/step2/map_and_sort/sample=sample-a/species=mouse"
	mappingTask := plan.TaskByID[mappingTaskID]
	if mappingTask == nil {
		t.Fatalf("missing PDX mapping task %q", mappingTaskID)
	}
	if mappingTask.Inputs["genome_index"][0] != filepath.Join(repositoryRoot, "fixtures", "BeaverPDX", "references", "bismark-mouse") {
		t.Fatalf("unexpected mouse genome index: %#v", mappingTask.Inputs["genome_index"])
	}
	if mappingTask.Outputs["bam"] != filepath.Join("workflow", "bsmap", "sample-a_mouse.bam") {
		t.Fatalf("unexpected mouse BAM output %q", mappingTask.Outputs["bam"])
	}
	if len(mappingTask.Steps) != 2 || !strings.Contains(mappingTask.Steps[0].Command, "--parallel 8") {
		t.Fatalf("mapping command should use worker cores: %#v", mappingTask.Steps)
	}

	qualimapTask := plan.TaskByID["BeaverPDX/step2/qualimap/sample=sample-a/species=mouse"]
	if qualimapTask == nil || !containsTaskID(qualimapTask.Dependencies, mappingTaskID) {
		t.Fatalf("mouse Qualimap should depend on matching mapping task: %#v", qualimapTask)
	}
	gcBiasTask := plan.TaskByID["BeaverPDX/step2/collect_gc_bias/sample=sample-a/species=mouse"]
	if gcBiasTask == nil || !containsTaskID(gcBiasTask.Dependencies, mappingTaskID) {
		t.Fatalf("mouse GC bias should depend on matching mapping task: %#v", gcBiasTask)
	}
	if gcBiasTask.Inputs["reference"][0] != filepath.Join(repositoryRoot, "fixtures", "BeaverPDX", "references", "mouse.fasta") {
		t.Fatalf("unexpected mouse GC-bias reference: %#v", gcBiasTask.Inputs["reference"])
	}
	if !strings.Contains(gcBiasTask.Steps[0].Command, `export JAVA_HOME="${CONDA_PREFIX}/lib/jvm"`) {
		t.Fatalf("PDX GC bias task should bind Picard to the active environment Java runtime: %q", gcBiasTask.Steps[0].Command)
	}
}
