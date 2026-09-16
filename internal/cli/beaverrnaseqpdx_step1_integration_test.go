package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeaverRNASEQPDXStep1RunsLocallyAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverBSTools(t, toolDirectory)
	writeBeaverRNASEQPDXStep1ProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "step1.yaml")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(os.Environ(), "PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "12",
		"--max-memory", "16G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 6") {
		t.Fatalf("unexpected first BeaverRNASEQPDX step1 status:\n%s", firstStatus)
	}

	for _, expectedOutput := range []string{
		"workflow/fastqc_raw/sample-a_R1_fastqcx/fastqc_data.txt",
		"workflow/fastqc_raw/sample-b_R2_fastqcx/fastqc_data.txt",
		"workflow/trim/sample-a_val_1.fq.gz",
		"workflow/trim/sample-b_R2.fastq.gz_trimming_report.txt",
		"workflow/fastqc_clean/sample-a_val_1_fastqcx/fastqc_data.txt",
		"workflow/fastqc_clean/sample-b_val_2_fastqcx/fastqc_data.txt",
	} {
		if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
			t.Fatalf("expected BeaverRNASEQPDX output %q: %v", expectedOutput, err)
		}
	}
	if _, err := os.Stat(filepath.Join(projectDirectory, "workflow", "log", "step1_success.txt")); !os.IsNotExist(err) {
		t.Fatalf("step1 must not generate a marker-only success file, stat error=%v", err)
	}

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "12",
		"--max-memory", "16G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 6") {
		t.Fatalf("unexpected cached BeaverRNASEQPDX step1 status:\n%s", secondStatus)
	}
}

func writeBeaverRNASEQPDXStep1ProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	dataDirectory := filepath.Join(projectDirectory, "data")
	if err := os.MkdirAll(dataDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sampleID := range []string{"sample-a", "sample-b"} {
		for _, readID := range []string{"R1", "R2"} {
			fastqPath := filepath.Join(dataDirectory, sampleID+"_"+readID+".fastq.gz")
			if err := os.WriteFile(fastqPath, []byte("@read\nACGT\n+\n!!!!\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	configuration := `SIDs: [sample-a, sample-b]
mode: RNASEQ
species1: human
species2: mouse
input:
  fastq_dir: data
  suffix: _R1.fastq.gz
  suffix2: _R2.fastq.gz
output:
  raw_dir: data
  trim_dir: workflow/trim
  workflow_dir: workflow
  analysis_dir: analysis
  log_dir: workflow/log
directories:
  work: workflow
  qc:
    before: workflow/fastqc_raw
    after: workflow/fastqc_clean
  sid_log: workflow/log
workflow:
  mode: RNASEQ
  jobid: beaverrnaseqpdx-step1-integration
  userid: integration
  species:
    name: [human, mouse]
    primary: human
    secondary: mouse
    graft: human
    host: mouse
  adapters:
    error: 0.2
    seq1: [AGATCGGAAGAGC, NO_ADAPTER_CAL_USE_DEFAULT]
    seq2: [AGATCGGAAGAGC, NO_ADAPTER_CAL_USE_DEFAULT]
  alignment:
    c1: 7
    c2: 9
    t1: 3
    t2: 4
metadata:
  sample_ids: [sample-a, sample-b]
  pdx_pipeline: "true"
reference:
  files:
    fasta: [references/human.fasta, references/mouse.fasta]
  indices:
    genome: [references/star-human, references/star-mouse]
  rnaseq:
    gtf: [references/human.gtf, references/mouse.gtf]
    ref: [references/star-human, references/star-mouse]
`
	if err := os.WriteFile(filepath.Join(projectDirectory, "config.yaml"), []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
}
