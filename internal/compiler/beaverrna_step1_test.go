package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverRNAStep1Fixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNA", "step1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverrna-step1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if context.Workflow.Mode != "RNASEQ" || context.Workflow.WorkflowName != "BeaverRNA" || context.Workflow.PDXMode {
		t.Fatalf("unexpected BeaverRNA context: %#v", context.Workflow)
	}
	if len(context.Species) != 1 || context.Species[0].Name != "human" {
		t.Fatalf("unexpected BeaverRNA species contexts: %#v", context.Species)
	}
	reference := context.Raw["reference"].(map[string]any)
	rnaReference := reference["rnaseq"].(map[string]any)
	if rnaReference["primary_gtf"] != context.Species[0].RNASeqGTF || rnaReference["primary_reference"] != context.Species[0].RNASeqReference {
		t.Fatalf("unexpected canonical primary RNA reference: %#v", rnaReference)
	}

	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 6 || len(plan.Submissions) != 6 {
		t.Fatalf("expected six tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	fastqcBeforeTask := plan.TaskByID["BeaverRNA/step1/fastqc_before/sample=sample-a"]
	if fastqcBeforeTask == nil || len(fastqcBeforeTask.Dependencies) != 0 {
		t.Fatalf("unexpected BeaverRNA pre-trim FastQC task: %#v", fastqcBeforeTask)
	}
	if fastqcBeforeTask.Outputs["read1_data"] != filepath.Join("workflow", "fastqc_raw", "sample-a_R1_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected BeaverRNA pre-trim Fastqcx output %q", fastqcBeforeTask.Outputs["read1_data"])
	}
	trimTask := plan.TaskByID["BeaverRNA/step1/trim_reads/sample=sample-a"]
	if trimTask == nil {
		t.Fatal("missing BeaverRNA sample-a trim task")
	}
	trimCommand := trimTask.Steps[0].Command
	for _, expectedArgument := range []string{"trim_galore", "--clip_R1 '7'", "--clip_R2 '9'", "--three_prime_clip_R1 '3'", "--three_prime_clip_R2 '4'"} {
		if !strings.Contains(trimCommand, expectedArgument) {
			t.Fatalf("trim command does not contain %q:\n%s", expectedArgument, trimCommand)
		}
	}
	fastqcAfterTask := plan.TaskByID["BeaverRNA/step1/fastqc_after/sample=sample-a"]
	if fastqcAfterTask == nil || !containsTaskID(fastqcAfterTask.Dependencies, trimTask.ID) {
		t.Fatalf("post-trim FastQC should depend on sample-a trimming: %#v", fastqcAfterTask)
	}
	if fastqcAfterTask.Outputs["read2_data"] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_2_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected BeaverRNA post-trim Fastqcx output %q", fastqcAfterTask.Outputs["read2_data"])
	}
	if plan.TaskByID["BeaverRNA/step1/step1_checker"] != nil {
		t.Fatal("step1 must terminate in per-sample RNA QC and trimming artifacts, not a marker-only checker task")
	}
	if fastqcAfterTask.Outputs["read1_data"] != filepath.Join("workflow", "fastqc_clean", "sample-a_val_1_fastqcx", "fastqc_data.txt") {
		t.Fatalf("unexpected RNA post-trim terminal output %q", fastqcAfterTask.Outputs["read1_data"])
	}
}
