package colab

import (
	"context"
	"errors"
	"strings"
	"testing"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
)

// fakeMountPreflightResult lets a test inject a pass/fail into CheckMount.
type fakeMountPreflightResult struct{ err error }

func (f fakeMountPreflightResult) CheckMount(context.Context, DriveMountRequest) error { return f.err }

func TestBeginRunMountNotAuthorizedReturnsHint(t *testing.T) {
	b := &Backend{
		Config:         Config{DriveRoot: "/content/drive/MyDrive/project", MountPath: "/content/drive", SessionID: "gpu"},
		Control:        &fakeControlPlane{},
		Executor:       fakeNotebookExecutor{},
		MountPreflight: fakeMountPreflightResult{err: errors.New("mount denied")},
	}
	err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1", ProjectDirectory: "/local/project"})
	if err == nil {
		t.Fatal("expected mount error")
	}
	var mountErr *MountNotAuthorizedError
	if !errors.As(err, &mountErr) {
		t.Fatalf("expected MountNotAuthorizedError, got %T", err)
	}
	if mountErr.SessionID != "gpu" {
		t.Fatalf("unexpected session: %q", mountErr.SessionID)
	}
	if !strings.Contains(err.Error(), "craftmake colab drive authorize --session gpu") {
		t.Fatalf("error missing authorization hint: %v", err)
	}
}

func TestBeginRunMountAuthorizedProceeds(t *testing.T) {
	control := &fakeControlPlane{}
	b := &Backend{
		Config:         Config{DriveRoot: "/content/drive/MyDrive/project", MountPath: "/content/drive", SessionID: "gpu"},
		Control:        control,
		Executor:       fakeNotebookExecutor{},
		MountPreflight: fakeMountPreflightResult{err: nil},
	}
	if err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1", ProjectDirectory: "/local/project"}); err != nil {
		t.Fatal(err)
	}
	if control.acquired != 0 {
		t.Fatalf("BeginRun must not acquire a runtime, got %d", control.acquired)
	}
}
