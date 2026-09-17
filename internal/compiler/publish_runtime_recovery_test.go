package compiler_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestBeaverRNAPublishCommandRecoversAfterSplicingDirectoryPublicationIsKilled(t *testing.T) {
	temporaryDirectory := t.TempDir()
	fixture := writeRNAPublishRuntimeFixture(t, temporaryDirectory)
	publishCommand := compilePublishRuntimeCommand(t, "BeaverRNA", "RNASEQ", fixture.rawConfiguration, fixture.paths)
	checkpointPath := filepath.Join(temporaryDirectory, "splicing-publication-checkpoint")
	toolDirectory, fakeOtterPath, otterLogPath := writePublishRuntimeTools(t, temporaryDirectory)

	runInterruptedPublishRuntimeCommand(t, publishCommand, toolDirectory, fakeOtterPath, otterLogPath, checkpointPath, "splicing")
	assertPublishRuntimeArtifacts(t, fixture.resultsDirectory, []string{
		"splicing/outcome.json",
		"splicing/files/events.tsv",
	})
	assertPublishRuntimeNoManifest(t, fixture.resultsDirectory)

	runPublishRuntimeCommand(t, publishCommand, publishRuntimeEnvironment(toolDirectory, fakeOtterPath, otterLogPath, "", ""))
	assertPublishRuntimeArtifacts(t, fixture.resultsDirectory, []string{
		"splicing/outcome.json",
		"splicing/files/events.tsv",
		"methylation/matrix_count.txt",
		"methylation/matrix_norm.txt",
		"qc/qc_summary.xlsx",
		"artifacts.json",
	})
	assertPublishRuntimeManifestAndIdempotency(t, publishCommand, toolDirectory, fakeOtterPath, otterLogPath, fixture.resultsDirectory, "publish\nverify\nverify\n")
}

func TestBeaverPDXPublishCommandRecoversAfterMethylationDirectoryPublicationIsKilled(t *testing.T) {
	temporaryDirectory := t.TempDir()
	fixture := writeBSPDXPublishRuntimeFixture(t, temporaryDirectory)
	publishCommand := compilePublishRuntimeCommand(t, "BeaverPDX", "RRBS", fixture.rawConfiguration, fixture.paths)
	checkpointPath := filepath.Join(temporaryDirectory, "methylation-publication-checkpoint")
	toolDirectory, fakeOtterPath, otterLogPath := writePublishRuntimeTools(t, temporaryDirectory)

	runInterruptedPublishRuntimeCommand(t, publishCommand, toolDirectory, fakeOtterPath, otterLogPath, checkpointPath, "methylation")
	assertPublishRuntimeArtifacts(t, fixture.resultsDirectory, []string{
		"pdx/graft/S01_graft_Filtered.bam",
		"pdx/graft/S01_graft_Filtered.bam.bai",
		"pdx/graft/classification.tsv",
		"pdx/graft/bismark_summary_report.html",
		"methylation/methrix_data.h5",
	})
	assertPublishRuntimeNoManifest(t, fixture.resultsDirectory)

	runPublishRuntimeCommand(t, publishCommand, publishRuntimeEnvironment(toolDirectory, fakeOtterPath, otterLogPath, "", ""))
	assertPublishRuntimeArtifacts(t, fixture.resultsDirectory, []string{
		"pdx/graft/S01_graft_Filtered.bam",
		"pdx/graft/S01_graft_Filtered.bam.bai",
		"pdx/graft/classification.tsv",
		"methylation/methrix_data.h5",
		"qc/qc_summary.xlsx",
		"artifacts.json",
	})
	assertPublishRuntimeManifestAndIdempotency(t, publishCommand, toolDirectory, fakeOtterPath, otterLogPath, fixture.resultsDirectory, "publish\nverify\nverify\n")
}

