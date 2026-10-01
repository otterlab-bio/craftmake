package colab

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func rcloneTestSpec() DriveMountSpec {
	return DriveMountSpec{
		Kind: DriveMountKindRclone,
		Credential: DriveMountCredential{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RefreshToken: "refresh-token",
			Email:        "user@example.com",
		},
		MountPath:   "/content/drive",
		TimeoutSecs: 15,
		Required:    true,
		Rclone: RcloneMountOptions{
			ArchiveURL:         "https://example.invalid/rclone.zip",
			InstallTimeoutSecs: 60,
			AutoInstall:        true,
			ExtraArgs:          []string{"--drive-shared-with-me"},
		},
	}
}

// TestNormalizeDriveMountModeRclone pins the spellings that select the mount
// which works with a third-party credential.
func TestNormalizeDriveMountModeRclone(t *testing.T) {
	for _, input := range []string{"rclone", "RCLONE", " rclone ", "rclone-mount", "fuse"} {
		if got := NormalizeDriveMountMode(input); got != DriveMountRclone {
			t.Fatalf("NormalizeDriveMountMode(%q) = %q, want %q", input, got, DriveMountRclone)
		}
	}
}

// TestBuildRcloneMountSourceRendersBootstrap checks the generated code carries
// what a real mount needs, without leaking the credential into messages.
func TestBuildRcloneMountSourceRendersBootstrap(t *testing.T) {
	source := BuildRcloneMountSource(rcloneTestSpec(), "/content/craftmake/runtime/task-1")
	for _, want := range []string{
		`_MOUNT_ROOT = "/content/drive"`,
		`_MY_DRIVE_DIR = _MOUNT_ROOT + "/MyDrive"`, // rclone mounts the My Drive root
		`_ALIAS_DIR = _MOUNT_ROOT + "/My Drive"`,   // Colab's other spelling
		`"mount"`,                                  // the rclone mount call
		"/root/.config/rclone",
		"oauth2.googleapis.com/token",
		"fusermount3",
		"apt-get",
		"--vfs-cache-mode",
		"0o600", // the token file is created unreadable
		"def _redact",
		"_REQUIRED = True",
		"_AUTO_INSTALL = True",
		"https://example.invalid/rclone.zip",
		"--drive-shared-with-me",
		"/content/craftmake/runtime/task-1/drive-mount.missing",
		"CRAFTMAKE_DRIVE_MOUNT_OK",
		"CRAFTMAKE_DRIVE_MOUNT_UNAVAILABLE",
		"client-secret", "refresh-token",
		// The RC endpoint is what lets the finalizer drain pending uploads.
		`"--rc"`,
		`_RC_ADDR = "127.0.0.1:5572"`,
		"--vfs-write-back",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("rclone mount source missing %q", want)
		}
	}
	// The FUSE helper is exposed by symlinking the helper the image ships, which
	// is instant; the package manager must only be a fallback.
	if symlink, apt := strings.Index(source, "fusermount3"), strings.Index(source, "apt-get"); symlink > apt {
		t.Fatal("the fusermount3 symlink must be tried before the package manager")
	}
	if !strings.Contains(source, "_TIMEOUT_S = 15") {
		t.Fatalf("mount timeout was not applied:\n%s", source)
	}
	if !strings.Contains(source, "_INSTALL_TIMEOUT_S = 60") {
		t.Fatalf("install timeout was not applied:\n%s", source)
	}
	// A stray printf verb means Sprintf mangled the template.
	if strings.Contains(source, "%!") {
		t.Fatalf("template placeholders were mangled:\n%s", source)
	}
}

// TestRcloneMountSourceHasNoUnescapedVerbs guards the template against literal
// percent signs, which Sprintf would consume.
func TestRcloneMountSourceHasNoUnescapedVerbs(t *testing.T) {
	source := BuildRcloneMountSource(rcloneTestSpec(), "/content/craftmake/runtime/task-1")
	if strings.Contains(source, "%Y") || strings.Contains(source, "%s") {
		t.Fatalf("template kept a printf verb:\n%s", source)
	}
}

