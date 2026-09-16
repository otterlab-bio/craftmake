package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeaverBSStep3RunsLocallyAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverBSStep3Tools(t, toolDirectory)
	writeBeaverBSStep3ProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step3.yaml")
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
		"--max-memory", "68G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 2") {
		t.Fatalf("unexpected first BeaverBS step3 status:\n%s", firstStatus)
	}

	for _, expectedOutput := range []string{
		"workflow/bsmap/sample-a_nsort.bam",
		"workflow/bsmap/sample-b_nsort.bam",
		"workflow/mCall/sample-a_nsort.bismark.cov.gz",
		"workflow/mCall/sample-b_nsort.bismark.cov.gz",
	} {
		if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
			t.Fatalf("expected BeaverBS step3 output %q: %v", expectedOutput, err)
		}
	}

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "2",
		"--max-cores", "20",
		"--max-memory", "68G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 2") {
		t.Fatalf("unexpected cached BeaverBS step3 status:\n%s", secondStatus)
	}
}

func writeFakeBeaverBSStep3Tools(t *testing.T, toolDirectory string) {
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
	writeExecutable(t, filepath.Join(toolDirectory, "samtools"), `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" != "sort" ]; then exit 2; fi
shift
output_path=""
input_path=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -@|-o) if [ "$1" = "-o" ]; then output_path="$2"; fi; shift 2 ;;
    -n) shift ;;
    *) input_path="$1"; shift ;;
  esac
done
mkdir -p "$(dirname "$output_path")"
printf 'name-sorted from %s\n' "$input_path" > "$output_path"
`)
	writeExecutable(t, filepath.Join(toolDirectory, "pairbam"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  *.bam) ;;
  *) printf 'input must end with .bam: %s\n' "$1" >&2; exit 2 ;;
esac
cp "$1" "$2"
`)
	writeExecutable(t, filepath.Join(toolDirectory, "bismark_methylation_extractor"), `#!/usr/bin/env bash
set -euo pipefail
output_directory=""
input_bam=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output_dir) output_directory="$2"; shift 2 ;;
    --multicore|--buffer_size) shift 2 ;;
    --paired-end|--gzip|--comprehensive|--merge_non_CpG|--bedGraph) shift ;;
    *) input_bam="$1"; shift ;;
  esac
done
mkdir -p "$output_directory"
input_name="$(basename "$input_bam" .bam)"
printf 'coverage\n' > "$output_directory/${input_name}.bismark.cov.gz"
`)
}

func writeBeaverBSStep3ProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	for _, relativePath := range []string{
		"workflow/bsmap/sample-a_human.bam",
		"workflow/bsmap/sample-b_human.bam",
		"references/human.fasta",
		"references/bismark-human",
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
species: human
output:
  workflow_dir: workflow
  analysis_dir: analysis
  log_dir: workflow/log
directories:
  work: workflow
  bsmap:
    main: workflow/bsmap
  methylation_call: workflow/mCall
  sid_log: workflow/log
workflow:
  mode: RRBS
  jobid: beaverbs-step3-integration
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
