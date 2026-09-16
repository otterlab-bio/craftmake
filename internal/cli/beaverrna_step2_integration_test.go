package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeaverRNAStep2RunsLocallyAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverRNAStep2Tools(t, toolDirectory)
	writeBeaverRNAStep2ProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverRNA", "step2.yaml")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(os.Environ(), "PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "80",
		"--max-memory", "128G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 6") {
		t.Fatalf("unexpected first BeaverRNA step2 status:\n%s", firstStatus)
	}

	for _, sampleID := range []string{"sample-a", "sample-b"} {
		for _, expectedOutput := range []string{
			filepath.Join("workflow", "bsmap", sampleID+"_human.bam"),
			filepath.Join("workflow", "bsmap", sampleID+"_human.bam.bai"),
			filepath.Join("workflow", "QC", "qualimap", sampleID+"_human", "qualimapReport.html"),
			filepath.Join("workflow", "QC", "qualimap", sampleID+"_human", "report.pdf"),
			filepath.Join("workflow", "expression", sampleID+"_human.txt"),
		} {
			if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
				t.Fatalf("expected BeaverRNA step2 output %q: %v", expectedOutput, err)
			}
		}
	}

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "80",
		"--max-memory", "128G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 6") {
		t.Fatalf("unexpected cached BeaverRNA step2 status:\n%s", secondStatus)
	}
}

func writeFakeBeaverRNAStep2Tools(t *testing.T, toolDirectory string) {
	t.Helper()
	writeFakeBeaverBSStep2Tools(t, toolDirectory)
	writeExecutable(t, filepath.Join(toolDirectory, "STAR"), `#!/usr/bin/env bash
set -euo pipefail
output_prefix=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --outFileNamePrefix) output_prefix="$2"; shift 2 ;;
    --readFilesIn) shift 3 ;;
    --outSAMattributes) shift 7 ;;
    --runThreadN|--readFilesCommand|--quantMode|--genomeDir|--twopassMode|--outSAMunmapped) shift 2 ;;
    --outSAMtype) shift 3 ;;
    *) shift ;;
  esac
done
mkdir -p "$(dirname "$output_prefix")"
printf 'STAR aligned BAM\n' > "${output_prefix}Aligned.sortedByCoord.out.bam"
`)
	writeExecutable(t, filepath.Join(toolDirectory, "htseq-count"), `#!/usr/bin/env bash
set -euo pipefail
bam_path="${@: -2:1}"
annotation_path="${@: -1}"
printf 'gene_a\t10\n'
printf 'gene_b\t20\n'
printf '__source__\t%s|%s\n' "$bam_path" "$annotation_path"
`)
}

func writeBeaverRNAStep2ProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	for _, relativePath := range []string{
		"workflow/trim/sample-a_val_1.fq.gz",
		"workflow/trim/sample-a_val_2.fq.gz",
		"workflow/trim/sample-b_val_1.fq.gz",
		"workflow/trim/sample-b_val_2.fq.gz",
		"references/human.fasta",
		"references/human.gtf",
		"references/star-human/Genome",
	} {
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
species: human
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
  methylation_call: workflow/expression
  qualimap: workflow/QC/qualimap
  qc:
    main: workflow/QC
  sid_log: workflow/log
workflow:
  mode: RNASEQ
  jobid: beaverrna-step2-integration
  userid: integration
  species:
    name: [human]
    primary: human
metadata:
  sample_ids: [sample-a, sample-b]
reference:
  files:
    fasta: [references/human.fasta]
  indices:
    genome: [references/star-human]
  rnaseq:
    gtf: [references/human.gtf]
    ref: [references/star-human]
`
	if err := os.WriteFile(filepath.Join(projectDirectory, "config.yaml"), []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
}
