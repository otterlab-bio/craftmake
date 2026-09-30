package colab

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestBuildNotebookHasBootstrapOneCellPerStepAndFinalizer(t *testing.T) {
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 2, WorkDirectory: "/host/work", TempDirectory: "/host/tmp", RuntimeDirectory: "/host/runtime", ResultPath: "/host/runtime/result.json", Steps: []protocol.StepManifest{{Index: 1, Name: "compile", Command: "make", Env: map[string]string{"MODE": "release"}}, {Index: 2, Name: "test", Command: "go test ./..."}}}
	notebook, err := BuildNotebook(manifest, RemoteTaskMapping{WorkDirectory: "/content/craftmake/work", TempDirectory: "/content/craftmake/tmp", RuntimeDirectory: "/content/craftmake/runtime", ResultPath: "/content/craftmake/runtime/result.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(notebook.Cells) != 4 {
		t.Fatalf("expected bootstrap + 2 steps + finalizer, got %d", len(notebook.Cells))
	}
	if notebook.Cells[1].CellType != "code" || !strings.HasPrefix(notebook.Cells[1].Source, "%%bash\n") {
		t.Fatalf("expected bash cell: %#v", notebook.Cells[1])
	}
	if !strings.Contains(notebook.Cells[1].Source, "CRAFTMAKE_WORK='/content/craftmake/work'") {
		t.Fatalf("missing mapped work directory: %s", notebook.Cells[1].Source)
	}
	if !strings.Contains(notebook.Cells[3].Source, "CRAFTMAKE_TASK_RESULT_BEGIN") {
		t.Fatalf("missing result sentinel")
	}
	if strings.Contains(notebook.Cells[0].Source, "token") {
		t.Fatalf("bootstrap must not contain credentials")
	}
}

func TestDecodeTaskResultUsesSentinelAndProtocolVersion(t *testing.T) {
	output := "noise\nCRAFTMAKE_TASK_RESULT_BEGIN\n{\"protocol_version\":2,\"run_id\":\"run-1\",\"task_id\":\"task-1\",\"attempt\":1,\"status\":\"succeeded\",\"steps\":[]}\nCRAFTMAKE_TASK_RESULT_END\n"
	result, err := DecodeTaskResult(output)
	if err != nil || result.TaskID != "task-1" || result.Status != "succeeded" {
		t.Fatalf("unexpected result %#v, %v", result, err)
	}
	if _, err := DecodeTaskResult(strings.Replace(output, `"protocol_version":2`, `"protocol_version":99`, 1)); err == nil {
		t.Fatal("expected protocol version rejection")
	}
}
func TestBootstrapMountsDriveAndFinalizerFlushes(t *testing.T) {
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1, WorkDirectory: "/content/drive/MyDrive/proj/work", TempDirectory: "/content/tmp", RuntimeDirectory: "/content/drive/MyDrive/proj/runtime", ResultPath: "/content/drive/MyDrive/proj/runtime/result.json", Steps: []protocol.StepManifest{{Index: 1, Name: "s", Command: "echo hi"}}}
	notebook, err := BuildNotebook(manifest, RemoteTaskMapping{WorkDirectory: "/content/drive/MyDrive/proj/work", TempDirectory: "/content/tmp", RuntimeDirectory: "/content/drive/MyDrive/proj/runtime", ResultPath: "/content/drive/MyDrive/proj/runtime/result.json"})
	if err != nil {
		t.Fatal(err)
	}
	// bootstrap (cell 0) must inject drive.mount when the work dir is on Drive.
	if !strings.Contains(notebook.Cells[0].Source, "drive.mount") {
		t.Fatalf("bootstrap missing drive.mount: %s", notebook.Cells[0].Source)
	}
	// finalizer (last cell) must flush Drive writes before the instance is released.
	finalizer := notebook.Cells[len(notebook.Cells)-1].Source
	if !strings.Contains(finalizer, "flush_and_unmount") {
		t.Fatalf("finalizer missing flush_and_unmount: %s", finalizer)
	}
}

