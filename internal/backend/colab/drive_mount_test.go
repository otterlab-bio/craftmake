package colab

import (
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestNormalizeDriveMountMode(t *testing.T) {
	cases := map[string]string{
		"":         DriveMountAuto,
		"auto":     DriveMountAuto,
		"whatever": DriveMountAuto,
		"drivefs":  DriveMountDriveFS,
		"on":       DriveMountDriveFS,
		"1":        DriveMountDriveFS,
		"off":      DriveMountOff,
		"skip":     DriveMountOff,
		"0":        DriveMountOff,
	}
	for input, want := range cases {
		if got := NormalizeDriveMountMode(input); got != want {
			t.Fatalf("NormalizeDriveMountMode(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestBuildDriveFSMountSourceRendersShim checks the generated bootstrap code
// carries the pieces a real mount needs, and none of the failure modes are
// fatal to the task.
func TestBuildDriveFSMountSourceRendersShim(t *testing.T) {
	source := BuildDriveFSMountSource(DriveMountSpec{
		Credential: DriveMountCredential{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RefreshToken: "refresh-token",
			Email:        "user@example.com",
		},
		MountPath:   "/content/drive",
		TimeoutSecs: 12,
	})
	for _, want := range []string{
		"/opt/google/drive/drive",           // the runtime binary
		"computeMetadata/v1",                // the metadata shim the binary talks to
		"oauth2.googleapis.com/token",       // in-runtime token refresh
		"CRAFTMAKE_DRIVE_MOUNT_OK",          // success marker
		"CRAFTMAKE_DRIVE_MOUNT_UNAVAILABLE", // graceful fallback marker
		"client-secret", "refresh-token",    // credential material for the shim
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("mount source missing %q", want)
		}
	}
	if !strings.Contains(source, "_TIMEOUT_S = 12") {
		t.Fatalf("mount timeout was not applied:\n%s", source)
	}
	// A missing binary must not raise: the caller falls back to the sync.
	if !strings.Contains(source, "_MOUNT_UNAVAILABLE") {
		t.Fatal("missing-binary path must be non-fatal")
	}
}

func TestDriveMountSourceRequiresCredentialAndDriveWorkspace(t *testing.T) {
	onDrive := RemoteTaskMapping{WorkDirectory: "/content/drive/MyDrive/proj/work", TempDirectory: "/content/tmp",
		RuntimeDirectory: "/content/drive/MyDrive/proj/runtime", ResultPath: "/content/drive/MyDrive/proj/runtime/result.json"}
	offDrive := RemoteTaskMapping{WorkDirectory: "/content/craftmake/work", TempDirectory: "/content/tmp",
		RuntimeDirectory: "/content/craftmake/runtime", ResultPath: "/content/craftmake/runtime/result.json"}

	if block := driveMountSource(offDrive, NotebookOptions{}); block != "" {
		t.Fatalf("a workspace outside Drive needs no mount block: %q", block)
	}
	// Without a Drive credential the historical best-effort block is used.
	legacy := driveMountSource(onDrive, NotebookOptions{})
	if !strings.Contains(legacy, "google.colab") {
		t.Fatalf("expected the legacy fallback block, got %q", legacy)
	}
	// With a credential the DriveFS mount replaces it.
	withFS := driveMountSource(onDrive, NotebookOptions{DriveMount: &DriveMountSpec{Credential: DriveMountCredential{RefreshToken: "tok"}, MountPath: "/content/drive"}})
	if !strings.Contains(withFS, "/opt/google/drive/drive") || strings.Contains(withFS, "google.colab") {
		t.Fatalf("expected the DriveFS block, got %q", withFS)
	}
}

// TestBuildNotebookWithDriveMountEmbedsShim checks the mount precedes the steps.
func TestBuildNotebookWithDriveMountEmbedsShim(t *testing.T) {
	manifest := &protocol.TaskManifest{ProtocolVersion: protocol.Version, RunID: "run-1", TaskID: "task-1", Attempt: 1,
		Steps: []protocol.StepManifest{{Index: 1, Name: "s", Command: "echo hi"}}}
	mapping := RemoteTaskMapping{WorkDirectory: "/content/drive/MyDrive/proj/work", TempDirectory: "/content/tmp",
		RuntimeDirectory: "/content/drive/MyDrive/proj/runtime", ResultPath: "/content/drive/MyDrive/proj/runtime/result.json"}
	notebook, err := BuildNotebookWithOptions(manifest, mapping, NotebookOptions{DriveMount: &DriveMountSpec{
		Credential: DriveMountCredential{ClientID: "id", ClientSecret: "secret", RefreshToken: "token"},
		MountPath:  "/content/drive",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(notebook.Cells) != 3 {
		t.Fatalf("expected bootstrap + step + finalizer, got %d", len(notebook.Cells))
	}
	if !strings.Contains(notebook.Cells[0].Source, "/opt/google/drive/drive") {
		t.Fatalf("bootstrap must mount Drive:\n%s", notebook.Cells[0].Source)
	}
	if !strings.Contains(notebook.Cells[2].Source, "CRAFTMAKE_TASK_RESULT_BEGIN") {
		t.Fatal("finalizer sentinel missing")
	}
}
