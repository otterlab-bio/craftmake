package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeaverRNASEQPDXStep3RunsLocallyAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverRNASEQPDXStep3Tools(t, toolDirectory)
	writeBeaverRNASEQPDXStep3ProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverRNASEQPDX", "step3.yaml")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(os.Environ(), "PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "2",
		"--max-cores", "20",
		"--max-memory", "64G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 3") {
		t.Fatalf("unexpected first BeaverRNASEQPDX step3 status:\n%s", firstStatus)
	}

	for _, expectedOutput := range []string{
		"workflow/expression/sample-a_human.txt",
		"workflow/expression/sample-b_human.txt",
		"workflow/bsmap/RNASplicing/splicing-outcome.json",
		"workflow/bsmap/RNASplicing/events.tsv",
	} {
		if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
			t.Fatalf("expected BeaverRNASEQPDX step3 output %q: %v", expectedOutput, err)
		}
	}
	splicingOutcome, err := os.ReadFile(filepath.Join(projectDirectory, "workflow", "bsmap", "RNASplicing", "splicing-outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(splicingOutcome), `"status": "produced"`) || !strings.Contains(string(splicingOutcome), `"events.tsv"`) {
		t.Fatalf("unexpected RNA-seq PDX splicing outcome %q", splicingOutcome)
	}

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "2",
		"--max-cores", "20",
		"--max-memory", "64G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 3") {
		t.Fatalf("unexpected cached BeaverRNASEQPDX step3 status:\n%s", secondStatus)
	}
}

func writeFakeBeaverRNASEQPDXStep3Tools(t *testing.T, toolDirectory string) {
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
	writeExecutable(t, filepath.Join(toolDirectory, "htseq-count"), `#!/usr/bin/env bash
set -euo pipefail
bam_path="${@: -2:1}"
annotation_path="${@: -1}"
if [[ "$bam_path" != *_fixed_human_Filtered.bam ]] || [ ! -f "$bam_path" ] || [ ! -f "$annotation_path" ]; then exit 3; fi
printf 'gene_a\t10\n'
printf 'gene_b\t20\n'
printf '__source__\t%s|%s\n' "$bam_path" "$annotation_path"
`)
	writeExecutable(t, filepath.Join(toolDirectory, "matsrun"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != "run" ]; then exit 2; fi
shift
root_directory=""
pdata_path=""
qc_directory=""
annotation_path=""
pdx_mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --root) root_directory="$2"; shift 2 ;;
    --pdata) pdata_path="$2"; shift 2 ;;
    --seqlengthQC) qc_directory="$2"; shift 2 ;;
    --gtf) annotation_path="$2"; shift 2 ;;
    --pdxmode) pdx_mode="$2"; shift 2 ;;
    --threads) shift 2 ;;
    *) shift ;;
  esac
done
if [ ! -d "$root_directory" ] || [ ! -f "$pdata_path" ] || [ ! -d "$qc_directory" ] || [ ! -f "$annotation_path" ] || [ "$pdx_mode" != "1" ]; then exit 3; fi
mkdir -p "$root_directory/RNASplicing"
printf 'event\tvalue\nSE\t1\n' > "$root_directory/RNASplicing/events.tsv"
`)
}

func writeBeaverRNASEQPDXStep3ProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	for _, relativePath := range []string{
		"workflow/bsmap/Filtered_bams/sample-a_fixed_human_Filtered.bam",
		"workflow/bsmap/Filtered_bams/sample-b_fixed_human_Filtered.bam",
		"workflow/QC/.keep",
		"config/pdata.xlsx",
		"references/human.fasta",
		"references/mouse.fasta",
		"references/human.gtf",
		"references/mouse.gtf",
		"references/star-human/Genome",
		"references/star-mouse/Genome",
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
  selfconfig: config
  bsmap:
    main: workflow/bsmap
  methylation_call: workflow/expression
  qc:
    main: workflow/QC
  sid_log: workflow/log
workflow:
  mode: RNASEQ
  jobid: beaverrnaseqpdx-step3-integration
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
  group_levels: 2
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
