package cli_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBeaverBSStep1RunsLocallyAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCommand := exec.Command("go", "build", "-o", binaryPath, "./cmd/craftmake")
	buildCommand.Dir = repositoryRoot
	if buildOutput, err := buildCommand.CombinedOutput(); err != nil {
		t.Fatalf("build craftmake: %v\n%s", err, buildOutput)
	}

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverBSTools(t, toolDirectory)
	writeBeaverBSProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step1.yaml")
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
	firstControllerLogPath := outputValue(t, firstRunOutput, "controller_log")
	controllerLogData, err := os.ReadFile(firstControllerLogPath)
	if err != nil {
		t.Fatalf("read controller log: %v", err)
	}
	if !strings.Contains(string(controllerLogData), `"event":"run.started"`) ||
		!strings.Contains(string(controllerLogData), `"event":"attempt.finished"`) ||
		!strings.Contains(string(controllerLogData), `"event":"run.finished"`) ||
		!strings.Contains(string(controllerLogData), `"run_id":"`+firstRunID+`"`) {
		t.Fatalf("controller log does not contain the expected lifecycle context:\n%s", controllerLogData)
	}
	listedLogs := runCraftmake(t, binaryPath, commandEnvironment, "logs", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(listedLogs, "controller\t"+firstRunID+"\t"+firstControllerLogPath) {
		t.Fatalf("logs command did not expose controller log path:\n%s", listedLogs)
	}
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 6") {
		t.Fatalf("unexpected first run status:\n%s", firstStatus)
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
			t.Fatalf("expected BeaverBS output %q: %v", expectedOutput, err)
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
		t.Fatalf("unexpected cached run status:\n%s", secondStatus)
	}
	verboseSecondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID, "--verbose")
	if !strings.Contains(verboseSecondStatus, "cache_decisions:") || !strings.Contains(verboseSecondStatus, "decision=hit") {
		t.Fatalf("expected verbose status to include cache hit decisions:\n%s", verboseSecondStatus)
	}

	reportDirectory := filepath.Join(temporaryDirectory, "reports")
	runCraftmake(t, binaryPath, commandEnvironment,
		"report", "--state", statePath, "--run", firstRunID, "--format", "csv", "--output", reportDirectory,
	)
	for _, reportName := range []string{"task_metrics.csv", "step_timings.csv", "allocations.csv"} {
		if _, err := os.Stat(filepath.Join(reportDirectory, reportName)); err != nil {
			t.Fatalf("expected report %q: %v", reportName, err)
		}
	}
	taskMetricsData, err := os.ReadFile(filepath.Join(reportDirectory, "task_metrics.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(taskMetricsData), "cache_decision,cache_reason_code,cache_reason_detail") ||
		!strings.Contains(string(taskMetricsData), "no_prior_success") {
		t.Fatalf("expected task metrics report to include cache diagnostics:\n%s", taskMetricsData)
	}
}

func resolveRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve current test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

func runCraftmake(t *testing.T, binaryPath string, environment []string, arguments ...string) string {
	t.Helper()
	command := exec.Command(binaryPath, arguments...)
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("craftmake %s: %v\n%s%s", strings.Join(arguments, " "), err, output, craftmakeFailureDiagnostics(output))
	}
	return string(output)
}

func craftmakeFailureDiagnostics(commandOutput []byte) string {
	controllerLogPath := ""
	for _, line := range strings.Split(string(commandOutput), "\n") {
		if strings.HasPrefix(line, "controller_log:") {
			controllerLogPath = strings.TrimSpace(strings.TrimPrefix(line, "controller_log:"))
			break
		}
	}
	if controllerLogPath == "" {
		return ""
	}

	var diagnostics strings.Builder
	if controllerLogData, err := os.ReadFile(controllerLogPath); err == nil {
		fmt.Fprintf(&diagnostics, "\ncontroller events:\n%s", controllerLogData)
	}
	runDirectory := filepath.Dir(controllerLogPath)
	_ = filepath.Walk(runDirectory, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() {
			return nil
		}
		fileName := info.Name()
		if fileName != "result.json" && fileName != "stderr.log" {
			return nil
		}
		fileData, readErr := os.ReadFile(path)
		if readErr == nil {
			fmt.Fprintf(&diagnostics, "\n%s:\n%s", path, fileData)
		}
		return nil
	})
	return diagnostics.String()
}

