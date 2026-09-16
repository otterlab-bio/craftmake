package compiler_test

import (
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

func TestBeaverBSPublishCommandRecoversAfterStagingProcessIsKilled(t *testing.T) {
	temporaryDirectory := t.TempDir()
	runDirectory := filepath.Join(temporaryDirectory, "run")
	workDirectory := filepath.Join(runDirectory, "work")
	resultsDirectory := filepath.Join(runDirectory, "results")
	configurationPath := filepath.Join(runDirectory, "run.yaml")
	methylationCallDirectory := filepath.Join(workDirectory, "mCall")
	bsmapDirectory := filepath.Join(workDirectory, "bsmap")
	qualityControlDirectory := filepath.Join(resultsDirectory, "qc")

	writeBeaverBSPublishRuntimeFile(t, configurationPath, "schema_version: otter.run/v1\n")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(methylationCallDirectory, "methrixh5", "methrix_data.h5"), "hdf5 payload\n")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(bsmapDirectory, "graft", "bismark_summary_report.html"), "<html>summary</html>\n")
	writeBeaverBSPublishRuntimeFile(t, filepath.Join(qualityControlDirectory, "qc_summary.xlsx"), "xlsx payload\n")

	workflow := loadBeaverBSPublishWorkflow(t)
	plan, err := compiler.Compile(workflow, &compiler.Context{
		Raw: map[string]any{
			"directories": map[string]any{
				"methylation_call": methylationCallDirectory,
				"bsmap": map[string]any{
					"main": bsmapDirectory,
				},
				"qc_summary": qualityControlDirectory,
			},
			"workflow": map[string]any{
				"species": map[string]any{
					"graft": "graft",
				},
			},
		},
		Workflow: compiler.WorkflowContext{Mode: "RRBS"},
		Paths: map[string]string{
			"config":  configurationPath,
			"work":    workDirectory,
			"results": resultsDirectory,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 || len(plan.Tasks[0].Steps) != 1 {
		t.Fatalf("expected one compiled publish command, got %#v", plan.Tasks)
	}
	publishCommand := plan.Tasks[0].Steps[0].Command

	toolDirectory := filepath.Join(temporaryDirectory, "tools")
	checkpointPath := filepath.Join(temporaryDirectory, "staging-checkpoint")
	otterLogPath := filepath.Join(temporaryDirectory, "otter.log")
	fakeOtterPath := filepath.Join(toolDirectory, "otter")
	writeBeaverBSPublishRuntimeExecutable(t, fakeOtterPath, `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != "artifact" ]; then
  exit 2
fi
case "${2:-}" in
  publish)
    configuration_path="${3:?missing run configuration}"
    declaration_path="${4:?missing declaration path}"
    results_root="$(dirname "$configuration_path")/results"
    test -s "$declaration_path"
    test -s "$results_root/methylation/methrix_data.h5"
    test -s "$results_root/methylation/bismark_summary_report.html"
    test -s "$results_root/qc/qc_summary.xlsx"
    printf '{"schema_version":"otter.artifacts/v1"}\n' > "$results_root/artifacts.json"
    chmod 444 "$results_root/artifacts.json"
    printf 'publish\n' >> "${OTTER_TEST_LOG:?missing OTTER_TEST_LOG}"
    ;;
  verify)
    configuration_path="${3:?missing run configuration}"
    manifest_path="$(dirname "$configuration_path")/results/artifacts.json"
    test -f "$manifest_path"
    test "$(stat -c '%a' "$manifest_path")" = 444
    printf 'verify\n' >> "${OTTER_TEST_LOG:?missing OTTER_TEST_LOG}"
    ;;
  *)
    exit 2
    ;;
esac
`)
	writeBeaverBSPublishRuntimeExecutable(t, filepath.Join(toolDirectory, "cp"), `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "--" ]; then
  shift
fi
source_path="${1:?missing copy source}"
destination_path="${2:?missing copy destination}"
/bin/cp -- "$source_path" "$destination_path"
if [ "${OTTER_TEST_BLOCK_STAGING:-}" = "1" ] && [ "$(basename "$source_path")" = "methrix_data.h5" ]; then
  printf 'staged\n' > "${OTTER_TEST_CHECKPOINT:?missing OTTER_TEST_CHECKPOINT}"
  while :; do sleep 1; done
fi
`)

	interruptedCommand := exec.Command("bash", "-c", publishCommand)
	interruptedCommand.Env = beaverBSPublishRuntimeEnvironment(toolDirectory, fakeOtterPath, checkpointPath, otterLogPath, true)
	interruptedCommand.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := interruptedCommand.Start(); err != nil {
		t.Fatalf("start interrupted publish command: %v", err)
	}
	waitForBeaverBSPublishRuntimeCheckpoint(t, interruptedCommand, checkpointPath)
	if err := syscall.Kill(-interruptedCommand.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill staging publish process group: %v", err)
	}
	if err := interruptedCommand.Wait(); err == nil {
		t.Fatal("interrupted publish command unexpectedly succeeded")
	}

	stagingRoots, err := filepath.Glob(filepath.Join(resultsDirectory, ".publish-staging", "bs.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stagingRoots) != 1 {
		t.Fatalf("expected one retained interrupted staging directory, got %#v", stagingRoots)
	}
	if _, err := os.Stat(filepath.Join(resultsDirectory, "methylation")); !os.IsNotExist(err) {
		t.Fatalf("interrupted staging must not publish methylation results, stat error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(resultsDirectory, "artifacts.json")); !os.IsNotExist(err) {
		t.Fatalf("interrupted staging must not publish a manifest, stat error=%v", err)
	}

	retryCommand := exec.Command("bash", "-c", publishCommand)
	retryCommand.Env = beaverBSPublishRuntimeEnvironment(toolDirectory, fakeOtterPath, "", otterLogPath, false)
	if output, err := retryCommand.CombinedOutput(); err != nil {
		t.Fatalf("retry publish command: %v\n%s", err, output)
	}
	for _, relativePath := range []string{
		"methylation/methrix_data.h5",
		"methylation/bismark_summary_report.html",
		"qc/qc_summary.xlsx",
		"artifacts.json",
	} {
		if info, err := os.Stat(filepath.Join(resultsDirectory, relativePath)); err != nil || info.IsDir() || info.Size() == 0 {
			t.Fatalf("retry did not publish regular non-empty artifact %s: info=%#v error=%v", relativePath, info, err)
		}
	}
	manifestInfo, err := os.Stat(filepath.Join(resultsDirectory, "artifacts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if manifestInfo.Mode().Perm() != 0o444 {
		t.Fatalf("published manifest permissions = %o, want 444", manifestInfo.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(workDirectory, "publish", "beaverbs-artifacts.json")); err != nil {
		t.Fatalf("retry did not atomically publish declarations: %v", err)
	}

	idempotentCommand := exec.Command("bash", "-c", publishCommand)
	idempotentCommand.Env = beaverBSPublishRuntimeEnvironment(toolDirectory, fakeOtterPath, "", otterLogPath, false)
	if output, err := idempotentCommand.CombinedOutput(); err != nil {
		t.Fatalf("idempotent publish verification command: %v\n%s", err, output)
	}
	otterLog, err := os.ReadFile(otterLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(otterLog) != "publish\nverify\nverify\n" {
		t.Fatalf("unexpected publisher and verifier sequence: %q", otterLog)
	}
}

func loadBeaverBSPublishWorkflow(t *testing.T) *spec.WorkflowSpec {
	t.Helper()
	workflow, err := spec.Load(filepath.Join(repositoryRootForTest(t), "workflows", "BeaverBS", "publish.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return workflow
}

func writeBeaverBSPublishRuntimeFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeBeaverBSPublishRuntimeExecutable(t *testing.T, path string, contents string) {
	t.Helper()
	writeBeaverBSPublishRuntimeFile(t, path, contents)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func beaverBSPublishRuntimeEnvironment(toolDirectory, fakeOtterPath, checkpointPath, otterLogPath string, blockStaging bool) []string {
	environment := make([]string, 0, len(os.Environ())+3)
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
	if blockStaging {
		environment = append(environment,
			"OTTER_TEST_BLOCK_STAGING=1",
			"OTTER_TEST_CHECKPOINT="+checkpointPath,
		)
	}
	return environment
}

func waitForBeaverBSPublishRuntimeCheckpoint(t *testing.T, command *exec.Cmd, checkpointPath string) {
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
	t.Fatal("publish command did not reach its staged artifact checkpoint")
}
