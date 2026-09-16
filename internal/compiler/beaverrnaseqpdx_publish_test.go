package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestBeaverRNASEQPDXPublishStagesGraftBAMPairsAndExpressionArtifacts(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "publish.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if workflow.On.Otter.Phase != "publish" || len(workflow.On.Otter.Modes) != 1 || workflow.On.Otter.Modes[0] != "RNASEQ" {
		t.Fatalf("unexpected RNA-seq PDX publish trigger: %#v", workflow.On.Otter)
	}
	publishJob := workflow.Jobs["publish_artifacts"]
	expectedInputs := map[string]string{
		"filtered_bam_directory": "${{ config.directories.bsmap.main }}/Filtered_bams",
		"count_matrix":           "${{ config.directories.beta_matrix }}/matrix_count.txt",
		"normalized_matrix":      "${{ config.directories.beta_matrix }}/matrix_norm.txt",
		"qc_summary":             "${{ config.directories.qc_summary }}/qc_summary.xlsx",
		"splicing_outcome":       "${{ config.directories.bsmap.main }}/RNASplicing/splicing-outcome.json",
	}
	if len(publishJob.Inputs) != len(expectedInputs) {
		t.Fatalf("unexpected RNA-seq PDX publish inputs: %#v", publishJob.Inputs)
	}
	for inputName, expectedPath := range expectedInputs {
		if publishJob.Inputs[inputName] != expectedPath {
			t.Fatalf("publish input %q = %q, want %q", inputName, publishJob.Inputs[inputName], expectedPath)
		}
	}
	for inputName, inputPath := range publishJob.Inputs {
		if strings.Contains(inputName, "success_marker") || strings.Contains(inputPath, "success_marker") {
			t.Fatalf("publish must not declare a marker-only input: %#v", publishJob.Inputs)
		}
	}
	if publishJob.Outputs["classification_summary"] != "${{ paths.results }}/pdx/graft/classification.tsv" ||
		publishJob.Outputs["manifest"] != "${{ paths.results }}/artifacts.json" {
		t.Fatalf("publish outputs must remain in immutable results root: %#v", publishJob.Outputs)
	}
	command := publishJob.Steps[0].Run
	if strings.Count(command, `"comparator": "expression-count-matrix/v1"`) != 2 {
		t.Fatalf("count and normalized matrices must use the registered expression comparator: %q", command)
	}
	for _, expectedFragment := range []string{
		"Missing non-empty filtered BAM/BAI pair",
		".publish-staging",
		"mktemp -d",
		"existing RNA-PDX publication differs from the staged retry payload",
		"samtools quickcheck -v",
		"samtools view -c -F 4",
		`"id": f"graft-alignment-bam-{artifact_index:04d}"`,
		`"id": f"graft-alignment-bai-{artifact_index:04d}"`,
		`"id": "graft-classification-summary"`,
		`"id": "expression-count-matrix"`,
		`"id": "expression-normalized-matrix"`,
		`"id": "splicing-outcome"`,
		`"comparison": {"tier": "structural", "comparator": "rna-splicing-outcome/v1"}`,
		"produced RNA splicing outcome requires artifact paths",
		"artifact publish '${{ paths.config }}'",
		"artifact verify '${{ paths.config }}'",
	} {
		if !strings.Contains(command, expectedFragment) {
			t.Fatalf("RNA-seq PDX publish contract is missing %q: %q", expectedFragment, command)
		}
	}
}
