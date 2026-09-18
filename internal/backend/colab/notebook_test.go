package colab

import (
	"context"
	"strings"
	"testing"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestBuildNotebookHasBootstrapOneCellPerStepAndFinalizer(t *testing.T) {
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 2, WorkDirectory: "/host/work", TempDirectory: "/host/tmp", RuntimeDirectory: "/host/runtime", ResultPath: "/host/runtime/result.json", Steps: []protocol.StepManifest{{Index: 0, Name: "compile", Command: "make", Env: map[string]string{"MODE": "release"}}, {Index: 1, Name: "test", Command: "go test ./..."}}}
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
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1, WorkDirectory: "/content/drive/MyDrive/proj/work", TempDirectory: "/content/tmp", RuntimeDirectory: "/content/drive/MyDrive/proj/runtime", ResultPath: "/content/drive/MyDrive/proj/runtime/result.json", Steps: []protocol.StepManifest{{Index: 0, Name: "s", Command: "echo hi"}}}
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
	return "CRAFTMAKE_TASK_RESULT_BEGIN\n{\"protocol_version\":2,\"run_id\":\"run-1\",\"task_id\":\"task-1\",\"attempt\":1,\"status\":\"succeeded\",\"steps\":[{\"index\":0,\"name\":\"compile\",\"started_at\":\"2025-01-01T00:00:00Z\",\"finished_at\":\"2025-01-01T00:00:01Z\",\"exit_code\":0,\"stdout_path\":\"/content/craftmake/runtime/step-0.stdout\",\"stderr_path\":\"/content/craftmake/runtime/step-0.stderr\"}]}\nCRAFTMAKE_TASK_RESULT_END\n", nil
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
	manifest := &protocol.TaskManifest{RunID: "run-1", TaskID: "task-1", RuntimeDirectory: "/tmp/task-1", Steps: []protocol.StepManifest{{Index: 0, Name: "compile", StdoutPath: "/tmp/stdout", StderrPath: "/tmp/stderr"}}}
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
