package cli_test

import (
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeaverBSStep1WithRealTools(t *testing.T) {
	if os.Getenv("CRAFTMAKE_REAL_TOOLS_SMOKE") != "1" {
		t.Skip("set CRAFTMAKE_REAL_TOOLS_SMOKE=1 to run the real fastqcx and trim_galore smoke test")
	}
	for _, executableName := range []string{"enva", "fastqcx", "trim_galore"} {
		if _, err := exec.LookPath(executableName); err != nil {
			t.Skipf("required real tool %q is unavailable: %v", executableName, err)
		}
	}

	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCommand := exec.Command("go", "build", "-o", binaryPath, "./cmd/craftmake")
	buildCommand.Dir = repositoryRoot
	if buildOutput, err := buildCommand.CombinedOutput(); err != nil {
		t.Fatalf("build craftmake: %v\n%s", err, buildOutput)
	}

	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeRealToolBeaverBSFixture(t, projectDirectory)
	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step1.yaml")
	runOutput := runCraftmake(t, binaryPath, os.Environ(),
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "2",
		"--max-cores", "8",
		"--max-memory", "16G",
	)
	runID := outputValue(t, runOutput, "run_id")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	statusOutput := runCraftmake(t, binaryPath, os.Environ(), "status", "--state", statePath, "--run", runID)
	if expectedStatus := "status: succeeded"; !containsText(statusOutput, expectedStatus) {
		t.Fatalf("real-tool smoke test did not succeed:\n%s", statusOutput)
	}

	for _, expectedOutput := range []string{
		"workflow/fastqc_raw/smoke_R1_fastqcx/fastqc_data.txt",
		"workflow/fastqc_raw/smoke_R2_fastqcx/fastqc_data.txt",
		"workflow/trim/smoke_val_1.fq.gz",
		"workflow/trim/smoke_val_2.fq.gz",
		"workflow/trim/smoke_R1.fastq.gz_trimming_report.txt",
		"workflow/trim/smoke_R2.fastq.gz_trimming_report.txt",
		"workflow/fastqc_clean/smoke_val_1_fastqcx/fastqc_data.txt",
		"workflow/fastqc_clean/smoke_val_2_fastqcx/fastqc_data.txt",
		"workflow/log/step1_success.txt",
	} {
		if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
			t.Fatalf("real-tool output contract mismatch for %q: %v", expectedOutput, err)
		}
	}
}

func writeRealToolBeaverBSFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	dataDirectory := filepath.Join(projectDirectory, "data")
	if err := os.MkdirAll(dataDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGzipFASTQ(t, filepath.Join(dataDirectory, "smoke_R1.fastq.gz"), "smoke-read", "ACGTACGTACGTACGTACGT")
	writeGzipFASTQ(t, filepath.Join(dataDirectory, "smoke_R2.fastq.gz"), "smoke-read", "TGCATGCATGCATGCATGCA")

	configuration := `SIDs: [smoke]
mode: RRBS
species: human
input:
  fastq_dir: data
  suffix: _R1.fastq.gz
  suffix2: _R2.fastq.gz
output:
  raw_dir: data
  trim_dir: workflow/trim
  workflow_dir: workflow
  analysis_dir: analysis
directories:
  qc:
    before: workflow/fastqc_raw
    after: workflow/fastqc_clean
  sid_log: workflow/log
workflow:
  mode: RRBS
  jobid: real-tools-smoke
  userid: integration
  species:
    primary: human
  adapters:
    error: 0.2
    seq1: [NO_ADAPTER_CAL_USE_DEFAULT]
    seq2: [NO_ADAPTER_CAL_USE_DEFAULT]
  alignment:
    c1: 0
    c2: 0
    t1: 0
    t2: 0
metadata:
  sample_ids: [smoke]
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

func writeGzipFASTQ(t *testing.T, path, readPrefix, sequence string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	quality := make([]byte, len(sequence))
	for qualityIndex := range quality {
		quality[qualityIndex] = 'I'
	}
	for readIndex := 1; readIndex <= 8; readIndex++ {
		if _, err := fmt.Fprintf(gzipWriter, "@%s-%d\n%s\n+\n%s\n", readPrefix, readIndex, sequence, quality); err != nil {
			t.Fatal(err)
		}
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func containsText(value, expected string) bool {
	return strings.Contains(value, expected)
}
