package local

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/processgroup"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestRunTaskRejectsFutureResultProtocol(t *testing.T) {
	temporaryDirectory := t.TempDir()
	manifest := recoveryTestManifest(t, temporaryDirectory)
	futureResultPath := filepath.Join(temporaryDirectory, "future-result.json")
	writeRecoveryResult(t, futureResultPath, protocol.TaskResult{
		ProtocolVersion: protocol.Version + 1,
		RunID:           manifest.RunID,
		TaskID:          manifest.TaskID,
		Attempt:         manifest.Attempt,
		Status:          "succeeded",
		StartedAt:       time.Now().UTC().Add(-time.Second),
		FinishedAt:      time.Now().UTC(),
		ExitCode:        0,
	})
	fakeTaskRunnerPath := filepath.Join(temporaryDirectory, "fake-task-runner")
	fakeTaskRunnerScript := "#!/bin/sh\ncp " + shellQuoteForRecoveryTest(futureResultPath) + " " + shellQuoteForRecoveryTest(manifest.ResultPath) + "\n"
	if err := os.WriteFile(fakeTaskRunnerPath, []byte(fakeTaskRunnerScript), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := New().runTask(context.Background(), fakeTaskRunnerPath, manifest)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected future protocol rejection, got result=%#v error=%v", result, err)
	}
}

func TestRecoverSubmissionLoadsTerminalTaskResultWithoutInspectingProcess(t *testing.T) {
	temporaryDirectory := t.TempDir()
	manifest := recoveryTestManifest(t, temporaryDirectory)
	processGroupPath := filepath.Join(manifest.RuntimeDirectory, "process-group.pid")
	processGroupRecord := []byte("intentionally-invalid-process-group\n")
	if err := os.WriteFile(processGroupPath, processGroupRecord, 0o644); err != nil {
		t.Fatal(err)
	}
	finishedAt := time.Now().UTC()
	writeRecoveryResult(t, manifest.ResultPath, protocol.TaskResult{
		ProtocolVersion: protocol.MinimumCompatibleVersion,
		RunID:           manifest.RunID,
		TaskID:          manifest.TaskID,
		Attempt:         manifest.Attempt,
		Status:          "succeeded",
		StartedAt:       finishedAt.Add(-time.Second),
		FinishedAt:      finishedAt,
		ExitCode:        0,
	})

	result, err := New().RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "local:submission-a",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, exists := result.Tasks[manifest.TaskID]
	if !exists || outcome.Err != nil || outcome.Result == nil || outcome.Result.TaskResult.Status != "succeeded" {
		t.Fatalf("unexpected recovered outcome: %#v", outcome)
	}
	processGroupRecordAfterRecovery, err := os.ReadFile(processGroupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(processGroupRecordAfterRecovery) != string(processGroupRecord) {
		t.Fatalf("terminal-result recovery modified process group record: got %q, want %q", processGroupRecordAfterRecovery, processGroupRecord)
	}
}

func TestRecoverSubmissionReportsMissingLocalProcessAsInterruptedOutcome(t *testing.T) {
	temporaryDirectory := t.TempDir()
	manifest := recoveryTestManifest(t, temporaryDirectory)

	result, err := New().RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "local:submission-a",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, exists := result.Tasks[manifest.TaskID]
	if !exists || outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "result is unavailable") {
		t.Fatalf("unexpected missing local recovery outcome: %#v", outcome)
	}
}

func TestRecoverSubmissionRejectsMismatchedTaskResultIdentity(t *testing.T) {
	temporaryDirectory := t.TempDir()
	manifest := recoveryTestManifest(t, temporaryDirectory)
	finishedAt := time.Now().UTC()
	writeRecoveryResult(t, manifest.ResultPath, protocol.TaskResult{
		ProtocolVersion: protocol.Version,
		RunID:           manifest.RunID,
		TaskID:          "different-task",
		Attempt:         manifest.Attempt,
		Status:          "succeeded",
		StartedAt:       finishedAt.Add(-time.Second),
		FinishedAt:      finishedAt,
		ExitCode:        0,
	})

	result, err := New().RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "local:submission-a",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if err == nil || !strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("expected mismatched identity error, got %v", err)
	}
	outcome := result.Tasks[manifest.TaskID]
	if outcome.Result != nil || outcome.Err == nil {
		t.Fatalf("unexpected mismatched identity outcome: %#v", outcome)
	}
}

func TestRecoverSubmissionRejectsFutureTaskResultProtocol(t *testing.T) {
	temporaryDirectory := t.TempDir()
	manifest := recoveryTestManifest(t, temporaryDirectory)
	finishedAt := time.Now().UTC()
	writeRecoveryResult(t, manifest.ResultPath, protocol.TaskResult{
		ProtocolVersion: protocol.Version + 1,
		RunID:           manifest.RunID,
		TaskID:          manifest.TaskID,
		Attempt:         manifest.Attempt,
		Status:          "succeeded",
		StartedAt:       finishedAt.Add(-time.Second),
		FinishedAt:      finishedAt,
		ExitCode:        0,
	})

	result, err := New().RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "local:submission-a",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected future protocol rejection, got %v", err)
	}
	outcome := result.Tasks[manifest.TaskID]
	if outcome.Result != nil || outcome.Err == nil {
		t.Fatalf("unexpected future protocol outcome: %#v", outcome)
	}
}

