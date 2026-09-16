package otter

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadBeaverBSStep1Fixture(t *testing.T) {
	context, err := LoadLegacy(repositoryPath(t, "testdata", "configs", "beaverbs-step1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if context.Workflow.Mode != "RRBS" || context.Workflow.WorkflowName != "BeaverBS" {
		t.Fatalf("unexpected workflow context: %#v", context.Workflow)
	}
	if len(context.Samples) != 2 {
		t.Fatalf("expected two samples, got %d", len(context.Samples))
	}
	firstSample := context.Samples[0]
	if firstSample.ID != "sample-a" || firstSample.Adapter1 != "AGATCGGAAGAGC" || firstSample.Adapter2 != "AGATCGGAAGAGC" {
		t.Fatalf("unexpected first sample context: %#v", firstSample)
	}
	secondSample := context.Samples[1]
	if secondSample.ID != "sample-b" || secondSample.Adapter1 != "NO_ADAPTER_CAL_USE_DEFAULT" || secondSample.Adapter2 != "NO_ADAPTER_CAL_USE_DEFAULT" {
		t.Fatalf("unexpected second sample context: %#v", secondSample)
	}
	if len(context.Species) != 1 || context.Species[0].Name != "human" {
		t.Fatalf("unexpected species context: %#v", context.Species)
	}
	if context.Species[0].GenomeIndex == "" {
		t.Fatal("expected normalized genome index path")
	}
	reference := context.Raw["reference"].(map[string]any)
	if reference["graft_fasta"] != context.Species[0].GenomeFasta || reference["graft_index"] != context.Species[0].GenomeIndex {
		t.Fatalf("unexpected canonical graft reference: %#v", reference)
	}
}

func TestLoadBeaverPDXReferences(t *testing.T) {
	context, err := LoadLegacy(repositoryPath(t, "fixtures", "BeaverPDX", "step2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(context.Species) != 2 {
		t.Fatalf("expected graft and host species, got %#v", context.Species)
	}
	reference := context.Raw["reference"].(map[string]any)
	if reference["graft_fasta"] != context.Species[0].GenomeFasta || reference["graft_index"] != context.Species[0].GenomeIndex {
		t.Fatalf("unexpected canonical graft reference: %#v", reference)
	}
	if reference["host_fasta"] != context.Species[1].GenomeFasta || reference["host_index"] != context.Species[1].GenomeIndex {
		t.Fatalf("unexpected canonical host reference: %#v", reference)
	}
}

func TestBismarkExecutionGenomeDirectoryAppendsBuildLayout(t *testing.T) {
	if actual := bismarkExecutionGenomeDirectory("/refs/hg19/indexes/bismark"); actual != "/refs/hg19/indexes/bismark/genome" {
		t.Fatalf("unexpected Bismark execution root %q", actual)
	}
	if actual := bismarkExecutionGenomeDirectory(""); actual != "" {
		t.Fatalf("empty Bismark index root should remain empty, got %q", actual)
	}
}

func TestLoadNormalizesWindowsStyleRuntimePaths(t *testing.T) {
	temporaryDirectory := t.TempDir()
	configurationPath := filepath.Join(temporaryDirectory, "config.yaml")
	configuration := `SIDs: [sample-a]
mode: RRBS
species: human
input:
  fastq_dir: project\\data
output:
  raw_dir: project\\data
  trim_dir: project\\workflow\\trim
directories:
  qc:
    before: project\\workflow\\fastqc_raw
    after: project\\workflow\\fastqc_clean
  sid_log: project\\workflow\\log
workflow:
  mode: RRBS
  species:
    primary: human
reference:
  genome_index: [references\\human]
`
	if err := os.WriteFile(configurationPath, []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
	context, err := LoadLegacy(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	output := context.Raw["output"].(map[string]any)
	if output["trim_dir"] != filepath.Join("project", "workflow", "trim") {
		t.Fatalf("unexpected normalized trim path %q", output["trim_dir"])
	}
	directories := context.Raw["directories"].(map[string]any)
	qualityControl := directories["qc"].(map[string]any)
	if qualityControl["before"] != filepath.Join("project", "workflow", "fastqc_raw") {
		t.Fatalf("unexpected normalized QC path %q", qualityControl["before"])
	}
	if strings.Contains(context.Species[0].GenomeIndex, "\\") {
		t.Fatalf("species genome index was not normalized: %q", context.Species[0].GenomeIndex)
	}
}

func repositoryPath(t *testing.T, pathSegments ...string) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve current test file")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
	return filepath.Join(append([]string{repositoryRoot}, pathSegments...)...)
}
