package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestCompileBeaverRNASEQPDXStep2CheckFixture(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "step2-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	context, err := otter.LoadLegacy(filepath.Join(repositoryRoot, "testdata", "configs", "beaverrnaseqpdx-step2-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 5 || len(plan.Submissions) != 5 {
		t.Fatalf("expected five tasks and submissions, got tasks=%d submissions=%d", len(plan.Tasks), len(plan.Submissions))
	}

	artifactTaskID := "BeaverRNASEQPDX/step2-check/sample_artifacts/sample=sample-a/species=mouse"
	artifactTask := plan.TaskByID[artifactTaskID]
	if artifactTask == nil || len(artifactTask.Inputs) != 4 {
		t.Fatalf("unexpected RNA-seq PDX artifact task: %#v", artifactTask)
	}
	for _, requiredFragment := range []string{
		"otter.sample-artifacts-validation/v1",
		"sha256sum",
		"regular non-symlink file",
		"mktemp \"${output_path}.tmp.XXXXXX\"",
	} {
		if !strings.Contains(artifactTask.Steps[0].Command, requiredFragment) {
			t.Fatalf("sample artifact validation manifest command does not contain %q:\n%s", requiredFragment, artifactTask.Steps[0].Command)
		}
	}

	xenofilxTask := plan.TaskByID["BeaverRNASEQPDX/step2-check/xenofilx"]
	if xenofilxTask == nil || len(xenofilxTask.Inputs["validated_artifacts"]) != 4 || len(xenofilxTask.Dependencies) != 4 {
		t.Fatalf("unexpected RNA-seq Xenofilx aggregation: %#v", xenofilxTask)
	}
	xenofilxCommand := xenofilxTask.Steps[0].Command
	if !strings.Contains(xenofilxCommand, "for graft_bam") || !strings.Contains(xenofilxCommand, `xenofilx_command+=(--graft "$graft_bam" --output-names`) || !strings.Contains(xenofilxCommand, "--mm-threshold 4") || !strings.Contains(xenofilxCommand, "--graft-ref '") || !strings.Contains(xenofilxCommand, "--host-ref '") || !strings.Contains(xenofilxCommand, "--recalculate-nm") || !strings.Contains(xenofilxCommand, "xenofilx_exit_code") || !strings.Contains(xenofilxCommand, "temporary_filtered_directory") {
		t.Fatalf("unexpected RNA-seq Xenofilx command: %q", xenofilxCommand)
	}
	if !strings.Contains(xenofilxCommand, "--threads '4'") || !strings.Contains(xenofilxCommand, "temporary_filtered_bai") {
		t.Fatalf("RNA-seq Xenofilx must require and preserve Xenofilx-provided BAI: %q", xenofilxCommand)
	}
	if xenofilxTask.Outputs["validation_manifest"] != filepath.Join("workflow", "bsmap", "Filtered_bams", "filtered-bam-validation.json") {
		t.Fatalf("unexpected RNA-seq filtered BAM validation manifest: %#v", xenofilxTask.Outputs)
	}
	for _, requiredFragment := range []string{
		"otter.filtered-bam-validation/v1",
		"sha256sum",
		"regular non-symlink files",
		"mktemp \"${manifest_path}.tmp.XXXXXX\"",
	} {
		if !strings.Contains(xenofilxCommand, requiredFragment) {
			t.Fatalf("filtered BAM validation manifest command does not contain %q:\n%s", requiredFragment, xenofilxCommand)
		}
	}
	if plan.TaskByID["BeaverRNASEQPDX/step2-check/filtered_artifacts/sample=sample-a"] != nil {
		t.Fatal("filtered BAM validation must be represented by Xenofilx's content-bearing aggregate manifest")
	}
	if plan.TaskByID["BeaverRNASEQPDX/step2-check/step2_checker"] != nil {
		t.Fatal("step2-check must terminate in the filtered BAM validation manifest, not a marker-only checker")
	}
}