func TestBeaverRNASEQPDXPublishCommandRecoversAfterSplicingDirectoryPublicationIsKilled(t *testing.T) {
	temporaryDirectory := t.TempDir()
	fixture := writeRNAPDXPublishRuntimeFixture(t, temporaryDirectory)
	publishCommand := compilePublishRuntimeCommand(t, "BeaverRNASEQPDX", "RNASEQ", fixture.rawConfiguration, fixture.paths)
	checkpointPath := filepath.Join(temporaryDirectory, "rna-pdx-splicing-publication-checkpoint")
	toolDirectory, fakeOtterPath, otterLogPath := writePublishRuntimeTools(t, temporaryDirectory)

	runInterruptedPublishRuntimeCommand(t, publishCommand, toolDirectory, fakeOtterPath, otterLogPath, checkpointPath, "splicing")
	assertPublishRuntimeArtifacts(t, fixture.resultsDirectory, []string{
		"pdx/graft/S01_graft_Filtered.bam",
		"pdx/graft/S01_graft_Filtered.bam.bai",
		"pdx/graft/classification.tsv",
		"splicing/outcome.json",
		"splicing/files/events.tsv",
	})
	assertPublishRuntimeNoManifest(t, fixture.resultsDirectory)

	runPublishRuntimeCommand(t, publishCommand, publishRuntimeEnvironment(toolDirectory, fakeOtterPath, otterLogPath, "", ""))
	assertPublishRuntimeArtifacts(t, fixture.resultsDirectory, []string{
		"pdx/graft/S01_graft_Filtered.bam",
		"pdx/graft/S01_graft_Filtered.bam.bai",
		"pdx/graft/classification.tsv",
		"splicing/outcome.json",
		"splicing/files/events.tsv",
		"methylation/matrix_count.txt",
		"methylation/matrix_norm.txt",
		"qc/qc_summary.xlsx",
		"artifacts.json",
	})
	assertPublishRuntimeManifestAndIdempotency(t, publishCommand, toolDirectory, fakeOtterPath, otterLogPath, fixture.resultsDirectory, "publish\nverify\nverify\n")
}

type publishRuntimeFixture struct {
	rawConfiguration map[string]any
	paths            map[string]string
	resultsDirectory string
}

func writeRNAPublishRuntimeFixture(t *testing.T, temporaryDirectory string) publishRuntimeFixture {
	t.Helper()
	runDirectory, workDirectory, resultsDirectory, configurationPath := publishRuntimeDirectories(t, temporaryDirectory)
	betaMatrixDirectory := filepath.Join(resultsDirectory, "methylation")
	qualityControlDirectory := filepath.Join(resultsDirectory, "qc")
	bsmapDirectory := filepath.Join(workDirectory, "bsmap")
	splicingDirectory := filepath.Join(bsmapDirectory, "RNASplicing")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(betaMatrixDirectory, "matrix_count.txt"), "gene\tS01\nGeneA\t10\n")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(betaMatrixDirectory, "matrix_norm.txt"), "gene\tS01\nGeneA\t9.5\n")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(qualityControlDirectory, "qc_summary.xlsx"), "xlsx payload\n")
	writeTypedSplicingOutcome(t, splicingDirectory)
	return publishRuntimeFixture{
		rawConfiguration: map[string]any{"directories": map[string]any{
			"beta_matrix": betaMatrixDirectory,
			"qc_summary":  qualityControlDirectory,
			"bsmap":       map[string]any{"main": bsmapDirectory},
		}},
		paths:            map[string]string{"config": configurationPath, "work": workDirectory, "results": resultsDirectory, "run": runDirectory},
		resultsDirectory: resultsDirectory,
	}
}

func writeBSPDXPublishRuntimeFixture(t *testing.T, temporaryDirectory string) publishRuntimeFixture {
	t.Helper()
	runDirectory, workDirectory, resultsDirectory, configurationPath := publishRuntimeDirectories(t, temporaryDirectory)
	bsmapDirectory := filepath.Join(workDirectory, "bsmap")
	methylationCallDirectory := filepath.Join(workDirectory, "mCall")
	qualityControlDirectory := filepath.Join(resultsDirectory, "qc")
	writePDXFilteredBAMPair(t, filepath.Join(bsmapDirectory, "Filtered_bams"))
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(methylationCallDirectory, "methrixh5", "methrix_data.h5"), "hdf5 payload\n")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(bsmapDirectory, "graft", "bismark_summary_report.html"), "<html>summary</html>\n")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(qualityControlDirectory, "qc_summary.xlsx"), "xlsx payload\n")
	return publishRuntimeFixture{
		rawConfiguration: map[string]any{
			"directories": map[string]any{"bsmap": map[string]any{"main": bsmapDirectory}, "methylation_call": methylationCallDirectory, "qc_summary": qualityControlDirectory},
			"workflow":    map[string]any{"species": map[string]any{"graft": "graft"}},
		},
		paths:            map[string]string{"config": configurationPath, "work": workDirectory, "results": resultsDirectory, "run": runDirectory},
		resultsDirectory: resultsDirectory,
	}
}

