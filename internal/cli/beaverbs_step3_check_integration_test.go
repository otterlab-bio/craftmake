package cli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeaverBSStep3CheckRunsLocallyAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverBSStep3CheckTools(t, toolDirectory)
	writeBeaverBSStep3CheckProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step3-check.yaml")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(
		os.Environ(),
		"PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"METHX="+filepath.Join(toolDirectory, "methx-override"),
	)
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "20",
		"--max-memory", "64G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 8") {
		t.Fatalf("unexpected first BeaverBS step3-check status:\n%s", firstStatus)
	}

	for _, expectedOutput := range []string{
		"workflow/log/step3-check/sample-a.ready",
		"workflow/log/step3-check/sample-b.ready",
		"workflow/mCall/methrixh5/reference_cpgs.ron",
		"workflow/mCall/methrixh5/human.gtf",
		"workflow/mCall/methrixh5/methrix_data.h5",
		"workflow/mCall/methrixh5/CpG_coverage.xlsx",
		"workflow/bsmap/human/sample-a.html",
		"workflow/bsmap/human/sample-b.html",
		"workflow/bsmap/human/bismark_summary_report.html",
		"workflow/QC/summary/qc_summary.xlsx",
	} {
		if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
			t.Fatalf("expected BeaverBS step3-check output %q: %v", expectedOutput, err)
		}
	}
	if _, err := os.Stat(filepath.Join(projectDirectory, "workflow", "log", "step3_success.txt")); !os.IsNotExist(err) {
		t.Fatalf("step3-check must not generate a marker-only success file, stat error=%v", err)
	}
	assertBeaverBSStep3ValidationManifest(t, projectDirectory, "sample-a")
	assertBeaverBSStep3ValidationManifest(t, projectDirectory, "sample-b")

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "20",
		"--max-memory", "64G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 8") {
		t.Fatalf("unexpected cached BeaverBS step3-check status:\n%s", secondStatus)
	}
}

func writeFakeBeaverBSStep3CheckTools(t *testing.T, toolDirectory string) {
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
	writeExecutable(t, filepath.Join(toolDirectory, "methx-override"), `#!/usr/bin/env bash
set -euo pipefail
command_name="$1"
shift
output_path=""
output_directory=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output_path="$2"; output_directory="$2"; shift 2 ;;
    --annotation-dir|--genome|--input|--threads) shift 2 ;;
    *) shift ;;
  esac
done
if [ "$command_name" = "extract-cpgs" ] || [ "$command_name" = "extract-cp-gs" ]; then
  mkdir -p "$(dirname "$output_path")"
  printf 'reference cpgs\n' > "$output_path"
elif [ "$command_name" = "process" ]; then
  mkdir -p "$output_directory"
  printf 'methrix data\n' > "$output_directory/methrix_data.h5"
  printf 'assays\n' > "$output_directory/assays.h5"
  printf 'coverage\n' > "$output_directory/CpG_coverage.xlsx"
  printf 'annotation report\n' > "$output_directory/CpG_annotation_report.xlsx"
  printf 'annotation details\n' > "$output_directory/CpG_annotation_details.tsv.gz"
else
  exit 2
fi
`)
	writeExecutable(t, filepath.Join(toolDirectory, "bismark2report"), `#!/usr/bin/env bash
set -euo pipefail
output_directory=""
output_name=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --dir) output_directory="$2"; shift 2 ;;
    --output) output_name="$2"; shift 2 ;;
    --alignment_report|--splitting_report|--mbias_report) shift 2 ;;
    *) printf 'unexpected bismark2report argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
mkdir -p "$output_directory"
printf 'bismark report\n' > "$output_directory/$output_name"
`)
	writeExecutable(t, filepath.Join(toolDirectory, "bismark2summary"), `#!/usr/bin/env bash
set -euo pipefail
printf 'bismark summary\n' > bismark_summary_report.html
`)
	writeExecutable(t, filepath.Join(toolDirectory, "qctb"), `#!/usr/bin/env bash
set -euo pipefail
output_path=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --config) shift 2 ;;
    --output) output_path="$2"; shift 2 ;;
    *) shift ;;
  esac
done
mkdir -p "$(dirname "$output_path")"
printf 'qc summary\n' > "$output_path"
`)
}