type fakeControlPlane struct{ acquired, released int }

func (f *fakeControlPlane) AcquireRuntime(context.Context, RuntimeRequest) (Runtime, error) {
	f.acquired++
	return Runtime{ID: "runtime-1"}, nil
}
func (f *fakeControlPlane) ReleaseRuntime(context.Context, Runtime) error         { f.released++; return nil }
func (f *fakeControlPlane) ListAssignments(context.Context) ([]Assignment, error) { return nil, nil }

type fakeNotebookExecutor struct{}

func (fakeNotebookExecutor) ExecuteNotebook(context.Context, Runtime, []byte) (string, error) {
	return "CRAFTMAKE_TASK_RESULT_BEGIN\n{\"protocol_version\":2,\"run_id\":\"run-1\",\"task_id\":\"task-1\",\"attempt\":1,\"status\":\"succeeded\",\"steps\":[{\"index\":1,\"name\":\"compile\",\"started_at\":\"2025-01-01T00:00:00Z\",\"finished_at\":\"2025-01-01T00:00:01Z\",\"exit_code\":0,\"stdout_path\":\"/content/craftmake/runtime/step-1.stdout\",\"stderr_path\":\"/content/craftmake/runtime/step-1.stderr\"}]}\nCRAFTMAKE_TASK_RESULT_END\n", nil
}

type fakeMaterializer struct{ paths [][2]string }

func (f *fakeMaterializer) Materialize(_ context.Context, remote, local string) error {
	f.paths = append(f.paths, [2]string{remote, local})
	return nil
}

