package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeaverRNASEQPDXStep2RunsLocallyInSpeciesBatchesAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverRNAStep2Tools(t, toolDirectory)
	writeBeaverRNASEQPDXStep2ProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "step2.yaml")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(os.Environ(), "PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "8",
		"--max-cores", "160",
		"--max-memory", "256G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 8") {
		t.Fatalf("unexpected first BeaverRNASEQPDX step2 status:\n%s", firstStatus)
	}

	for _, sampleID := range []string{"sample-a", "sample-b"} {
		for _, speciesName := range []string{"human", "mouse"} {
			for _, expectedOutput := range []string{
				filepath.Join("workflow", "bsmap", sampleID+"_"+speciesName+".bam"),
				filepath.Join("workflow", "bsmap", sampleID+"_"+speciesName+".bam.bai"),
				filepath.Join("workflow", "QC", "qualimap", sampleID+"_"+speciesName, "qualimapReport.html"),
				filepath.Join("workflow", "QC", "qualimap", sampleID+"_"+speciesName, "report.pdf"),
			} {
				if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
					t.Fatalf("expected BeaverRNASEQPDX step2 output %q: %v", expectedOutput, err)
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
		"--max-parallel", "8",
		"--max-cores", "160",
		"--max-memory", "256G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 8") {
		t.Fatalf("unexpected cached BeaverRNASEQPDX step2 status:\n%s", secondStatus)
	}
}

func writeBeaverRNASEQPDXStep2ProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	inputPaths := []string{
		"workflow/trim/sample-a_val_1.fq.gz",
		"workflow/trim/sample-a_val_2.fq.gz",
		"workflow/trim/sample-b_val_1.fq.gz",
		"workflow/trim/sample-b_val_2.fq.gz",
		"references/human.fasta",
		"references/mouse.fasta",
		"references/human.gtf",
		"references/mouse.gtf",
		"references/star-human/Genome",
		"references/star-mouse/Genome",
	}
	for _, relativePath := range inputPaths {
		fixturePath := filepath.Join(projectDirectory, relativePath)
		if err := os.MkdirAll(filepath.Dir(fixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath, []byte("fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configuration := `SIDs: [sample-a, sample-b]
mode: RNASEQ
species1: human
species2: mouse
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
  mode: RNASEQ
  jobid: beaverrnaseqpdx-step2-integration
  userid: integration
  species:
    name: [human, mouse]
    primary: human
    secondary: mouse
    graft: human
    host: mouse
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
