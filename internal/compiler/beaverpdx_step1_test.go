package compiler_test

import (
	"path/filepath"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverPDXStep1Fixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverPDX", "step1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverpdx-step1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if context.Workflow.WorkflowName != "BeaverPDX" || !context.Workflow.PDXMode {
		t.Fatalf("unexpected PDX context: %#v", context.Workflow)
	}
	if len(context.Species) != 2 || context.Species[0].Name != "human" || context.Species[1].Name != "mouse" {
		t.Fatalf("unexpected PDX species contexts: %#v", context.Species)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 6 || len(plan.Submissions) != 6 {
		t.Fatalf("expected six tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	fastqcTask := plan.TaskByID["BeaverPDX/step1/fastqc_before/sample=sample-a"]
	if fastqcTask == nil {
		t.Fatal("missing sample-only PDX FastQC task")
	}
	if len(fastqcTask.Dimensions) != 1 || fastqcTask.Dimensions["sample"] != "sample-a" {
		t.Fatalf("PDX step1 should expand by sample only: %#v", fastqcTask.Dimensions)
	}
	if fastqcTask.Inputs["read1"][0] != filepath.Join("data", "sample-a_R1.fastq.gz") {
		t.Fatalf("unexpected PDX sample input: %#v", fastqcTask.Inputs["read1"])
	}
	if fastqcTask.Outputs["read1_data"] != filepath.Join("workflow", "fastqc_raw", "sample-a_R1_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected PDX pre-trim Fastqcx output %q", fastqcTask.Outputs["read1_data"])
	}
	fastqcAfterTask := plan.TaskByID["BeaverPDX/step1/fastqc_after/sample=sample-a"]
	if fastqcAfterTask == nil || fastqcAfterTask.Outputs["read2_data"] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_2_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected PDX post-trim Fastqcx task: %#v", fastqcAfterTask)
	}
	if plan.TaskByID["BeaverPDX/step1/step1_checker"] != nil {
		t.Fatal("step1 must terminate in per-sample PDX QC and trimming artifacts, not a marker-only checker task")
	}
	if fastqcAfterTask.Outputs["read1_data"] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_1_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected PDX post-trim terminal output %q", fastqcAfterTask.Outputs["read1_data"])
	}
}
