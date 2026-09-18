package colab

import (
	"context"
	"path/filepath"
	"testing"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
)

func TestNewFactoryLoadsSessionConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := UpsertSessionAuth(path, SessionAuth{SessionID: "gpu", DriveRoot: "/drive/project", MountPath: "/content/drive", ColabCredentialFile: "/tmp/colab.json", DriveCredentialFile: "/tmp/drive.json"}); err != nil {
		t.Fatal(err)
	}
	factory := NewFactory(FactoryDependencies{Control: &fakeControlPlane{}, Executor: fakeNotebookExecutor{}, MountPreflight: &fakeMountPreflight{}, Workspace: &fakeWorkspaceSyncer{}})
	b, err := factory(context.Background(), backendpkg.FactoryConfig{AuthConfigPath: path, SessionID: "gpu", ProjectDirectory: "/local/project"})
	if err != nil {
		t.Fatal(err)
	}
	colabBackend, ok := b.(*Backend)
	if !ok {
		t.Fatalf("unexpected backend type: %T", b)
	}
	if colabBackend.Config.SessionID != "gpu" || colabBackend.Config.DriveRoot != "/drive/project" {
		t.Fatalf("unexpected config: %#v", colabBackend.Config)
	}
}
