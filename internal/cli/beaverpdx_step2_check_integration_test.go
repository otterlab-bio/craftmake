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

func TestBeaverPDXStep2CheckRunsLocallyAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverPDXStep2CheckTools(t, toolDirectory)
	writeBeaverPDXStep2CheckProjectFixture(t, projectDirectory)

	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverPDX", "step2-check.yaml")
	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(
		os.Environ(),
		"PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SAMTOOLS="+filepath.Join(toolDirectory, "samtools"),
	)
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "8",
		"--max-cores", "16",
		"--max-memory", "128G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "status: succeeded") || !strings.Contains(firstStatus, "succeeded: 5") {
		t.Fatalf("unexpected first BeaverPDX step2-check status:\n%s", firstStatus)
	}

	expectedOutputs := []string{
		"workflow/bsmap/Filtered_bams/filtered-bam-validation.json",
	}
	for _, sampleID := range []string{"sample-a", "sample-b"} {
		expectedOutputs = append(expectedOutputs,
			filepath.Join("workflow", "bsmap", "Filtered_bams", sampleID+"_fixed_human_Filtered.bam"),
			filepath.Join("workflow", "bsmap", "Filtered_bams", sampleID+"_fixed_human_Filtered.bam.bai"),
		)
		for _, speciesName := range []string{"human", "mouse"} {
			expectedOutputs = append(expectedOutputs,
				filepath.Join("workflow", "log", "step2-check", sampleID+"_"+speciesName+".ready"),
			)
		}
	}
	for _, expectedOutput := range expectedOutputs {
		if _, err := os.Stat(filepath.Join(projectDirectory, expectedOutput)); err != nil {
			t.Fatalf("expected BeaverPDX step2-check output %q: %v", expectedOutput, err)
		}
	}
	assertBeaverPDXFilteredBAMValidationManifest(t, projectDirectory)
	for _, sampleID := range []string{"sample-a", "sample-b"} {
		for _, speciesName := range []string{"human", "mouse"} {
			assertBeaverPDXSampleValidationManifest(t, projectDirectory, sampleID, speciesName)
		}
	}
	for _, obsoleteMarker := range []string{
		filepath.Join("workflow", "bsmap", "Filtered_bams", "filtered_success.txt"),
		filepath.Join("workflow", "log", "step2_success.txt"),
		filepath.Join("workflow", "log", "step2-check", "sample-a_filtered.ready"),
		filepath.Join("workflow", "bsmap", "sample-a_human_pdx_patch_success"),
	} {
		if _, err := os.Stat(filepath.Join(projectDirectory, obsoleteMarker)); !os.IsNotExist(err) {
			t.Fatalf("step2-check must not generate marker-only output %q, stat error=%v", obsoleteMarker, err)
		}
	}

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "8",
		"--max-cores", "16",
		"--max-memory", "128G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 5") {
		t.Fatalf("unexpected cached BeaverPDX step2-check status:\n%s", secondStatus)
	}
}

func writeFakeBeaverPDXStep2CheckTools(t *testing.T, toolDirectory string) {
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
	writeExecutable(t, filepath.Join(toolDirectory, "picard"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != "SetNmMdAndUqTags" ]; then exit 2; fi
shift
input_path=""
output_path=""
reference_path=""
bisulfite_mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    I=*) input_path="${1#I=}"; shift ;;
    O=*) output_path="${1#O=}"; shift ;;
    R=*) reference_path="${1#R=}"; shift ;;
    IS_BISULFITE_SEQUENCE=*) bisulfite_mode="${1#IS_BISULFITE_SEQUENCE=}"; shift ;;
    *) shift ;;
  esac
