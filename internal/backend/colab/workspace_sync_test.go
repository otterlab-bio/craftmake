package colab

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

// fakeSyncExecutor implements NotebookExecutor plus WorkspaceTransport so the
// backend's sync hooks can be observed without a kernel.
type fakeSyncExecutor struct {
	uploads   int
	downloads int
	uploaded  [][]byte
	download  []byte
}

func (f *fakeSyncExecutor) ExecuteNotebook(context.Context, Runtime, []byte) (string, error) {
	return "CRAFTMAKE_TASK_RESULT_BEGIN\n{\"protocol_version\":2,\"run_id\":\"run-1\",\"task_id\":\"task-1\",\"attempt\":1,\"status\":\"succeeded\",\"steps\":[{\"index\":1,\"exit_code\":0,\"stdout_path\":\"/remote/step-1.stdout\",\"stderr_path\":\"/remote/step-1.stderr\"}]}\nCRAFTMAKE_TASK_RESULT_END\n", nil
}

func (f *fakeSyncExecutor) UploadWorkspace(_ context.Context, _ Runtime, archive []byte) error {
	f.uploads++
	f.uploaded = append(f.uploaded, archive)
	return nil
}

func (f *fakeSyncExecutor) DownloadWorkspace(context.Context, Runtime, string, []string) ([]byte, error) {
	f.downloads++
	return f.download, nil
}

func syncableManifest(taskID string) *protocol.TaskManifest {
	return &protocol.TaskManifest{RunID: "run-1", TaskID: taskID, Attempt: 1, RuntimeDirectory: "/tmp/runtime",
		Steps: []protocol.StepManifest{{Index: 1, Name: "s", StdoutPath: "/tmp/o", StderrPath: "/tmp/e"}}}
}

func TestRunSubmissionSyncsWorkspaceInOncePerRun(t *testing.T) {
	localRoot := t.TempDir()
	writeWorkspaceFile(t, filepath.Join(localRoot, "script.sh"), "echo hi\n")
	executor := &fakeSyncExecutor{}
	colabBackend := &Backend{
		Control:  &fakeControlPlane{},
		Executor: executor,
		Config:   Config{RemoteRoot: "/content/craftmake", LocalRoot: localRoot, SyncIn: true},
	}
	if _, err := colabBackend.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{syncableManifest("t1"), syncableManifest("t2")}}); err != nil {
		t.Fatal(err)
	}
	if executor.uploads != 1 {
		t.Fatalf("workspace must be uploaded once per run, got %d uploads", executor.uploads)
	}
	names := archiveMemberNames(t, executor.uploaded[0])
	if !containsString(names, "content/craftmake/work/script.sh") {
		t.Fatalf("uploaded archive does not contain the project file: %#v", names)
	}
}

func TestRunSubmissionSyncsWorkspaceOut(t *testing.T) {
	localRoot := t.TempDir()
	archive := archiveWithMember(t, "outputs/result.txt", "trained\n")
	executor := &fakeSyncExecutor{download: archive}
	colabBackend := &Backend{
		Control:  &fakeControlPlane{},
		Executor: executor,
		Config:   Config{RemoteRoot: "/content/craftmake", LocalRoot: localRoot, SyncOut: true},
	}
	if _, err := colabBackend.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{syncableManifest("t1")}}); err != nil {
		t.Fatal(err)
	}
	if executor.downloads != 1 {
		t.Fatalf("expected one workspace download, got %d", executor.downloads)
	}
	data, err := os.ReadFile(filepath.Join(localRoot, ".craftmake", "colab-workspace", "outputs", "result.txt"))
	if err != nil {
		t.Fatalf("downloaded output was not written back: %v", err)
	}
	if string(data) != "trained\n" {
		t.Fatalf("unexpected downloaded content %q", data)
	}
}

// TestWorkspaceSyncIsIdempotentAcrossRuns guards against the mirror being
// re-uploaded on the next run, which would nest work/work/... indefinitely.
func TestWorkspaceSyncIsIdempotentAcrossRuns(t *testing.T) {
	localRoot := t.TempDir()
	writeWorkspaceFile(t, filepath.Join(localRoot, "input.txt"), "local\n")
	executor := &fakeSyncExecutor{download: archiveWithMember(t, "work/input.txt", "local\n")}
	config := Config{RemoteRoot: "/content/craftmake", LocalRoot: localRoot, SyncIn: true, SyncOut: true}
	for run := 0; run < 2; run++ {
		colabBackend := &Backend{Control: &fakeControlPlane{}, Executor: executor, Config: config}
		if _, err := colabBackend.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{syncableManifest("t1")}}); err != nil {
			t.Fatal(err)
		}
	}
	if executor.uploads != 2 {
		t.Fatalf("expected one upload per run, got %d", executor.uploads)
	}
	names := archiveMemberNames(t, executor.uploaded[1])
	for _, name := range names {
		if strings.Contains(name, "colab-workspace") {
			t.Fatalf("the local mirror must not be uploaded back: %#v", names)
		}
	}
	// The mirrored copy from the first run must still be present exactly once.
	if _, err := os.Stat(filepath.Join(localRoot, ".craftmake", "colab-workspace", "work", "input.txt")); err != nil {
		t.Fatalf("mirror missing after the second run: %v", err)
	}
}

// TestRunSubmissionSyncInRequiresTransportSupport makes sure an executor
// without workspace support fails loudly instead of silently skipping the sync.
func TestRunSubmissionSyncInRequiresTransportSupport(t *testing.T) {
	colabBackend := &Backend{
		Control:  &fakeControlPlane{},
		Executor: fakeNotebookExecutor{},
		Config:   Config{RemoteRoot: "/content/craftmake", LocalRoot: t.TempDir(), SyncIn: true},
	}
	result, err := colabBackend.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{syncableManifest("t1")}})
	if err != nil {
		t.Fatal(err)
	}
	outcome := result.Tasks["t1"]
	if outcome.Err == nil {
		t.Fatal("expected a sync_in failure for an executor without transport support")
	}
}

func archiveWithMember(t *testing.T, name, content string) []byte {
	t.Helper()
	return archiveWithMembers(t, map[string]string{name: content})
}

func archiveWithMembers(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		payload := []byte(members[name])
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(payload))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
