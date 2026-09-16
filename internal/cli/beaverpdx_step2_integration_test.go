package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeaverPDXStep2RunsLocallyInSpeciesBatchesAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverBSStep2Tools(t, toolDirectory)
	writeBeaverPDXStep2ProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverPDX", "step2.yaml")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(os.Environ(), "PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "10",
		"--max-cores", "80",
		"--max-memory", "320G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 12") {
		t.Fatalf("unexpected first BeaverPDX step2 status:\n%s", firstStatus)
	}

	for _, sampleID := range []string{"sample-a", "sample-b"} {
		for _, speciesName := range []string{"human", "mouse"} {
			for _, expectedOutput := range []string{
				filepath.Join("workflow", "bsmap", sampleID+"_"+speciesName+".bam"),
				filepath.Join("workflow", "bsmap", sampleID+"_"+speciesName+".bam.bai"),
				filepath.Join("workflow", "QC", "qualimap", sampleID+"_"+speciesName, "qualimapReport.html"),
				filepath.Join("workflow", "QC", "qualimap", sampleID+"_"+speciesName, "report.pdf"),
				filepath.Join("workflow", "QC", "GCbias", sampleID+"_"+speciesName, "gc_bias_metrics.txt"),
				filepath.Join("workflow", "QC", "GCbias", sampleID+"_"+speciesName, "gc_bias_metrics.pdf"),
				filepath.Join("workflow", "QC", "GCbias", sampleID+"_"+speciesName, "summary_metrics.txt"),
			} {
				if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
					t.Fatalf("expected BeaverPDX step2 output %q: %v", expectedOutput, err)
				}
			}
		}
	}

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "10",
		"--max-cores", "80",
		"--max-memory", "320G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 12") {
		t.Fatalf("unexpected cached BeaverPDX step2 status:\n%s", secondStatus)
	}
}

func writeBeaverPDXStep2ProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	for _, relativePath := range []string{
		"workflow/trim/sample-a_val_1.fq.gz",
		"workflow/trim/sample-a_val_2.fq.gz",
		"workflow/trim/sample-b_val_1.fq.gz",
		"workflow/trim/sample-b_val_2.fq.gz",
		"references/human.fasta",
		"references/mouse.fasta",
		"references/bismark-human",
		"references/bismark-mouse",
	} {
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
  bsmap:
    main: workflow/bsmap
  qualimap: workflow/QC/qualimap
  qc:
    main: workflow/QC
  sid_log: workflow/log
workflow:
  mode: RRBS
  jobid: beaverpdx-step2-integration
  userid: integration
  species:
    name: [human, mouse]
    primary: human
    secondary: mouse
    graft: human
    host: mouse
metadata:
  sample_ids: [sample-a, sample-b]
reference:
  files:
    fasta: [references/human.fasta, references/mouse.fasta]
  indices:
    genome: [references/bismark-human, references/bismark-mouse]
`
	if err := os.WriteFile(filepath.Join(projectDirectory, "config.yaml"), []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
}