func TestBackendUsesOneRuntimeAndMaterializesLogs(t *testing.T) {
	control := &fakeControlPlane{}
	materializer := &fakeMaterializer{}
	colabBackend := &Backend{Control: control, Executor: fakeNotebookExecutor{}, Materializer: materializer, Config: Config{RemoteRoot: "/content/craftmake"}}
	if err := colabBackend.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	manifest := &protocol.TaskManifest{RunID: "run-1", TaskID: "task-1", RuntimeDirectory: "/tmp/task-1", Steps: []protocol.StepManifest{{Index: 1, Name: "compile", StdoutPath: "/tmp/stdout", StderrPath: "/tmp/stderr"}}}
	if _, err := colabBackend.RunSubmission(context.Background(), "submission-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{manifest}}); err != nil {
		t.Fatal(err)
	}
	if err := colabBackend.EndRun(context.Background(), backendpkg.RunOutcome{RunID: "run-1", Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if control.acquired != 1 || control.released != 1 {
		t.Fatalf("expected one runtime lifecycle, got %d/%d", control.acquired, control.released)
	}
	if len(materializer.paths) != 2 || materializer.paths[0][1] != "/tmp/stdout" || materializer.paths[1][1] != "/tmp/stderr" {
		t.Fatalf("unexpected materialization: %#v", materializer.paths)
	}
}

func TestPathMapperRejectsOutsideRoot(t *testing.T) {
	mapper := PathMapper{HostRoot: "/repo", RemoteRoot: "/content/work"}
	mapped, err := mapper.Map("/repo/src/main.go")
	if err != nil || mapped != "/content/work/src/main.go" {
		t.Fatalf("unexpected mapped path %q, %v", mapped, err)
	}
	if _, err := mapper.Map("/other/file"); err == nil {
		t.Fatal("expected outside root rejection")
	}
}

// writeTestFile is a small helper for the executable finalizer test.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFinalizerCellUsesManifestStepIndices guards the regression where the
// finalizer iterated 0..len(steps)-1 while the %%bash cells wrote artifacts
// keyed by the manifest's 1-based step index. That mismatch reported failing
// steps as succeeded and pointed the result at nonexistent log files.
func TestFinalizerCellUsesManifestStepIndices(t *testing.T) {
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1,
		Steps: []protocol.StepManifest{{Index: 1, Name: "a", Command: "true"}, {Index: 2, Name: "b", Command: "true"}}}
	notebook, err := BuildNotebook(manifest, RemoteTaskMapping{WorkDirectory: "/w", TempDirectory: "/t", RuntimeDirectory: "/r", ResultPath: "/r/result.json"})
	if err != nil {
		t.Fatal(err)
	}
	finalizer := notebook.Cells[len(notebook.Cells)-1].Source
	if !strings.Contains(finalizer, "step_indices = [1, 2]") {
		t.Fatalf("finalizer must iterate the manifest step indices:\n%s", finalizer)
	}
	if strings.Contains(finalizer, "range(step_count)") {
		t.Fatalf("finalizer must not assume a 0-based step range:\n%s", finalizer)
	}
	// Step cells are emitted in manifest order and must write artifacts under
	// the same 1-based key the finalizer reads.
	for offset, step := range manifest.Steps {
		cell := notebook.Cells[1+offset]
		if !strings.HasPrefix(cell.Source, "%%bash") {
			t.Fatalf("cell %d is not a step cell: %#v", 1+offset, cell)
		}
		key := "step-" + strconv.Itoa(step.Index)
		for _, suffix := range []string{".exit", ".stdout", ".stderr"} {
			if !strings.Contains(cell.Source, key+suffix) {
				t.Fatalf("step cell missing %s%s:\n%s", key, suffix, cell.Source)
			}
		}
	}
}

// TestFinalizerCellReportsFailedStep executes the generated finalizer cell with
// the real interpreter and checks that a non-zero step exit code fails the task
// and that step log paths resolve to the files the %%bash cell actually wrote.
func TestFinalizerCellReportsFailedStep(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, "runtime")
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1,
		Steps: []protocol.StepManifest{{Index: 1, Name: "step-1", Command: "exit 7"}}}
	mapping := RemoteTaskMapping{WorkDirectory: filepath.Join(dir, "work"), TempDirectory: filepath.Join(dir, "tmp"), RuntimeDirectory: runtimeDir, ResultPath: filepath.Join(runtimeDir, "result.json")}
	notebook, err := BuildNotebook(manifest, mapping)
	if err != nil {
		t.Fatal(err)
	}
	finalizer := notebook.Cells[len(notebook.Cells)-1].Source

	writeTestFile(t, filepath.Join(runtimeDir, "step-1.exit"), "7\n")
	writeTestFile(t, filepath.Join(runtimeDir, "step-1.stdout"), "boom\n")
	writeTestFile(t, filepath.Join(runtimeDir, "step-1.stderr"), "warn\n")
	writeTestFile(t, filepath.Join(runtimeDir, "step-1.start"), "1700000000\n")
	writeTestFile(t, filepath.Join(runtimeDir, "step-1.end"), "1700000001\n")

	script := filepath.Join(dir, "finalizer.py")
	writeTestFile(t, script, finalizer)
	out, err := exec.Command(python, script).CombinedOutput()
	if err != nil {
		t.Fatalf("finalizer failed: %v\n%s", err, out)
	}
	result, err := DecodeTaskResult(string(out))
	if err != nil {
		t.Fatalf("decode finalizer output: %v\n%s", err, out)
	}
	if result.Status != "failed" || result.ExitCode != 7 {
		t.Fatalf("expected failed/7, got %q/%d", result.Status, result.ExitCode)
	}
	if len(result.Steps) != 1 || result.Steps[0].Index != 1 || result.Steps[0].ExitCode != 7 {
		t.Fatalf("unexpected steps: %#v", result.Steps)
	}
	if !strings.HasSuffix(result.Steps[0].StdoutPath, filepath.Join("runtime", "step-1.stdout")) {
		t.Fatalf("stdout path must key off the manifest index: %q", result.Steps[0].StdoutPath)
	}
	if result.Steps[0].StartedAt.IsZero() {
		t.Error("expected the step start timestamp to be recorded")
	}
	if !strings.Contains(string(out), "[step-1 stdout]\nboom") {
		t.Fatalf("step stdout was not surfaced:\n%s", out)
	}
	if _, err := os.Stat(mapping.ResultPath); err != nil {
		t.Fatalf("finalizer did not write result.json: %v", err)
	}
}

