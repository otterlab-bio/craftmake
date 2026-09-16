package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverRNASEQPDXStep1Fixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "step1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverrnaseqpdx-step1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if context.Workflow.Mode != "RNASEQ" || context.Workflow.WorkflowName != "BeaverRNASEQPDX" || !context.Workflow.PDXMode {
		t.Fatalf("unexpected BeaverRNASEQPDX context: %#v", context.Workflow)
	}
	if len(context.Species) != 2 || context.Species[0].Name != "human" || context.Species[1].Name != "mouse" {
		t.Fatalf("unexpected RNA-seq PDX species contexts: %#v", context.Species)
	}
	reference := context.Raw["reference"].(map[string]any)
	rnaReference := reference["rnaseq"].(map[string]any)
	if rnaReference["graft_gtf"] != context.Species[0].RNASeqGTF || rnaReference["host_gtf"] != context.Species[1].RNASeqGTF {
		t.Fatalf("unexpected canonical PDX RNA annotations: %#v", rnaReference)
	}
	if rnaReference["graft_reference"] != context.Species[0].RNASeqReference || rnaReference["host_reference"] != context.Species[1].RNASeqReference {
		t.Fatalf("unexpected canonical PDX RNA references: %#v", rnaReference)
	}

	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 6 || len(plan.Submissions) != 6 {
		t.Fatalf("expected six tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	fastqcTask := plan.TaskByID["BeaverRNASEQPDX/step1/fastqc_before/sample=sample-a"]
	if fastqcTask == nil || len(fastqcTask.Dimensions) != 1 || fastqcTask.Dimensions["sample"] != "sample-a" {
		t.Fatalf("RNA-seq PDX step1 should expand by sample only: %#v", fastqcTask)
	}
	if fastqcTask.Outputs["read1_data"] != filepath.Join("workflow", "fastqc_raw", "sample-a_R1_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected RNA-seq PDX pre-trim Fastqcx output %q", fastqcTask.Outputs["read1_data"])
	}
	fastqcAfterTask := plan.TaskByID["BeaverRNASEQPDX/step1/fastqc_after/sample=sample-a"]
	if fastqcAfterTask == nil || fastqcAfterTask.Outputs["read2_data"] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_2_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected RNA-seq PDX post-trim Fastqcx task: %#v", fastqcAfterTask)
	}
	trimTask := plan.TaskByID["BeaverRNASEQPDX/step1/trim_reads/sample=sample-a"]
	if trimTask == nil || !strings.Contains(trimTask.Steps[0].Command, "--three_prime_clip_R2 '4'") {
		t.Fatalf("unexpected RNA-seq PDX trim task: %#v", trimTask)
	}
	if plan.TaskByID["BeaverRNASEQPDX/step1/step1_checker"] != nil {
		t.Fatal("step1 must terminate in per-sample RNA-seq PDX QC and trimming artifacts, not a marker-only checker task")
	}
	if fastqcAfterTask.Outputs["read1_data"] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_1_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected RNA-seq PDX post-trim terminal output %q", fastqcAfterTask.Outputs["read1_data"])
	}
}
