package compiler_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestBeaverBSPublishRequiresFinalArtifactsAndUsesImmutablePublisher(t *testing.T) {
	repositoryRoot := repositoryRootForTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverBS", "publish.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if workflow.On.Otter.Phase != "publish" {
		t.Fatalf("unexpected publish phase: %#v", workflow.On.Otter)
	}
	publishJob := workflow.Jobs["publish_artifacts"]
	if len(publishJob.Inputs) != 3 {
		t.Fatalf("publish must require final scientific outputs, got %#v", publishJob.Inputs)
	}
	forbiddenSuccessMarker := "success_marker"
	for inputName, inputPath := range publishJob.Inputs {
		if strings.Contains(inputName, forbiddenSuccessMarker) || strings.Contains(inputPath, forbiddenSuccessMarker) {
			t.Fatalf("publish must not depend on a success marker: %#v", publishJob.Inputs)
		}
	}
	if publishJob.Outputs["methrix_data"] != "${{ paths.results }}/methylation/methrix_data.h5" ||
		publishJob.Outputs["bismark_summary"] != "${{ paths.results }}/methylation/bismark_summary_report.html" ||
		publishJob.Outputs["manifest"] != "${{ paths.results }}/artifacts.json" {
		t.Fatalf("publish outputs must use immutable results root: %#v", publishJob.Outputs)
	}
	command := publishJob.Steps[0].Run
	for _, expectedFragment := range []string{
		".publish-staging",
		"mktemp -d",
		"cp -- '${{ inputs.methrix_data }}' \"$staging_methylation_directory/methrix_data.h5\"",
		"cp -- '${{ inputs.bismark_summary }}' \"$staging_methylation_directory/bismark_summary_report.html\"",
		"existing methylation publication differs from the staged retry payload",
		`"id": "methylation-matrix"`,
		`"id": "bismark-summary"`,
		"artifact publish '${{ paths.config }}'",
		"artifact verify '${{ paths.config }}'",
	} {
		if !strings.Contains(command, expectedFragment) {
			t.Fatalf("BeaverBS publish contract is missing %q: %q", expectedFragment, command)
		}
	}
}
