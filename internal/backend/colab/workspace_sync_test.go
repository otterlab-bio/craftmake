package colab

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

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

// flakyUploadTransport fails the first N uploads with a kernel-disconnected
// error, mirroring a freshly assigned runtime whose kernel is not answering yet.
type flakyUploadTransport struct {
	failures  int
	attempts  int
	lastError error
}

func (f *flakyUploadTransport) ExecuteNotebook(context.Context, Runtime, []byte) (string, error) {
	return "", nil
}

func (f *flakyUploadTransport) UploadWorkspace(context.Context, Runtime, []byte) error {
	f.attempts++
	if f.attempts <= f.failures {
		return f.lastError
	}
	return nil
}

func (f *flakyUploadTransport) DownloadWorkspace(context.Context, Runtime, string, []string) ([]byte, error) {
	return nil, nil
}

// TestUploadWorkspaceRetriesTransientKernelFailure pins the fix for the live
// failure where the workspace upload timed out because it was issued before the
// kernel answered; the upload is idempotent, so a bounded retry is safe.
func TestUploadWorkspaceRetriesTransientKernelFailure(t *testing.T) {
	original := workspaceRetryBackoff
	workspaceRetryBackoff = time.Millisecond
	defer func() { workspaceRetryBackoff = original }()

	transport := &flakyUploadTransport{failures: 2, lastError: &RemoteError{Kind: ErrorKernelDisconnected, Operation: "read kernel message", Err: errors.New("i/o timeout")}}
	if err := uploadWorkspaceWithRetry(context.Background(), transport, Runtime{ID: "runtime-1"}, []byte("archive")); err != nil {
		t.Fatalf("transient failures should be retried: %v", err)
	}
	if transport.attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", transport.attempts)
	}
}

// TestUploadWorkspaceDoesNotRetryPermanentFailures keeps a protocol or
// configuration error from being repeated three times.
func TestUploadWorkspaceDoesNotRetryPermanentFailures(t *testing.T) {
	original := workspaceRetryBackoff
	workspaceRetryBackoff = time.Millisecond
	defer func() { workspaceRetryBackoff = original }()

	transport := &flakyUploadTransport{failures: 3, lastError: &RemoteError{Kind: ErrorProtocolMismatch, Operation: "upload workspace", Err: errors.New("unknown member")}}
	if err := uploadWorkspaceWithRetry(context.Background(), transport, Runtime{ID: "runtime-1"}, []byte("archive")); err == nil {
		t.Fatal("expected the permanent failure to surface")
	}
	if transport.attempts != 1 {
		t.Fatalf("permanent failures must not be retried, got %d attempts", transport.attempts)
	}
}

// TestUploadWorkspaceGivesUpAfterAttempts bounds the retry.
func TestUploadWorkspaceGivesUpAfterAttempts(t *testing.T) {
	original := workspaceRetryBackoff
	workspaceRetryBackoff = time.Millisecond
	defer func() { workspaceRetryBackoff = original }()

	transport := &flakyUploadTransport{failures: 10, lastError: &RemoteError{Kind: ErrorKernelDisconnected, Operation: "read kernel message", Err: errors.New("i/o timeout")}}
	if err := uploadWorkspaceWithRetry(context.Background(), transport, Runtime{ID: "runtime-1"}, []byte("archive")); err == nil {
		t.Fatal("expected the upload to fail after exhausting attempts")
	}
	if transport.attempts != workspaceUploadAttempts {
		t.Fatalf("expected %d attempts, got %d", workspaceUploadAttempts, transport.attempts)
	}
}

// fakeDriveStore records what the REST transport uploads and serves what it
// should restore. It also mirrors the store's incremental behaviour: content it
// already holds is reported as skipped rather than uploaded again.
type fakeDriveStore struct {
	uploaded map[string]string
	restore  map[string]string
	// uploadedPaths records the local paths the backend streamed from, so a test
	// can prove the file was not read into memory by the caller.
	uploadedPaths []string
}

func newFakeDriveStore() *fakeDriveStore {
	return &fakeDriveStore{uploaded: map[string]string{}}
}

func (f *fakeDriveStore) Upload(_ context.Context, folderPath, name, localPath string) (DriveUploadResult, error) {
	key := name
	if folderPath != "" {
		key = folderPath + "/" + name
	}
	f.uploadedPaths = append(f.uploadedPaths, localPath)
	info, err := os.Stat(localPath)
	if err != nil {
		return DriveUploadResult{}, err
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return DriveUploadResult{}, err
	}
	// Mirror the store's incremental behaviour: identical content already in
	// Drive is reported as skipped rather than transferred again.
	if existing, ok := f.uploaded[key]; ok && existing == string(data) {
		return DriveUploadResult{ID: "skipped-" + key, Size: info.Size(), Skipped: true}, nil
	}
	f.uploaded[key] = string(data)
	return DriveUploadResult{ID: "id-" + key, Size: info.Size()}, nil
}

func (f *fakeDriveStore) Walk(_ context.Context, folderPath string, visit func(string, int64, io.Reader) error) error {
	prefix := strings.Trim(folderPath, "/")
	for relative, content := range f.restore {
		if prefix != "" {
			if !strings.HasPrefix(relative, prefix+"/") {
				continue
			}
			relative = strings.TrimPrefix(relative, prefix+"/")
		}
		if err := visit(relative, int64(len(content)), strings.NewReader(content)); err != nil {
			return err
		}
	}
	return nil
}