func writeBeaverBSStep3CheckProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	inputPaths := []string{
		"config/config.yaml",
		"references/human.fasta",
		"references/human.gtf",
	}
	for _, sampleID := range []string{"sample-a", "sample-b"} {
		inputPaths = append(inputPaths,
			"workflow/mCall/"+sampleID+"_nsort.bismark.cov.gz",
			"workflow/mCall/"+sampleID+"_nsort_splitting_report.txt",
			"workflow/mCall/"+sampleID+"_nsort.M-bias.txt",
			"workflow/bsmap/"+sampleID+"_nsort.bam",
			"workflow/bsmap/"+sampleID+"_human.bam",
			"workflow/trim/"+sampleID+"_R1.fastq.gz_trimming_report.txt",
			"workflow/trim/"+sampleID+"_R2.fastq.gz_trimming_report.txt",
			"workflow/fastqc_raw/"+sampleID+"_R1_fastqcx/fastqc_data.txt",
			"workflow/fastqc_raw/"+sampleID+"_R2_fastqcx/fastqc_data.txt",
			"workflow/fastqc_clean/"+sampleID+"_val_1_fastqcx/fastqc_data.txt",
			"workflow/fastqc_clean/"+sampleID+"_val_2_fastqcx/fastqc_data.txt",
			"workflow/QC/qualimap/"+sampleID+"_human/qualimapReport.html",
			"workflow/bsmap/human/"+sampleID+"_val_1_bismark_bt2_pe.bam",
			"workflow/bsmap/human/"+sampleID+"_val_1_bismark_bt2_PE_report.txt",
		)
	}
	for _, relativePath := range inputPaths {
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
  trim_dir: workflow/trim
  workflow_dir: workflow
  analysis_dir: analysis
  log_dir: workflow/log
directories:
  work: workflow
  selfconfig: config
  bsmap:
    main: workflow/bsmap
  methylation_call: workflow/mCall
  qualimap: workflow/QC/qualimap
  qc:
    main: workflow/QC
    before: workflow/fastqc_raw
    after: workflow/fastqc_clean
  qc_summary: workflow/QC/summary
  sid_log: workflow/log
workflow:
  mode: RRBS
  jobid: beaverbs-step3-check-integration
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
  rnaseq:
    gtf: [references/human.gtf]
`
	if err := os.WriteFile(filepath.Join(projectDirectory, "config.yaml"), []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
}

type sampleArtifactValidationManifest struct {
	SchemaVersion string                             `json:"schema_version"`
	Status        string                             `json:"status"`
	Workflow      string                             `json:"workflow"`
	Phase         string                             `json:"phase"`
	SampleID      string                             `json:"sample_id"`
	Dimensions    map[string]string                  `json:"dimensions"`
	Artifacts     []sampleArtifactValidationArtifact `json:"artifacts"`
}

type sampleArtifactValidationArtifact struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

func assertBeaverBSStep3ValidationManifest(t *testing.T, projectDirectory string, sampleID string) {
	t.Helper()
	manifestPath := filepath.Join(projectDirectory, "workflow", "log", "step3-check", sampleID+".ready")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read validation manifest %q: %v", manifestPath, err)
	}

	var manifest sampleArtifactValidationManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("parse validation manifest %q: %v\n%s", manifestPath, err, manifestData)
	}
	if manifest.SchemaVersion != "otter.sample-artifacts-validation/v1" ||
		manifest.Status != "validated" ||
		manifest.Workflow != "BeaverBS" ||
		manifest.Phase != "step3-check" ||
		manifest.SampleID != sampleID ||
		manifest.Dimensions["sample"] != sampleID ||
		len(manifest.Dimensions) != 1 {
		t.Fatalf("unexpected validation manifest identity: %#v", manifest)
	}

	expectedArtifactPaths := map[string]string{
		"coverage":         filepath.Join("workflow", "mCall", sampleID+"_nsort.bismark.cov.gz"),
		"name_sorted_bam":  filepath.Join("workflow", "bsmap", sampleID+"_nsort.bam"),
		"trim_report_r1":   filepath.Join("workflow", "trim", sampleID+"_R1.fastq.gz_trimming_report.txt"),
		"trim_report_r2":   filepath.Join("workflow", "trim", sampleID+"_R2.fastq.gz_trimming_report.txt"),
		"fastqc_before_r1": filepath.Join("workflow", "fastqc_raw", sampleID+"_R1_fastqcx", "fastqc_data.txt"),
		"fastqc_before_r2": filepath.Join("workflow", "fastqc_raw", sampleID+"_R2_fastqcx", "fastqc_data.txt"),
		"fastqc_after_r1":  filepath.Join("workflow", "fastqc_clean", sampleID+"_val_1_fastqcx", "fastqc_data.txt"),
		"fastqc_after_r2":  filepath.Join("workflow", "fastqc_clean", sampleID+"_val_2_fastqcx", "fastqc_data.txt"),
		"sorted_bam":       filepath.Join("workflow", "bsmap", sampleID+"_human.bam"),
		"qualimap_report":  filepath.Join("workflow", "QC", "qualimap", sampleID+"_human", "qualimapReport.html"),
	}
	if len(manifest.Artifacts) != len(expectedArtifactPaths) {
		t.Fatalf("expected %d validation manifest artifacts, got %#v", len(expectedArtifactPaths), manifest.Artifacts)
	}

	for _, artifact := range manifest.Artifacts {
		expectedPath, knownArtifact := expectedArtifactPaths[artifact.ID]
		if !knownArtifact {
			t.Fatalf("unexpected validation manifest artifact: %#v", artifact)
		}
		if artifact.Path != expectedPath || artifact.MediaType == "" || artifact.SizeBytes <= 0 {
			t.Fatalf("unexpected validation manifest artifact metadata: %#v", artifact)
		}
		artifactData, err := os.ReadFile(filepath.Join(projectDirectory, artifact.Path))
		if err != nil {
			t.Fatalf("read validated artifact %q: %v", artifact.Path, err)
		}
		digest := sha256.Sum256(artifactData)
		if artifact.SHA256 != hex.EncodeToString(digest[:]) || artifact.SizeBytes != int64(len(artifactData)) {
			t.Fatalf("validation manifest artifact digest or size does not match %q: %#v", artifact.Path, artifact)
		}
		delete(expectedArtifactPaths, artifact.ID)
	}
	if len(expectedArtifactPaths) != 0 {
		t.Fatalf("validation manifest omitted artifact IDs: %#v", expectedArtifactPaths)
	}
}
