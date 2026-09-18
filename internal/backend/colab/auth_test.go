package colab

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
)

type fakeMountPreflight struct{ request DriveMountRequest }

func (f *fakeMountPreflight) CheckMount(_ context.Context, request DriveMountRequest) error {
	f.request = request
	return nil
}

type fakeWorkspaceSyncer struct{ requests []WorkspaceSyncRequest }

func (f *fakeWorkspaceSyncer) SyncIn(_ context.Context, request WorkspaceSyncRequest) error {
	f.requests = append(f.requests, request)
	return nil
}
func (f *fakeWorkspaceSyncer) SyncOut(_ context.Context, request WorkspaceSyncRequest) error {
	f.requests = append(f.requests, request)
	return nil
}

func TestBackendLoadsSessionAuthAndSyncsWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	auth := SessionAuth{SessionID: "gpu", DriveRoot: "/content/drive/MyDrive/project", MountPath: "/content/drive", ColabCredentialFile: "/tmp/colab.json", DriveCredentialFile: "/tmp/drive.json"}
	if err := UpsertSessionAuth(path, auth); err != nil {
		t.Fatal(err)
	}
	control := &fakeControlPlane{}
	mount := &fakeMountPreflight{}
	workspace := &fakeWorkspaceSyncer{}
	backend := &Backend{Config: Config{AuthConfigPath: path, SessionID: "gpu"}, Control: control, Executor: fakeNotebookExecutor{}, MountPreflight: mount, Workspace: workspace}
	if err := backend.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1", ProjectDirectory: "/local/project"}); err != nil {
		t.Fatal(err)
	}
	if mount.request.SessionID != "gpu" || mount.request.AuthConfigPath != path || mount.request.DriveRoot != auth.DriveRoot {
		t.Fatalf("unexpected mount request: %#v", mount.request)
	}
	if len(workspace.requests) != 1 || workspace.requests[0].Direction != "in" || workspace.requests[0].LocalRoot != "/local/project" || workspace.requests[0].RemoteRoot != auth.DriveRoot {
		t.Fatalf("unexpected sync-in request: %#v", workspace.requests)
	}
	if err := backend.EndRun(context.Background(), backendpkg.RunOutcome{RunID: "run-1", Status: "succeeded", ProjectDirectory: "/local/project"}); err != nil {
		t.Fatal(err)
	}
	if len(workspace.requests) != 2 || workspace.requests[1].Direction != "out" {
		t.Fatalf("expected sync-out request: %#v", workspace.requests)
	}
}

func TestSessionAuthConfigRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "colab-auth.json")
	auth := SessionAuth{SessionID: "gpu", DriveRoot: "/content/drive/MyDrive/craftmake", MountPath: "/content/drive", ColabCredentialFile: "/tmp/colab.json", DriveCredentialFile: "/tmp/drive.json"}
	if err := UpsertSessionAuth(path, auth); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionAuth(path, "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if loaded != auth {
		t.Fatalf("auth mismatch: %#v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %o", info.Mode().Perm())
	}
}
