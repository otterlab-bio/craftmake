package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestBeaverPDXPublishStagesGraftBAMPairsAndScientificArtifacts(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverPDX", "publish.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if workflow.On.Otter.Phase != "publish" {
		t.Fatalf("unexpected publish trigger: %#v", workflow.On.Otter)
	}
	publishJob := workflow.Jobs["publish_artifacts"]
	expectedInputs := map[string]string{
		"filtered_bam_directory": "${{ config.directories.bsmap.main }}/Filtered_bams",
		"methrix_data":           "${{ config.directories.methylation_call }}/methrixh5/methrix_data.h5",
		"bismark_summary":        "${{ config.directories.bsmap.main }}/${{ config.workflow.species.graft }}/bismark_summary_report.html",
		"qc_summary":             "${{ config.directories.qc_summary }}/qc_summary.xlsx",
	}
	if len(publishJob.Inputs) != len(expectedInputs) {
		t.Fatalf("unexpected PDX publish inputs: %#v", publishJob.Inputs)
	}
	for inputName, expectedPath := range expectedInputs {
		if publishJob.Inputs[inputName] != expectedPath {
			t.Fatalf("publish input %q = %q, want %q", inputName, publishJob.Inputs[inputName], expectedPath)
		}
	}
	for inputName, inputPath := range publishJob.Inputs {
		if strings.Contains(inputName, "success_marker") || strings.Contains(inputPath, "success_marker") {
			t.Fatalf("publish must not depend on a success marker: %#v", publishJob.Inputs)
		}
	}
	if publishJob.Outputs["classification_summary"] != "${{ paths.results }}/pdx/graft/classification.tsv" ||
		publishJob.Outputs["methrix_data"] != "${{ paths.results }}/methylation/methrix_data.h5" ||
		publishJob.Outputs["manifest"] != "${{ paths.results }}/artifacts.json" {
		t.Fatalf("publish outputs must remain in immutable results root: %#v", publishJob.Outputs)
	}
	command := publishJob.Steps[0].Run
	for _, expectedFragment := range []string{
		"Missing non-empty filtered BAM/BAI pair",
		".publish-staging",
		"mktemp -d",
		"existing PDX publication differs from the staged retry payload",
		"samtools quickcheck -v",
		"samtools view -c -F 4",
		`"id": f"graft-alignment-bam-{artifact_index:04d}"`,
		`"id": f"graft-alignment-bai-{artifact_index:04d}"`,
		`"id": "graft-classification-summary"`,
		`"id": "methylation-matrix"`,
		"artifact publish '${{ paths.config }}'",
		"artifact verify '${{ paths.config }}'",
	} {
		if !strings.Contains(command, expectedFragment) {
			t.Fatalf("PDX publish contract is missing %q: %q", expectedFragment, command)
		}
	}
}