func outputValue(t *testing.T, output, key string) string {
	t.Helper()
	prefix := key + ":"
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	t.Fatalf("output does not contain %q:\n%s", prefix, output)
	return ""
}

func writeFakeBeaverBSTools(t *testing.T, toolDirectory string) {
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
	writeExecutable(t, filepath.Join(toolDirectory, "fastqcx"), `#!/usr/bin/env bash
set -euo pipefail
output_directory=""
input_path=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -q) input_path="$2"; shift 2 ;;
    -s) output_directory="$2"; shift 2 ;;
    --no-html) shift ;;
    *) shift ;;
  esac
done
mkdir -p "$output_directory"
printf 'input=%s\n' "$input_path" > "$output_directory/fastqc_data.txt"
`)
	writeExecutable(t, filepath.Join(toolDirectory, "fastqc"), `#!/usr/bin/env bash
set -euo pipefail
output_directory=""
input_paths=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output_directory="$2"; shift 2 ;;
    -t) shift 2 ;;
    --extract) shift ;;
    *) input_paths+=("$1"); shift ;;
  esac
done
mkdir -p "$output_directory"
for input_path in "${input_paths[@]}"; do
  file_name="$(basename "$input_path")"
  sample_name="${file_name%.fastq.gz}"
  sample_name="${sample_name%.fq.gz}"
  printf 'fastqc input=%s\n' "$input_path" > "$output_directory/${sample_name}_fastqc.zip"
  printf 'fastqc input=%s\n' "$input_path" > "$output_directory/${sample_name}_fastqc.html"
done
`)
	writeExecutable(t, filepath.Join(toolDirectory, "seqkit"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != "stat" ]; then exit 2; fi
shift
for argument in "$@"; do
  case "$argument" in
    -a|-T|-b) ;;
    -j) shift ;;
    *) printf 'seqkit input=%s\n' "$argument" ;;
  esac
done
`)
	writeExecutable(t, filepath.Join(toolDirectory, "trim_galore"), `#!/usr/bin/env bash
set -euo pipefail
output_directory=""
basename_value=""
input_paths=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    -e|-j|--adapter|--adapter2|--clip_R1|--clip_R2|--three_prime_clip_R1|--three_prime_clip_R2) shift 2 ;;
    --basename) basename_value="$2"; shift 2 ;;
    -o) output_directory="$2"; shift 2 ;;
    --paired) shift ;;
    *) input_paths+=("$1"); shift ;;
  esac
done
mkdir -p "$output_directory"
printf 'trimmed from %s\n' "${input_paths[0]}" > "$output_directory/${basename_value}_val_1.fq.gz"
printf 'trimmed from %s\n' "${input_paths[1]}" > "$output_directory/${basename_value}_val_2.fq.gz"
printf 'report\n' > "$output_directory/${basename_value}_R1.fastq.gz_trimming_report.txt"
printf 'report\n' > "$output_directory/${basename_value}_R2.fastq.gz_trimming_report.txt"
`)
}

func writeBeaverBSProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	dataDirectory := filepath.Join(projectDirectory, "data")
	if err := os.MkdirAll(dataDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sampleID := range []string{"sample-a", "sample-b"} {
		for _, readID := range []string{"R1", "R2"} {
			path := filepath.Join(dataDirectory, sampleID+"_"+readID+".fastq.gz")
			if err := os.WriteFile(path, []byte("@read\nACGT\n+\n!!!!\n"), 0o644); err != nil {
				t.Fatal(err)
			}
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
directories:
  qc:
    main: workflow/QC
    before: workflow/fastqc_raw
    after: workflow/fastqc_clean
  sid_log: workflow/log
workflow:
  mode: RRBS
  jobid: integration
  userid: integration
  species:
    primary: human
  adapters:
    error: 0.2
    seq1: [AGATCGGAAGAGC, NO_ADAPTER_CAL_USE_DEFAULT]
    seq2: [AGATCGGAAGAGC, NO_ADAPTER_CAL_USE_DEFAULT]
  alignment:
    c1: 7
    c2: 9
    t1: 0
    t2: 0
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

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