// TestFinalizerCellMissingExitFileFails guards against silently reporting a
// step that never recorded an exit code as successful.
func TestFinalizerCellMissingExitFileFails(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, "runtime")
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1,
		Steps: []protocol.StepManifest{{Index: 1, Name: "step-1", Command: "true"}}}
	mapping := RemoteTaskMapping{WorkDirectory: filepath.Join(dir, "work"), TempDirectory: filepath.Join(dir, "tmp"), RuntimeDirectory: runtimeDir, ResultPath: filepath.Join(runtimeDir, "result.json")}
	notebook, err := BuildNotebook(manifest, mapping)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "finalizer.py")
	writeTestFile(t, script, notebook.Cells[len(notebook.Cells)-1].Source)
	out, err := exec.Command(python, script).CombinedOutput()
	if err != nil {
		t.Fatalf("finalizer failed: %v\n%s", err, out)
	}
	result, err := DecodeTaskResult(string(out))
	if err != nil {
		t.Fatalf("decode finalizer output: %v\n%s", err, out)
	}
	if result.Status != "failed" || result.Steps[0].ExitCode != -1 {
		t.Fatalf("missing exit file must fail the step, got %q/%d", result.Status, result.Steps[0].ExitCode)
	}
}

// TestMaterializeTaskLogsResolvesOneBasedStepIndex guards the second half of the
// index mismatch: the manifest step must be resolved by Index rather than used
// as a slice offset, otherwise the guard skipped every step.
func TestMaterializeTaskLogsResolvesOneBasedStepIndex(t *testing.T) {
	dir := t.TempDir()
	localStdout := filepath.Join(dir, "logs", "step-1.stdout")
	localStderr := filepath.Join(dir, "logs", "step-1.stderr")
	manifest := &protocol.TaskManifest{RunID: "run-1", TaskID: "task-1", Attempt: 1,
		RuntimeDirectory: filepath.Join(dir, "runtime"), ResultPath: filepath.Join(dir, "result.json"),
		Steps: []protocol.StepManifest{{Index: 1, Name: "s", StdoutPath: localStdout, StderrPath: localStderr}}}
	taskResult := &protocol.TaskResult{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1, Status: "succeeded",
		Steps: []protocol.StepResult{{Index: 1, ExitCode: 0, StdoutPath: "/remote/step-1.stdout", StderrPath: "/remote/step-1.stderr"}}}
	output := "[step-1 stdout]\nhello from step\n[step-1 stderr]\nwarn only\nCRAFTMAKE_TASK_RESULT_BEGIN\n"
	colabBackend := &Backend{}
	if err := colabBackend.materializeTaskLogs(context.Background(), taskResult, manifest, RemoteTaskMapping{RuntimeDirectory: manifest.RuntimeDirectory}, output); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.ReadFile(localStdout)
	if err != nil {
		t.Fatalf("stdout log was not materialized: %v", err)
	}
	if !strings.Contains(string(stdout), "hello from step") {
		t.Fatalf("unexpected stdout log: %q", stdout)
	}
	stderr, err := os.ReadFile(localStderr)
	if err != nil {
		t.Fatalf("stderr log was not materialized: %v", err)
	}
	if !strings.Contains(string(stderr), "warn only") {
		t.Fatalf("unexpected stderr log: %q", stderr)
	}
	if _, err := os.Stat(manifest.ResultPath); err != nil {
		t.Fatalf("result.json was not written locally: %v", err)
	}
}
