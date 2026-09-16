package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestBeaverRNAPublishRequiresMatricesAndUsesImmutablePublisher(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverRNA", "publish.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if workflow.On.Otter.Phase != "publish" || len(workflow.On.Otter.Modes) != 1 || workflow.On.Otter.Modes[0] != "RNASEQ" {
		t.Fatalf("unexpected publish trigger: %#v", workflow.On.Otter)
	}
	publishJob := workflow.Jobs["publish_artifacts"]
	expectedInputs := map[string]string{
		"count_matrix":      "${{ config.directories.beta_matrix }}/matrix_count.txt",
		"normalized_matrix": "${{ config.directories.beta_matrix }}/matrix_norm.txt",
		"qc_summary":        "${{ config.directories.qc_summary }}/qc_summary.xlsx",
		"splicing_outcome":  "${{ config.directories.bsmap.main }}/RNASplicing/splicing-outcome.json",
	}
	if len(publishJob.Inputs) != len(expectedInputs) {
		t.Fatalf("unexpected publish inputs: %#v", publishJob.Inputs)
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
	if publishJob.Outputs["manifest"] != "${{ paths.results }}/artifacts.json" {
		t.Fatalf("publish manifest must use immutable results root: %#v", publishJob.Outputs)
	}
	command := publishJob.Steps[0].Run
	for _, expectedArtifactID := range []string{
		"expression-count-matrix",
		"expression-normalized-matrix",
		"qc-summary",
	} {
		if !strings.Contains(command, `"id": "`+expectedArtifactID+`"`) {
			t.Fatalf("publish declaration does not include %q: %q", expectedArtifactID, command)
		}
	}
	for _, requiredFragment := range []string{
		`"comparison": {"tier": "structural", "comparator": "rna-splicing-outcome/v1"}`,
		".publish-staging",
		"mktemp -d",
		"existing splicing publication differs from the staged retry payload",
		"artifact publish '${{ paths.config }}'",
		"artifact verify '${{ paths.config }}'",
	} {
		if !strings.Contains(command, requiredFragment) {
			t.Fatalf("publish must use durable staged publication fragment %q: %q", requiredFragment, command)
		}
	}
	if !strings.Contains(command, "artifact publish '${{ paths.config }}'") ||
		!strings.Contains(command, "artifact verify '${{ paths.config }}'") {
		t.Fatalf("publish must invoke the immutable artifact publisher and verifier: %q", command)
	}
}