// TestNotebookOptionsSelectsRcloneAndMarksItRequired pins that an explicit
// rclone request is a required mount, while the default stays off.
func TestNotebookOptionsSelectsRcloneAndMarksItRequired(t *testing.T) {
	credential := &DriveMountCredential{ClientID: "id", ClientSecret: "secret", RefreshToken: "tok"}
	rclone := (&Backend{Config: Config{DriveMount: DriveMountRclone, DriveCredential: credential, DriveMountPath: "/content/drive"}}).notebookOptions()
	if rclone.DriveMount == nil {
		t.Fatal("rclone mode must produce a mount spec")
	}
	if rclone.DriveMount.Kind != DriveMountKindRclone {
		t.Fatalf("kind = %q, want %q", rclone.DriveMount.Kind, DriveMountKindRclone)
	}
	if !rclone.DriveMount.Required {
		t.Fatal("an explicit rclone mount must be required, so a failure fails the task")
	}
	// DriveFS stays non-fatal, as documented.
	drivefs := (&Backend{Config: Config{DriveMount: DriveMountDriveFS, DriveCredential: credential, DriveMountPath: "/content/drive"}}).notebookOptions()
	if drivefs.DriveMount == nil || drivefs.DriveMount.Required {
		t.Fatalf("DriveFS must stay non-fatal: %#v", drivefs.DriveMount)
	}
	// The default is still no mount at all.
	off := (&Backend{Config: Config{DriveCredential: credential}}).notebookOptions()
	if off.DriveMount != nil {
		t.Fatalf("mounting must stay off by default: %#v", off.DriveMount)
	}
}

// TestDriveMountSourceDispatchesByKind checks the notebook bootstrap follows the
// requested kind.
func TestDriveMountSourceDispatchesByKind(t *testing.T) {
	mapping := RemoteTaskMapping{
		WorkDirectory:    "/content/drive/MyDrive/proj/work",
		TempDirectory:    "/content/tmp",
		RuntimeDirectory: "/content/drive/MyDrive/proj/runtime",
		ResultPath:       "/content/drive/MyDrive/proj/runtime/result.json",
	}
	rclone := driveMountSource(mapping, NotebookOptions{DriveMount: &DriveMountSpec{Kind: DriveMountKindRclone, Credential: DriveMountCredential{RefreshToken: "tok"}, MountPath: "/content/drive"}})
	if !strings.Contains(rclone, "rclone") || strings.Contains(rclone, "/opt/google/drive/drive") {
		t.Fatalf("expected the rclone bootstrap, got %q", rclone)
	}
	drivefs := driveMountSource(mapping, NotebookOptions{DriveMount: &DriveMountSpec{Credential: DriveMountCredential{RefreshToken: "tok"}, MountPath: "/content/drive"}})
	if !strings.Contains(drivefs, "/opt/google/drive/drive") {
		t.Fatalf("expected the DriveFS bootstrap, got %q", drivefs)
	}
}