// TestSyncOutUploadsMirrorToDrive covers the REST transport: after the kernel
// download, the local mirror is persisted into Drive.
func TestSyncOutUploadsMirrorToDrive(t *testing.T) {
	localRoot := t.TempDir()
	executor := &fakeSyncExecutor{download: archiveWithMember(t, "outputs/result.txt", "trained\n")}
	store := newFakeDriveStore()
	colabBackend := &Backend{
		Control:  &fakeControlPlane{},
		Executor: executor,
		Config: Config{RemoteRoot: "/content/craftmake", LocalRoot: localRoot, SyncOut: true,
			DriveTransport: DriveTransportREST, DriveFolder: "craftmake", DriveStore: store},
	}
	if _, err := colabBackend.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{syncableManifest("t1")}}); err != nil {
		t.Fatal(err)
	}
	if got := store.uploaded["craftmake/outputs/result.txt"]; got != "trained\n" {
		t.Fatalf("Drive upload missing or wrong: %#v", store.uploaded)
	}
	// The kernel download still populates the local mirror.
	if _, err := os.Stat(filepath.Join(localRoot, ".craftmake", "colab-workspace", "outputs", "result.txt")); err != nil {
		t.Fatalf("local mirror missing: %v", err)
	}
}

// TestSyncInRestoresDriveBeforeLocalProject covers the other direction: the
// durable Drive workspace is restored into the runtime before the local project.
func TestSyncInRestoresDriveBeforeLocalProject(t *testing.T) {
	localRoot := t.TempDir()
	writeWorkspaceFile(t, filepath.Join(localRoot, "script.sh"), "echo hi\n")
	executor := &fakeSyncExecutor{}
	store := newFakeDriveStore()
	store.restore = map[string]string{"craftmake/outputs/previous.txt": "from-drive\n"}
	colabBackend := &Backend{
		Control:  &fakeControlPlane{},
		Executor: executor,
		Config: Config{RemoteRoot: "/content/craftmake", LocalRoot: localRoot, SyncIn: true,
			DriveTransport: DriveTransportREST, DriveFolder: "craftmake", DriveStore: store},
	}
	if _, err := colabBackend.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{syncableManifest("t1")}}); err != nil {
		t.Fatal(err)
	}
	if executor.uploads != 2 {
		t.Fatalf("expected a Drive restore plus the project upload, got %d uploads", executor.uploads)
	}
	names := archiveMemberNames(t, executor.uploaded[0])
	if !containsString(names, "content/craftmake/outputs/previous.txt") {
		t.Fatalf("restored archive is missing the Drive file: %#v", names)
	}
	projectNames := archiveMemberNames(t, executor.uploaded[1])
	if !containsString(projectNames, "content/craftmake/work/script.sh") {
		t.Fatalf("project archive is missing the local file: %#v", projectNames)
	}
	mirror, err := os.ReadFile(filepath.Join(localRoot, ".craftmake", "colab-workspace", "outputs", "previous.txt"))
	if err != nil || string(mirror) != "from-drive\n" {
		t.Fatalf("mirror not restored from Drive: %q %v", mirror, err)
	}
}

// TestDriveFolderDerivesFromDriveRoot keeps the Drive folder predictable.
func TestDriveFolderDerivesFromDriveRoot(t *testing.T) {
	cases := map[string]string{
		"/content/drive/MyDrive/craftmake":  "craftmake",
		"/content/drive/My Drive/project/x": "project/x",
		"/content/craftmake":                "craftmake",
	}
	for root, want := range cases {
		colabBackend := &Backend{Config: Config{RemoteRoot: root}}
		if got := colabBackend.driveFolder(); got != want {
			t.Fatalf("driveFolder(%q) = %q, want %q", root, got, want)
		}
	}
	explicit := &Backend{Config: Config{RemoteRoot: "/content/craftmake", DriveFolder: "custom/dir"}}
	if got := explicit.driveFolder(); got != "custom/dir" {
		t.Fatalf("explicit drive folder ignored: %q", got)
	}
}

// TestPersistDriveWorkspaceReportsSkippedFiles pins the incremental behaviour at
// the backend level: an unchanged workspace is reported as skipped, and the store
// is handed paths so the content is streamed rather than read by the caller.
func TestPersistDriveWorkspaceReportsSkippedFiles(t *testing.T) {
	localRoot := t.TempDir()
	mirror := filepath.Join(localRoot, ".craftmake", "colab-workspace", "outputs")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, filepath.Join(mirror, "a.txt"), "aaa\n")
	writeWorkspaceFile(t, filepath.Join(mirror, "b.txt"), "bbb\n")

	store := newFakeDriveStore()
	colabBackend := &Backend{Config: Config{LocalRoot: localRoot, DriveFolder: "craftmake",
		DriveTransport: DriveTransportREST, DriveStore: store}}

	first, err := colabBackend.persistDriveWorkspace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Uploaded != 2 || first.Skipped != 0 {
		t.Fatalf("first report = %#v, want both files uploaded", first)
	}
	for _, uploaded := range store.uploadedPaths {
		if _, err := os.Stat(uploaded); err != nil {
			t.Fatalf("the store must be given a readable path to stream from: %v", err)
		}
	}

	// The same workspace again: nothing changed, so nothing is transferred.
	second, err := colabBackend.persistDriveWorkspace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Skipped != 2 || second.Uploaded != 0 {
		t.Fatalf("second report = %#v, want both files skipped", second)
	}
	if got := store.uploaded["craftmake/outputs/a.txt"]; got != "aaa\n" {
		t.Fatalf("Drive content = %q", got)
	}

	// A changed file is uploaded again.
	writeWorkspaceFile(t, filepath.Join(mirror, "a.txt"), "changed\n")
	third, err := colabBackend.persistDriveWorkspace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.Uploaded != 1 || third.Skipped != 1 {
		t.Fatalf("third report = %#v, want one changed file uploaded", third)
	}
}
