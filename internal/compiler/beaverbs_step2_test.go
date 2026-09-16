package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverBSStep2Fixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverbs-step2.yaml"))
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

	mappingTaskID := "BeaverBS/step2/map_and_sort/sample=sample-a/species=human"
	mappingTask := plan.TaskByID[mappingTaskID]
	if mappingTask == nil {
		t.Fatalf("missing mapping task %q", mappingTaskID)
	}
	if mappingTask.Inputs["genome_index"][0] != filepath.Join(repositoryRoot, "testdata", "configs", "references", "bismark-human") {
		t.Fatalf("unexpected normalized genome index: %#v", mappingTask.Inputs["genome_index"])
	}
	if mappingTask.Outputs["bam"] != filepath.Join("workflow", "bsmap", "sample-a_human.bam") {
		t.Fatalf("unexpected BAM output %q", mappingTask.Outputs["bam"])
	}
	if len(mappingTask.Steps) != 2 || !strings.Contains(mappingTask.Steps[0].Command, "bismark") || !strings.Contains(mappingTask.Steps[1].Command, "samtools sort") {
		t.Fatalf("unexpected mapping steps: %#v", mappingTask.Steps)
	}

	qualimapTask := plan.TaskByID["BeaverBS/step2/qualimap/sample=sample-a/species=human"]
	if qualimapTask == nil || !containsTaskID(qualimapTask.Dependencies, mappingTaskID) {
		t.Fatalf("qualimap should depend on sample-a mapping: %#v", qualimapTask)
	}
	gcBiasTask := plan.TaskByID["BeaverBS/step2/collect_gc_bias/sample=sample-a/species=human"]
	if gcBiasTask == nil || !containsTaskID(gcBiasTask.Dependencies, mappingTaskID) {
		t.Fatalf("GC bias should depend on sample-a mapping: %#v", gcBiasTask)
	}
	if !strings.Contains(gcBiasTask.Steps[0].Command, `export JAVA_HOME="${CONDA_PREFIX}/lib/jvm"`) {
		t.Fatalf("GC bias task should bind Picard to the active environment Java runtime: %q", gcBiasTask.Steps[0].Command)
	}
}