// TestFinalizerFailsTaskWhenRequiredMountIsMissing is the behavioural guard for
// the silent-data-loss failure mode: a run whose workspace was supposed to be on
// Drive must not report success when the mount never happened.
func TestFinalizerFailsTaskWhenRequiredMountIsMissing(t *testing.T) {
	python := pythonOrSkip(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "step-1.exit"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rcloneMountMissingSentinel), []byte("rclone did not mount within 15s"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := runFinalizer(t, python, dir)
	if payload["status"] != "failed" {
		t.Fatalf("status = %v, want failed", payload["status"])
	}
	if payload["exit_code"] != float64(2) {
		t.Fatalf("exit_code = %v, want 2", payload["exit_code"])
	}

	// Without the sentinel the same run succeeds.
	clean := t.TempDir()
	if err := os.WriteFile(filepath.Join(clean, "step-1.exit"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload = runFinalizer(t, python, clean)
	if payload["status"] != "succeeded" || payload["exit_code"] != float64(0) {
		t.Fatalf("status = %v exit_code = %v, want succeeded/0", payload["status"], payload["exit_code"])
	}
}

// TestGeneratedCellsAreValidPython compiles every generated Python cell, so a
// template mistake fails a test instead of a live run.
func TestGeneratedCellsAreValidPython(t *testing.T) {
	python := pythonOrSkip(t)
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1,
		Steps: []protocol.StepManifest{{Index: 1, Name: "s", Command: "echo hi"}}}
	mapping := RemoteTaskMapping{WorkDirectory: "/content/drive/MyDrive/proj/work", TempDirectory: "/content/tmp",
		RuntimeDirectory: "/content/drive/MyDrive/proj/runtime", ResultPath: "/content/drive/MyDrive/proj/runtime/result.json"}
	for _, kind := range []DriveMountKind{DriveMountKindDriveFS, DriveMountKindRclone} {
		spec := rcloneTestSpec()
		spec.Kind = kind
		notebook, err := BuildNotebookWithOptions(manifest, mapping, NotebookOptions{DriveMount: &spec})
		if err != nil {
			t.Fatal(err)
		}
		for index, cell := range notebook.Cells {
			source := strings.TrimSpace(cell.Source)
			// %%bash cells are shell, not Python.
			if strings.HasPrefix(source, "%%") {
				continue
			}
			path := filepath.Join(t.TempDir(), "cell.py")
			if err := os.WriteFile(path, []byte(cell.Source), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(python, "-c", "import sys; compile(open(sys.argv[1]).read(), sys.argv[1], 'exec')", path).CombinedOutput(); err != nil {
				t.Fatalf("%s cell %d does not compile: %v\n%s", kind, index, err, out)
			}
		}
	}
}

func pythonOrSkip(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	return python
}

// runFinalizer executes the generated finalizer against a prepared runtime
// directory and returns the task payload it reports.
func runFinalizer(t *testing.T, python, runtimeDir string) map[string]any {
	t.Helper()
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1,
		Steps: []protocol.StepManifest{{Index: 1, Name: "s", Command: "echo hi"}}}
	mapping := RemoteTaskMapping{WorkDirectory: filepath.Join(runtimeDir, "work"), TempDirectory: filepath.Join(runtimeDir, "tmp"),
		RuntimeDirectory: runtimeDir, ResultPath: filepath.Join(runtimeDir, "result.json")}
	notebook, err := BuildNotebookWithOptions(manifest, mapping, NotebookOptions{})
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "finalizer.py")
	if err := os.WriteFile(script, []byte(notebook.Cells[len(notebook.Cells)-1].Source), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(python, script).CombinedOutput()
	if err != nil {
		t.Fatalf("finalizer failed: %v\n%s", err, output)
	}
	start := strings.Index(string(output), "CRAFTMAKE_TASK_RESULT_BEGIN")
	end := strings.Index(string(output), "CRAFTMAKE_TASK_RESULT_END")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("finalizer did not report a payload:\n%s", output)
	}
	body := strings.TrimSpace(strings.TrimPrefix(string(output)[start:end], "CRAFTMAKE_TASK_RESULT_BEGIN"))
	// The payload is printed on the line after the begin sentinel.
	if newline := strings.Index(body, "\n"); newline >= 0 {
		body = body[newline+1:]
	}
	body = strings.TrimSpace(body)
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, body)
	}
	return payload
}

// TestFinalizerDrainsDriveUploadsBeforeUnmount pins the durability fix: rclone
// buffers writes and uploads them later, so releasing the mount without waiting
// loses them (observed live).
func TestFinalizerDrainsDriveUploadsBeforeUnmount(t *testing.T) {
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1,
		Steps: []protocol.StepManifest{{Index: 1, Name: "s", Command: "echo hi"}}}
	mapping := RemoteTaskMapping{WorkDirectory: "/content/drive/MyDrive/proj/work", TempDirectory: "/content/tmp",
		RuntimeDirectory: "/content/drive/MyDrive/proj/runtime", ResultPath: "/content/drive/MyDrive/proj/runtime/result.json"}
	notebook, err := BuildNotebookWithOptions(manifest, mapping, NotebookOptions{})
	if err != nil {
		t.Fatal(err)
	}
	finalizer := notebook.Cells[len(notebook.Cells)-1].Source
	for _, want := range []string{"_flush_rclone_mount", "_pending_uploads", "diskCache", "vfs/stats", "127.0.0.1:5572", driveFlushTimeoutMarker, "umount"} {
		if !strings.Contains(finalizer, want) {
			t.Fatalf("finalizer missing %q:\n%s", want, finalizer)
		}
	}
	// The result payload must be written before the mount is released, so the
	// flush also carries it to Drive.
	if strings.Index(finalizer, "result_path.write_text") > strings.Index(finalizer, "_flush_rclone_mount()") {
		t.Fatal("the result must be written before the mount is flushed")
	}
}

// TestDriveMountNoticeSurfacesFlushTimeout checks a flush that timed out is
// reported instead of being silently dropped.
func TestDriveMountNoticeSurfacesFlushTimeout(t *testing.T) {
	output := "[step-1 stdout]\nok\n" + driveFlushTimeoutMarker + " pending Drive uploads did not drain\nCRAFTMAKE_TASK_RESULT_BEGIN\n{}"
	notice := driveMountNotice(output)
	if !strings.Contains(notice, "did not drain") {
		t.Fatalf("flush timeout was not surfaced: %q", notice)
	}
	if got := driveMountNotice("nothing to report"); got != "" {
		t.Fatalf("unexpected notice: %q", got)
	}
}