done
if [ ! -f "$input_path" ] || [ ! -f "$reference_path" ] || [ "$bisulfite_mode" != "true" ]; then exit 3; fi
mkdir -p "$(dirname "$output_path")"
printf 'fixed from %s with %s\n' "$input_path" "$reference_path" > "$output_path"
`)
	writeExecutable(t, filepath.Join(toolDirectory, "xenofilx"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != "run" ]; then exit 2; fi
shift
graft_bams=()
host_bams=()
output_names=()
output_directory=""
graft_reference=""
host_reference=""
recalculate_nm_enabled="false"
bisulfite_enabled="false"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --graft)
      shift
      while [ "$#" -gt 0 ] && [[ "$1" != --* ]]; do graft_bams+=("$1"); shift; done
      ;;
    --host)
      shift
      while [ "$#" -gt 0 ] && [[ "$1" != --* ]]; do host_bams+=("$1"); shift; done
      ;;
    --output-names)
      shift
      while [ "$#" -gt 0 ] && [[ "$1" != --* ]]; do output_names+=("$1"); shift; done
      ;;
    --output) output_directory="$2"; shift 2 ;;
    --graft-ref) graft_reference="$2"; shift 2 ;;
    --host-ref) host_reference="$2"; shift 2 ;;
    --mm-threshold|--unmapped-penalty|--threads) shift 2 ;;
    --recalculate-nm) recalculate_nm_enabled="true"; shift ;;
    --bisulfite) bisulfite_enabled="true"; shift ;;
    *) shift ;;
  esac
done
if [ "${#graft_bams[@]}" -ne "${#host_bams[@]}" ] || [ "${#graft_bams[@]}" -ne "${#output_names[@]}" ] || [ "${#graft_bams[@]}" -eq 0 ]; then exit 3; fi
if [ ! -f "$graft_reference" ] || [ ! -f "$host_reference" ] || [ "$recalculate_nm_enabled" != "true" ] || [ "$bisulfite_enabled" != "true" ]; then exit 4; fi
mkdir -p "$output_directory"
for graft_index in "${!graft_bams[@]}"; do
  graft_bam="${graft_bams[$graft_index]}"
  output_name="${output_names[$graft_index]}"
  if [[ "$graft_bam" == *"_fixed_"* ]]; then exit 5; fi
  printf 'filtered from %s\n' "$graft_bam" > "$output_directory/$output_name"
  printf 'index for %s\n' "$output_name" > "$output_directory/$output_name.bai"
done
`)
	writeExecutable(t, filepath.Join(toolDirectory, "samtools"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  quickcheck)
    shift
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -*) shift ;;
        *) test -s "$1"; shift ;;
      esac
    done
    ;;
  view)
    printf '1\n'
    ;;
  *)
    exit 2
    ;;
