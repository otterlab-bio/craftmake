package cli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeaverBSStep2CheckRunsLocallyAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverBSStep2CheckTools(t, toolDirectory)
	writeBeaverBSStep2CheckProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step2-check.yaml")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(os.Environ(), "PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "8",
		"--max-memory", "16G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 2") {
		t.Fatalf("unexpected first BeaverBS step2-check status:\n%s", firstStatus)
	}

	for _, expectedOutput := range []string{
		"workflow/log/step2-check/sample-a_human.ready",
		"workflow/log/step2-check/sample-b_human.ready",
	} {
		if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
			t.Fatalf("expected BeaverBS step2-check output %q: %v", expectedOutput, err)
		}
	}
	assertBeaverBSStep2ValidationManifest(t, projectDirectory, "sample-a")
	assertBeaverBSStep2ValidationManifest(t, projectDirectory, "sample-b")

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "8",
		"--max-memory", "16G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 2") {
		t.Fatalf("unexpected cached BeaverBS step2-check status:\n%s", secondStatus)
	}
}

func writeFakeBeaverBSStep2CheckTools(t *testing.T, toolDirectory string) {
	t.Helper()
	if err := os.MkdirAll(toolDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(toolDirectory, "enva"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "--quiet" ]; then shift; fi
if [ "${1:-}" != "run" ]; then exit 2; fi
shift 2
if [ "${1:-}" = "--" ]; then shift; fi
exec "$@"
`)
}

func writeBeaverBSStep2CheckProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	inputPaths := make([]string, 0, 14)
	for _, sampleID := range []string{"sample-a", "sample-b"} {
		inputPaths = append(inputPaths,
			"workflow/bsmap/"+sampleID+"_human.bam",
			"workflow/bsmap/"+sampleID+"_human.bam.bai",
			"workflow/QC/qualimap/"+sampleID+"_human/qualimapReport.html",
			"workflow/QC/qualimap/"+sampleID+"_human/report.pdf",
			"workflow/QC/GCbias/"+sampleID+"_human/gc_bias_metrics.txt",
			"workflow/QC/GCbias/"+sampleID+"_human/gc_bias_metrics.pdf",
			"workflow/QC/GCbias/"+sampleID+"_human/summary_metrics.txt",
		)
	}
	for _, relativePath := range inputPaths {
		path := filepath.Join(projectDirectory, relativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configuration := `SIDs: [sample-a, sample-b]
mode: RRBS
species: human
output:
  trim_dir: workflow/trim
  workflow_dir: workflow
  analysis_dir: analysis
  log_dir: workflow/log
directories:
  work: workflow
  bsmap:
    main: workflow/bsmap
  qualimap: workflow/QC/qualimap
  qc:
    main: workflow/QC
  qc_summary: workflow/QC/summary
  sid_log: workflow/log
workflow:
  mode: RRBS
  jobid: beaverbs-step2-check-integration
  userid: integration
  species:
    primary: human
    name: human
    graft: human
metadata:
  sample_ids: [sample-a, sample-b]
reference:
  files:
    fasta: [references/human.fasta]
  indices:
    genome: [references/bismark-human]
`
	if err := os.WriteFile(filepath.Join(projectDirectory, "config.yaml"), []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertBeaverBSStep2ValidationManifest(t *testing.T, projectDirectory string, sampleID string) {
	t.Helper()
	manifestPath := filepath.Join(projectDirectory, "workflow", "log", "step2-check", sampleID+"_human.ready")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read BeaverBS step2 validation manifest %q: %v", manifestPath, err)
	}

	var manifest sampleArtifactValidationManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("parse BeaverBS step2 validation manifest %q: %v\n%s", manifestPath, err, manifestData)
	}
	if manifest.SchemaVersion != "otter.sample-artifacts-validation/v1" ||
		manifest.Status != "validated" ||
		manifest.Workflow != "BeaverBS" ||
		manifest.Phase != "step2-check" ||
		manifest.SampleID != sampleID ||
		manifest.Dimensions["sample"] != sampleID ||
		manifest.Dimensions["species"] != "human" ||
		len(manifest.Dimensions) != 2 {
		t.Fatalf("unexpected BeaverBS step2 validation manifest identity: %#v", manifest)
	}

	expectedArtifacts := map[string]string{
		"bam":           filepath.Join("workflow", "bsmap", sampleID+"_human.bam"),
		"bam_index":     filepath.Join("workflow", "bsmap", sampleID+"_human.bam.bai"),
		"qualimap_html": filepath.Join("workflow", "QC", "qualimap", sampleID+"_human", "qualimapReport.html"),
		"qualimap_pdf":  filepath.Join("workflow", "QC", "qualimap", sampleID+"_human", "report.pdf"),
		"gc_metrics":    filepath.Join("workflow", "QC", "GCbias", sampleID+"_human", "gc_bias_metrics.txt"),
		"gc_chart":      filepath.Join("workflow", "QC", "GCbias", sampleID+"_human", "gc_bias_metrics.pdf"),
		"gc_summary":    filepath.Join("workflow", "QC", "GCbias", sampleID+"_human", "summary_metrics.txt"),
	}
	if len(manifest.Artifacts) != len(expectedArtifacts) {
		t.Fatalf("expected %d BeaverBS step2 validation artifacts, got %#v", len(expectedArtifacts), manifest.Artifacts)
	}
	for _, artifact := range manifest.Artifacts {
		expectedPath, knownArtifact := expectedArtifacts[artifact.ID]
		if !knownArtifact || artifact.Path != expectedPath || artifact.MediaType == "" || artifact.SizeBytes <= 0 {
			t.Fatalf("unexpected BeaverBS step2 validation artifact: %#v", artifact)
		}
		artifactData, err := os.ReadFile(filepath.Join(projectDirectory, artifact.Path))
		if err != nil {
			t.Fatalf("read validated BeaverBS step2 artifact %q: %v", artifact.Path, err)
		}
		digest := sha256.Sum256(artifactData)
		if artifact.SHA256 != hex.EncodeToString(digest[:]) || artifact.SizeBytes != int64(len(artifactData)) {
			t.Fatalf("BeaverBS step2 validation digest or size does not match %q", artifact.Path)
		}
		delete(expectedArtifacts, artifact.ID)
	}
	if len(expectedArtifacts) != 0 {
		t.Fatalf("BeaverBS step2 validation manifest omitted artifacts: %#v", expectedArtifacts)
	}
}