func TestRecoverSubmissionTerminatesOwnedOrphanTaskRunner(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("local orphan recovery requires Linux process groups and /proc")
	}

	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildRecoveryTestBinary(t, binaryPath)

	manifest := recoveryTestManifest(t, temporaryDirectory)
	manifest.TempDirectory = filepath.Join(manifest.RuntimeDirectory, "temp")
	stepStartedPath := filepath.Join(temporaryDirectory, "step-started")
	manifest.Steps = []protocol.StepManifest{
		{
			Index:   1,
			Name:    "Wait for recovery",
			Shell:   "bash",
			Command: "printf 'started\\n' > '" + stepStartedPath + "'\nsleep 30",
		},
	}
	manifestPath := filepath.Join(manifest.RuntimeDirectory, "manifest.json")
	writeRecoveryManifest(t, manifestPath, manifest)

	taskRunnerCommand := exec.Command(binaryPath, "__task-runner", "--manifest", manifestPath)
	taskRunnerCommand.Dir = temporaryDirectory
	processgroup.Configure(taskRunnerCommand)
	if err := taskRunnerCommand.Start(); err != nil {
		t.Fatalf("start real task runner: %v", err)
	}
	processGroupID := taskRunnerCommand.Process.Pid
	processGroupPath := filepath.Join(manifest.RuntimeDirectory, "process-group.pid")
	if err := os.WriteFile(processGroupPath, []byte(strconv.Itoa(processGroupID)+"\n"), 0o644); err != nil {
		_ = processgroup.Terminate(taskRunnerCommand)
		_ = taskRunnerCommand.Wait()
		t.Fatalf("write process group record: %v", err)
	}

	waitResult := make(chan error, 1)
	go func() {
		waitResult <- taskRunnerCommand.Wait()
	}()
	taskRunnerWaited := false
	t.Cleanup(func() {
		if processgroup.GroupExists(processGroupID) {
			_ = processgroup.TerminateGroup(processGroupID)
		}
		if taskRunnerWaited {
			return
		}
		select {
		case <-waitResult:
		case <-time.After(3 * time.Second):
			_ = taskRunnerCommand.Process.Kill()
		}
	})
	waitForRecoveryPath(t, stepStartedPath, 5*time.Second)

	recoveryResult, recoveryErr := New().RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "local:submission-a",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if recoveryErr != nil {
		t.Fatalf("recover real orphan task runner: %v", recoveryErr)
	}
	outcome, exists := recoveryResult.Tasks[manifest.TaskID]
	if !exists || outcome.Result == nil || outcome.Result.TaskResult == nil {
		t.Fatalf("recovery did not return the orphan terminal result: %#v", outcome)
	}
	if outcome.Result.TaskResult.Status != "failed" {
		t.Fatalf("unexpected orphan terminal status %q", outcome.Result.TaskResult.Status)
	}
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "context canceled") {
		t.Fatalf("orphan recovery did not retain the cancellation failure: %#v", outcome)
	}
	if processgroup.GroupExists(processGroupID) {
		t.Fatalf("orphan process group %d is still running", processGroupID)
	}
	if _, err := os.Stat(processGroupPath); !os.IsNotExist(err) {
		t.Fatalf("process group record still exists after recovery: %v", err)
	}

	select {
	case <-waitResult:
		taskRunnerWaited = true
	case <-time.After(3 * time.Second):
		t.Fatal("real task runner was not reaped after orphan recovery")
	}
}

func recoveryTestManifest(t *testing.T, temporaryDirectory string) *protocol.TaskManifest {
	t.Helper()
	runtimeDirectory := filepath.Join(temporaryDirectory, "runtime")
	if err := os.MkdirAll(runtimeDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	return &protocol.TaskManifest{
		ProtocolVersion:  protocol.Version,
		RunID:            "run-a",
		TaskID:           "workflow/phase/task-a",
		JobID:            "task-a",
		Attempt:          1,
		Workflow:         "workflow",
		Phase:            "phase",
		Scope:            "global",
		WorkDirectory:    temporaryDirectory,
		RuntimeDirectory: runtimeDirectory,
		ResultPath:       filepath.Join(runtimeDirectory, "result.json"),
	}
}

func writeRecoveryResult(t *testing.T, resultPath string, result protocol.TaskResult) {
	t.Helper()
	resultData, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, resultData, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRecoveryManifest(t *testing.T, manifestPath string, manifest *protocol.TaskManifest) {
	t.Helper()
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
}

func shellQuoteForRecoveryTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func buildRecoveryTestBinary(t *testing.T, binaryPath string) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve local recovery test path")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
	buildCommand := exec.Command("go", "build", "-o", binaryPath, "./cmd/craftmake")
	buildCommand.Dir = repositoryRoot
	buildOutput, err := buildCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("build craftmake recovery test binary: %v\n%s", err, buildOutput)
	}
}

func waitForRecoveryPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for recovery path %q", path)
}
