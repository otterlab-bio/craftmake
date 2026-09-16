package otter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReferenceBuildCreatesDedicatedImmutableContext(t *testing.T) {
	configurationPath := repositoryPath(t, "testdata", "configs", "reference-build.yaml")
	context, err := LoadReferenceBuild(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	if context.Workflow.WorkflowName != "ReferenceBuild" || context.Workflow.Mode != "REFERENCE" {
		t.Fatalf("unexpected reference build workflow context: %#v", context.Workflow)
	}
	if context.Workflow.Backend != "slurm" || context.Workflow.Executor != "craftmake" {
		t.Fatalf("reference build must use Craftmake on Slurm: %#v", context.Workflow)
	}
	if context.Workflow.JobID != "reference-mm10-canary-20260729T000000Z" {
		t.Fatalf("unexpected reference build identity: %q", context.Workflow.JobID)
	}
	if context.Execution.Slurm.Partition != "" {
		t.Fatalf("unexpected default Slurm partition: %#v", context.Execution.Slurm)
	}
	if got := context.Raw["reference_build"].(map[string]any)["reference_id"]; got != "mm10-canary" {
		t.Fatalf("unexpected reference identity: %q", got)
	}
}

func TestLoadReferenceBuildUsesConfiguredBackendAndPartition(t *testing.T) {
	configurationData, err := os.ReadFile(repositoryPath(t, "testdata", "configs", "reference-build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	configuredData := strings.Replace(string(configurationData), "  run_id: reference-mm10-canary-20260729T000000Z\n", "  run_id: reference-mm10-canary-20260729T000000Z\n  backend: local\n  partition: local-test\n", 1)
	configurationPath := filepath.Join(t.TempDir(), "reference-build.yaml")
	if err := os.WriteFile(configurationPath, []byte(configuredData), 0o644); err != nil {
		t.Fatal(err)
	}

	context, err := LoadReferenceBuild(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	if context.Workflow.Backend != "local" {
		t.Fatalf("configured reference build backend was not preserved: %q", context.Workflow.Backend)
	}
	if context.Execution.Slurm.Partition != "local-test" {
		t.Fatalf("configured reference build partition was not preserved: %q", context.Execution.Slurm.Partition)
	}
}

func TestLoadReferenceBuildRejectsUnsupportedChecksumAlgorithm(t *testing.T) {
	configurationData, err := os.ReadFile(repositoryPath(t, "testdata", "configs", "reference-build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	invalidConfiguration := strings.Replace(string(configurationData), "fasta_checksum_algorithm: bsd-sum", "fasta_checksum_algorithm: sha1", 1)
	configurationPath := filepath.Join(t.TempDir(), "reference-build.yaml")
	if err := os.WriteFile(configurationPath, []byte(invalidConfiguration), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadReferenceBuild(configurationPath)
	if err == nil || !strings.Contains(err.Error(), "unsupported reference_build.fasta_checksum_algorithm") {
		t.Fatalf("expected checksum algorithm validation error, got %v", err)
	}
}

func TestLoadReferenceBuildRejectsNonPositiveIndexBuildThreads(t *testing.T) {
	configurationData, err := os.ReadFile(repositoryPath(t, "testdata", "configs", "reference-build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	invalidConfiguration := strings.Replace(string(configurationData), "index_build_threads: 16", "index_build_threads: 0", 1)
	configurationPath := filepath.Join(t.TempDir(), "reference-build.yaml")
	if err := os.WriteFile(configurationPath, []byte(invalidConfiguration), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadReferenceBuild(configurationPath)
	if err == nil || !strings.Contains(err.Error(), "positive reference_build.index_build_threads") {
		t.Fatalf("expected index build thread validation error, got %v", err)
	}
}

func TestLoadReferenceBuildRejectsRelativeToolPath(t *testing.T) {
	configurationData, err := os.ReadFile(repositoryPath(t, "testdata", "configs", "reference-build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	invalidConfiguration := strings.Replace(string(configurationData), "otter_binary: /usr/local/bin/otter", "otter_binary: otter", 1)
	configurationPath := filepath.Join(t.TempDir(), "reference-build.yaml")
	if err := os.WriteFile(configurationPath, []byte(invalidConfiguration), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadReferenceBuild(configurationPath)
	if err == nil || !strings.Contains(err.Error(), "absolute reference_build.otter_binary") {
		t.Fatalf("expected absolute tool path validation error, got %v", err)
	}
}
