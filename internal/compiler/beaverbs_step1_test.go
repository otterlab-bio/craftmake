package compiler_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverBSStep1Fixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverbs-step1.yaml"))
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
	if len(plan.Order) != 6 {
		t.Fatalf("expected six tasks in topological order, got %d", len(plan.Order))
	}

	fastqcBeforeSampleA := plan.TaskByID["BeaverBS/step1/fastqc_before/sample=sample-a"]
	if fastqcBeforeSampleA == nil {
		t.Fatal("missing sample-a fastqc_before task")
	}
	if len(fastqcBeforeSampleA.Dependencies) != 0 {
		t.Fatalf("fastqc_before should have no dependencies: %#v", fastqcBeforeSampleA.Dependencies)
	}
	if fastqcBeforeSampleA.Outputs["read1_data"] != filepath.Join("workflow", "fastqc_raw", "sample-a_R1_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected pre-trim Fastqcx output %q", fastqcBeforeSampleA.Outputs["read1_data"])
	}

	fastqcAfterSampleA := plan.TaskByID["BeaverBS/step1/fastqc_after/sample=sample-a"]
	if fastqcAfterSampleA == nil || !containsTaskID(fastqcAfterSampleA.Dependencies, "BeaverBS/step1/trim_reads/sample=sample-a") {
		t.Fatalf("fastqc_after should depend on sample-a trimming: %#v", fastqcAfterSampleA)
	}
	if fastqcAfterSampleA.Outputs["read2_data"] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_2_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected post-trim Fastqcx output %q", fastqcAfterSampleA.Outputs["read2_data"])
	}

	if plan.TaskByID["BeaverBS/step1/step1_checker"] != nil {
		t.Fatal("step1 must terminate in per-sample QC and trimming artifacts, not a marker-only checker task")
	}
	if fastqcAfterSampleA.Outputs["read1_data"] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_1_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected post-trim terminal output %q", fastqcAfterSampleA.Outputs["read1_data"])
	}
	if !strings.Contains(plan.TaskByID["BeaverBS/step1/trim_reads/sample=sample-a"].Steps[0].Command, "trim_galore") {
		t.Fatal("trim task command does not contain trim_galore")
	}
}

func containsTaskID(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func repositoryRootForTest(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve current test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}
