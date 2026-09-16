package cli_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	craftmakeBinaryBuildOnce   sync.Once
	sharedCraftmakeBinaryPath  string
	sharedCraftmakeBinaryError error
	sharedCraftmakeBuildOutput []byte
)

func TestBeaverBSStep2RunsLocallyAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverBSStep2Tools(t, toolDirectory)
	writeBeaverBSStep2ProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step2.yaml")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(os.Environ(), "PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "40",
		"--max-memory", "160G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 6") {
		t.Fatalf("unexpected first BeaverBS step2 status:\n%s", firstStatus)
	}

	for _, expectedOutput := range []string{
		"workflow/bsmap/sample-a_human.bam",
		"workflow/bsmap/sample-a_human.bam.bai",
		"workflow/bsmap/sample-b_human.bam",
		"workflow/bsmap/sample-b_human.bam.bai",
		"workflow/QC/qualimap/sample-a_human/qualimapReport.html",
		"workflow/QC/qualimap/sample-b_human/report.pdf",
		"workflow/QC/GCbias/sample-a_human/gc_bias_metrics.txt",
		"workflow/QC/GCbias/sample-b_human/summary_metrics.txt",
	} {
		if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
			t.Fatalf("expected BeaverBS step2 output %q: %v", expectedOutput, err)
		}
	}

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "40",
		"--max-memory", "160G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 6") {
		t.Fatalf("unexpected cached BeaverBS step2 status:\n%s", secondStatus)
	}
}

func buildCraftmakeBinary(t *testing.T, repositoryRoot, binaryPath string) {
	t.Helper()
	craftmakeBinaryBuildOnce.Do(func() {
		buildDirectory, err := os.MkdirTemp("", "craftmake-cli-test-build-")
		if err != nil {
			sharedCraftmakeBinaryError = err
			return
		}
		sharedCraftmakeBinaryPath = filepath.Join(buildDirectory, "craftmake")
		buildCommand := exec.Command("go", "build", "-o", sharedCraftmakeBinaryPath, "./cmd/craftmake")
		buildCommand.Dir = repositoryRoot
		sharedCraftmakeBuildOutput, sharedCraftmakeBinaryError = buildCommand.CombinedOutput()
	})
	if sharedCraftmakeBinaryError != nil {
		t.Fatalf("build shared Craftmake binary: %v\n%s", sharedCraftmakeBinaryError, sharedCraftmakeBuildOutput)
	}

	sourceBinary, err := os.Open(sharedCraftmakeBinaryPath)
	if err != nil {
		t.Fatalf("open shared Craftmake binary: %v", err)
	}
	defer sourceBinary.Close()
	destinationBinary, err := os.OpenFile(binaryPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create test Craftmake binary: %v", err)
	}
	if _, err := io.Copy(destinationBinary, sourceBinary); err != nil {
		_ = destinationBinary.Close()
		t.Fatalf("copy shared Craftmake binary: %v", err)
	}
	if err := destinationBinary.Close(); err != nil {
		t.Fatalf("finalize test Craftmake binary: %v", err)
	}
}

func writeFakeBeaverBSStep2Tools(t *testing.T, toolDirectory string) {
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
	writeExecutable(t, filepath.Join(toolDirectory, "bismark"), `#!/usr/bin/env bash
set -euo pipefail
output_directory=""
sample_input=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --genome|--parallel|-2|--temp_dir) shift 2 ;;
    -1) sample_input="$2"; shift 2 ;;
    -o) output_directory="$2"; shift 2 ;;
    --nucleotide_coverage) shift ;;
    *) shift ;;
  esac
done
mkdir -p "$output_directory"
sample_name="$(basename "${sample_input}" _val_1.fq.gz)"
printf 'bismark output\n' > "$output_directory/${sample_name}_val_1_bismark_bt2_pe.bam"
`)
	writeExecutable(t, filepath.Join(toolDirectory, "samtools"), `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = "sort" ]; then
  output_path=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -@) shift 2 ;;
      -o) output_path="$2"; shift 2 ;;
      *) input_path="$1"; shift ;;
    esac
  done
  mkdir -p "$(dirname "$output_path")"
  printf 'sorted from %s\n' "$input_path" > "$output_path"
elif [ "$1" = "index" ]; then
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -@) shift 2 ;;
      -b) shift ;;
      *) bam_path="$1"; shift ;;
    esac
  done
  printf 'index\n' > "$bam_path.bai"
else
  exit 2
fi
`)
	writeExecutable(t, filepath.Join(toolDirectory, "qualimap"), `#!/usr/bin/env bash
set -euo pipefail
output_directory=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    bamqc) shift ;;
    -bam) shift 2 ;;
    -outdir) output_directory="$2"; shift 2 ;;
    -outformat|--java-mem-size=*)
      if [ "$1" = "-outformat" ]; then shift 2; else shift; fi ;;
    *) shift ;;
  esac
done
mkdir -p "$output_directory"
printf 'qualimap html\n' > "$output_directory/qualimapReport.html"
printf 'qualimap pdf\n' > "$output_directory/report.pdf"
`)
	writeExecutable(t, filepath.Join(toolDirectory, "picard"), `#!/usr/bin/env bash
set -euo pipefail
metrics=""
chart=""
summary=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    CollectGcBiasMetrics) shift ;;
    I=*) shift ;;
    O=*) metrics="${1#O=}"; shift ;;
    CHART=*) chart="${1#CHART=}"; shift ;;
    S=*) summary="${1#S=}"; shift ;;
    R=*) shift ;;
    *) shift ;;
  esac
done
mkdir -p "$(dirname "$metrics")"
printf 'gc metrics\n' > "$metrics"
printf 'gc chart\n' > "$chart"
printf 'gc summary\n' > "$summary"
`)
}

func writeBeaverBSStep2ProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	for _, relativePath := range []string{
		"workflow/trim/sample-a_val_1.fq.gz",
		"workflow/trim/sample-a_val_2.fq.gz",
		"workflow/trim/sample-b_val_1.fq.gz",
		"workflow/trim/sample-b_val_2.fq.gz",
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
  qc_summary: workflow/QC/summary
  sid_log: workflow/log
workflow:
  mode: RRBS
  jobid: beaverbs-step2-integration
  userid: integration
  species:
    primary: human
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