func writeRNAPDXPublishRuntimeFixture(t *testing.T, temporaryDirectory string) publishRuntimeFixture {
	t.Helper()
	fixture := writeRNAPublishRuntimeFixture(t, temporaryDirectory)
	bsmapDirectory := fixture.rawConfiguration["directories"].(map[string]any)["bsmap"].(map[string]any)["main"].(string)
	writePDXFilteredBAMPair(t, filepath.Join(bsmapDirectory, "Filtered_bams"))
	fixture.rawConfiguration["workflow"] = map[string]any{"species": map[string]any{"graft": "graft"}}
	return fixture
}

func publishRuntimeDirectories(t *testing.T, temporaryDirectory string) (string, string, string, string) {
	t.Helper()
	runDirectory := filepath.Join(temporaryDirectory, "run")
	workDirectory := filepath.Join(runDirectory, "work")
	resultsDirectory := filepath.Join(runDirectory, "results")
	configurationPath := filepath.Join(runDirectory, "run.yaml")
	writeBeaverBSPublishRuntimeFile(t, configurationPath, "schema_version: otter.run/v1\n")
	return runDirectory, workDirectory, resultsDirectory, configurationPath
}

func writeTypedSplicingOutcome(t *testing.T, splicingDirectory string) {
	t.Helper()
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(splicingDirectory, "events.tsv"), "event\tvalue\nSE\t1\n")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(splicingDirectory, "splicing-outcome.json"), fmt.Sprintf(`{
  "schema_version": "otter.rna-splicing-outcome/v1",
  "status": "produced",
  "source_root": %q,
  "artifact_paths": ["events.tsv"]
}
`, splicingDirectory))
}

func writePDXFilteredBAMPair(t *testing.T, filteredDirectory string) {
	t.Helper()
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(filteredDirectory, "S01_graft_Filtered.bam"), "bam payload\n")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(filteredDirectory, "S01_graft_Filtered.bam.bai"), "bai payload\n")
}

