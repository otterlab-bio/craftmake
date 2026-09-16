package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverBSStep2CheckFixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step2-check.yaml"))
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
	if len(plan.Tasks) != 2 || len(plan.Submissions) != 2 {
		t.Fatalf("expected two sample-validation tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	artifactTask := plan.TaskByID["BeaverBS/step2-check/sample_artifacts/sample=sample-a/species=human"]
	if artifactTask == nil || len(artifactTask.Inputs) != 7 {
		t.Fatalf("unexpected sample artifact task: %#v", artifactTask)
	}
	for _, requiredFragment := range []string{
		"otter.sample-artifacts-validation/v1",
		"gc_metrics",
		"gc_chart",
		"gc_summary",
		"sha256sum",
		"regular non-symlink file",
	} {
		if !strings.Contains(artifactTask.Steps[0].Command, requiredFragment) {
			t.Fatalf("sample validation manifest command does not contain %q:\n%s", requiredFragment, artifactTask.Steps[0].Command)
		}
	}
	if plan.TaskByID["BeaverBS/step2-check/multiqc"] != nil {
		t.Fatal("BeaverBS step2-check must not schedule MultiQC")
	}
	if plan.TaskByID["BeaverBS/step2-check/step2_checker"] != nil {
		t.Fatal("step2-check must not create a marker-only checker task")
	}
}