esac
`)
}

func writeBeaverPDXStep2CheckProjectFixture(t *testing.T, projectDirectory string) {
	t.Helper()
	inputPaths := []string{
		"references/human.fasta",
		"references/mouse.fasta",
		"references/bismark-human",
		"references/bismark-mouse",
	}
	for _, sampleID := range []string{"sample-a", "sample-b"} {
		for _, speciesName := range []string{"human", "mouse"} {
			inputPaths = append(inputPaths,
				filepath.Join("workflow", "bsmap", sampleID+"_"+speciesName+".bam"),
				filepath.Join("workflow", "bsmap", sampleID+"_"+speciesName+".bam.bai"),
				filepath.Join("workflow", "QC", "qualimap", sampleID+"_"+speciesName, "qualimapReport.html"),
				filepath.Join("workflow", "QC", "qualimap", sampleID+"_"+speciesName, "report.pdf"),
				filepath.Join("workflow", "QC", "GCbias", sampleID+"_"+speciesName, "gc_bias_metrics.txt"),
				filepath.Join("workflow", "QC", "GCbias", sampleID+"_"+speciesName, "gc_bias_metrics.pdf"),
				filepath.Join("workflow", "QC", "GCbias", sampleID+"_"+speciesName, "summary_metrics.txt"),
			)
		}
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
  qc_summary: workflow/QC/summary
  sid_log: workflow/log
workflow:
  mode: RRBS
  jobid: beaverpdx-step2-check-integration
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

func assertBeaverPDXFilteredBAMValidationManifest(t *testing.T, projectDirectory string) {
	t.Helper()
	manifestPath := filepath.Join(projectDirectory, "workflow", "bsmap", "Filtered_bams", "filtered-bam-validation.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read BeaverPDX filtered BAM validation manifest %q: %v", manifestPath, err)
	}

	var manifest filteredBAMValidationManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("parse BeaverPDX filtered BAM validation manifest %q: %v\n%s", manifestPath, err, manifestData)
	}
	if manifest.SchemaVersion != "otter.filtered-bam-validation/v1" ||
		manifest.Status != "validated" ||
		manifest.Workflow != "BeaverPDX" ||
		manifest.Phase != "step2-check" ||
		manifest.GraftSpecies != "human" {
		t.Fatalf("unexpected BeaverPDX filtered BAM validation manifest identity: %#v", manifest)
	}

	expectedSampleIDs := map[string]struct{}{"sample-a": {}, "sample-b": {}}
	if len(manifest.Artifacts) != len(expectedSampleIDs) {
		t.Fatalf("expected %d BeaverPDX filtered BAM entries, got %#v", len(expectedSampleIDs), manifest.Artifacts)
	}
	for _, artifact := range manifest.Artifacts {
		if _, knownSample := expectedSampleIDs[artifact.SampleID]; !knownSample {
			t.Fatalf("unexpected BeaverPDX filtered BAM validation entry: %#v", artifact)
		}
		expectedBAMPath := filepath.Join("workflow", "bsmap", "Filtered_bams", artifact.SampleID+"_fixed_human_Filtered.bam")
		if artifact.BAMPath != expectedBAMPath || artifact.BAIPath != expectedBAMPath+".bai" || artifact.BAMSizeBytes <= 0 || artifact.BAISizeBytes <= 0 || artifact.MappedReadCount <= 0 {
			t.Fatalf("unexpected BeaverPDX filtered BAM validation metadata: %#v", artifact)
		}
		for _, fileMetadata := range []struct {
			path     string
			size     int64
			checksum string
		}{
			{artifact.BAMPath, artifact.BAMSizeBytes, artifact.BAMSHA256},
			{artifact.BAIPath, artifact.BAISizeBytes, artifact.BAISHA256},
		} {
			fileData, err := os.ReadFile(filepath.Join(projectDirectory, fileMetadata.path))
			if err != nil {
				t.Fatalf("read validated BeaverPDX filtered BAM artifact %q: %v", fileMetadata.path, err)
			}
			digest := sha256.Sum256(fileData)
			if fileMetadata.size != int64(len(fileData)) || fileMetadata.checksum != hex.EncodeToString(digest[:]) {
				t.Fatalf("BeaverPDX filtered BAM validation digest or size does not match %q", fileMetadata.path)
			}
		}
		delete(expectedSampleIDs, artifact.SampleID)
	}
	if len(expectedSampleIDs) != 0 {
		t.Fatalf("BeaverPDX filtered BAM validation manifest omitted samples: %#v", expectedSampleIDs)
	}
}

func assertBeaverPDXSampleValidationManifest(t *testing.T, projectDirectory string, sampleID string, speciesName string) {
	t.Helper()
	manifestPath := filepath.Join(projectDirectory, "workflow", "log", "step2-check", sampleID+"_"+speciesName+".ready")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read BeaverPDX sample validation manifest %q: %v", manifestPath, err)
	}

	var manifest sampleArtifactValidationManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("parse BeaverPDX sample validation manifest %q: %v\n%s", manifestPath, err, manifestData)
	}
	if manifest.SchemaVersion != "otter.sample-artifacts-validation/v1" ||
		manifest.Status != "validated" ||
		manifest.Workflow != "BeaverPDX" ||
		manifest.Phase != "step2-check" ||
		manifest.SampleID != sampleID ||
		manifest.Dimensions["sample"] != sampleID ||
		manifest.Dimensions["species"] != speciesName ||
		len(manifest.Dimensions) != 2 {
		t.Fatalf("unexpected BeaverPDX sample validation manifest identity: %#v", manifest)
	}

	expectedArtifacts := map[string]string{
		"bam":           filepath.Join("workflow", "bsmap", sampleID+"_"+speciesName+".bam"),
		"bam_index":     filepath.Join("workflow", "bsmap", sampleID+"_"+speciesName+".bam.bai"),
		"qualimap_html": filepath.Join("workflow", "QC", "qualimap", sampleID+"_"+speciesName, "qualimapReport.html"),
		"qualimap_pdf":  filepath.Join("workflow", "QC", "qualimap", sampleID+"_"+speciesName, "report.pdf"),
		"gc_metrics":    filepath.Join("workflow", "QC", "GCbias", sampleID+"_"+speciesName, "gc_bias_metrics.txt"),
		"gc_chart":      filepath.Join("workflow", "QC", "GCbias", sampleID+"_"+speciesName, "gc_bias_metrics.pdf"),
		"gc_summary":    filepath.Join("workflow", "QC", "GCbias", sampleID+"_"+speciesName, "summary_metrics.txt"),
	}
	if len(manifest.Artifacts) != len(expectedArtifacts) {
		t.Fatalf("expected %d BeaverPDX sample validation artifacts, got %#v", len(expectedArtifacts), manifest.Artifacts)
	}
	for _, artifact := range manifest.Artifacts {
		expectedPath, knownArtifact := expectedArtifacts[artifact.ID]
		if !knownArtifact || artifact.Path != expectedPath || artifact.MediaType == "" || artifact.SizeBytes <= 0 {
			t.Fatalf("unexpected BeaverPDX sample validation artifact: %#v", artifact)
		}
		artifactData, err := os.ReadFile(filepath.Join(projectDirectory, artifact.Path))
		if err != nil {
			t.Fatalf("read validated BeaverPDX sample artifact %q: %v", artifact.Path, err)
		}
		digest := sha256.Sum256(artifactData)
		if artifact.SHA256 != hex.EncodeToString(digest[:]) || artifact.SizeBytes != int64(len(artifactData)) {
			t.Fatalf("BeaverPDX sample validation digest or size does not match %q", artifact.Path)
		}
		delete(expectedArtifacts, artifact.ID)
	}
	if len(expectedArtifacts) != 0 {
		t.Fatalf("BeaverPDX sample validation manifest omitted artifacts: %#v", expectedArtifacts)
	}
}