func compilePublishRuntimeCommand(t *testing.T, workflowName, mode string, rawConfiguration map[string]any, paths map[string]string) string {
	t.Helper()
	workflow, err := spec.Load(filepath.Join(repositoryRootForTest(t), "workflows", workflowName, "publish.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, &compiler.Context{Raw: rawConfiguration, Workflow: compiler.WorkflowContext{Mode: mode}, Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 || len(plan.Tasks[0].Steps) != 1 {
		t.Fatalf("expected one compiled %s publish command, got %#v", workflowName, plan.Tasks)
	}
	return plan.Tasks[0].Steps[0].Command
}

func writePublishRuntimeTools(t *testing.T, temporaryDirectory string) (string, string, string) {
	t.Helper()
	toolDirectory := filepath.Join(temporaryDirectory, "tools")
	fakeOtterPath := filepath.Join(toolDirectory, "otter")
	otterLogPath := filepath.Join(temporaryDirectory, "otter.log")
	writeBeaverBSPublishRuntimeExecutable(t, fakeOtterPath, `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}:${2:-}" in
  artifact:publish)
    configuration_path="${3:?missing run configuration}"
    declaration_path="${4:?missing declaration path}"
    results_root="$(dirname "$configuration_path")/results"
    test -s "$declaration_path"
    test -d "$results_root"
    printf '{"schema_version":"otter.artifacts/v1"}\n' > "$results_root/artifacts.json"
    chmod 444 "$results_root/artifacts.json"
    printf 'publish\n' >> "${OTTER_TEST_LOG:?missing OTTER_TEST_LOG}"
    ;;
  artifact:verify)
    configuration_path="${3:?missing run configuration}"
    manifest_path="$(dirname "$configuration_path")/results/artifacts.json"
    test -f "$manifest_path"
    manifest_mode="$(stat -c '%a' "$manifest_path" 2>/dev/null || stat -f '%Lp' "$manifest_path")"
    test "$manifest_mode" = 444
    printf 'verify\n' >> "${OTTER_TEST_LOG:?missing OTTER_TEST_LOG}"
    ;;
  *)
    exit 2
    ;;
esac
`)
	writeBeaverBSPublishRuntimeExecutable(t, filepath.Join(toolDirectory, "mv"), `#!/usr/bin/env bash
set -euo pipefail
source_path="${1:?missing move source}"
destination_path="${2:?missing move destination}"
/bin/mv "$source_path" "$destination_path"
if [ -n "${OTTER_TEST_BLOCK_AFTER_MOVE_BASENAME:-}" ] && [ "$(basename "$source_path")" = "$OTTER_TEST_BLOCK_AFTER_MOVE_BASENAME" ]; then
  printf 'published\n' > "${OTTER_TEST_CHECKPOINT:?missing OTTER_TEST_CHECKPOINT}"
  while :; do sleep 1; done
fi
`)
	writeBeaverBSPublishRuntimeExecutable(t, filepath.Join(toolDirectory, "samtools"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  quickcheck) exit 0 ;;
  view) printf '7\n' ;;
  *) exit 2 ;;
esac
`)
	return toolDirectory, fakeOtterPath, otterLogPath
}

func runInterruptedPublishRuntimeCommand(t *testing.T, publishCommand, toolDirectory, fakeOtterPath, otterLogPath, checkpointPath, moveBasename string) {
	t.Helper()
	command := exec.Command("bash", "-c", publishCommand)
	command.Env = publishRuntimeEnvironment(toolDirectory, fakeOtterPath, otterLogPath, checkpointPath, moveBasename)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("start interrupted publish command: %v", err)
	}
	waitForPublishRuntimeCheckpoint(t, command, checkpointPath)
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill interrupted publish process group: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("interrupted publish command unexpectedly succeeded")
	}
}

func runPublishRuntimeCommand(t *testing.T, publishCommand string, environment []string) {
	t.Helper()
	command := exec.Command("bash", "-c", publishCommand)
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run publish command: %v\n%s", err, output)
	}
}

func assertPublishRuntimeManifestAndIdempotency(t *testing.T, publishCommand, toolDirectory, fakeOtterPath, otterLogPath, resultsDirectory, expectedLog string) {
	t.Helper()
	manifestInfo, err := os.Stat(filepath.Join(resultsDirectory, "artifacts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if manifestInfo.Mode().Perm() != 0o444 {
		t.Fatalf("published manifest permissions = %o, want 444", manifestInfo.Mode().Perm())
	}
	runPublishRuntimeCommand(t, publishCommand, publishRuntimeEnvironment(toolDirectory, fakeOtterPath, otterLogPath, "", ""))
	otterLog, err := os.ReadFile(otterLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(otterLog) != expectedLog {
		t.Fatalf("unexpected publisher and verifier sequence: %q", otterLog)
	}
}

func assertPublishRuntimeArtifacts(t *testing.T, resultsDirectory string, relativePaths []string) {
	t.Helper()
	for _, relativePath := range relativePaths {
		artifactPath := filepath.Join(resultsDirectory, relativePath)
		info, err := os.Stat(artifactPath)
		if err != nil || info.IsDir() || info.Size() == 0 {
			t.Fatalf("missing regular non-empty published artifact %s: info=%#v error=%v", relativePath, info, err)
		}
	}
}

func assertPublishRuntimeNoManifest(t *testing.T, resultsDirectory string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(resultsDirectory, "artifacts.json")); !os.IsNotExist(err) {
		t.Fatalf("partial publication must not create a manifest, stat error=%v", err)
	}
}

func publishRuntimeEnvironment(toolDirectory, fakeOtterPath, otterLogPath, checkpointPath, moveBasename string) []string {
	environment := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") || strings.HasPrefix(entry, "OTTER=") || strings.HasPrefix(entry, "OTTER_TEST_") {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment,
		"OTTER="+fakeOtterPath,
		"OTTER_TEST_LOG="+otterLogPath,
		"PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if checkpointPath != "" {
		environment = append(environment, "OTTER_TEST_CHECKPOINT="+checkpointPath)
	}
	if moveBasename != "" {
		environment = append(environment, "OTTER_TEST_BLOCK_AFTER_MOVE_BASENAME="+moveBasename)
	}
	return environment
}

func waitForPublishRuntimeCheckpoint(t *testing.T, command *exec.Cmd, checkpointPath string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(checkpointPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	_ = command.Wait()
	t.Fatal("publish command did not reach its directory-publication checkpoint")
}
