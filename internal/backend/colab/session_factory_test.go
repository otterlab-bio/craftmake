package colab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
)

func TestNewFactoryWiresReferenceInformedBackend(t *testing.T) {
	var assigned bool
	colab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, TunEndpoint+"/assign") {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"token":"xsrf"}`))
				return
			}
			assigned = true
			_ = json.NewEncoder(w).Encode(map[string]any{"endpoint": "https://proxy.example.test", "runtimeProxyInfo": map[string]any{"token": "pt", "url": "https://proxy.example.test"}})
			return
		}
		if strings.HasPrefix(r.URL.Path, TunEndpoint+"/unassign/") {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "ux"})
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer colab.Close()
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := UpsertSessionAuth(authPath, SessionAuth{SessionID: "gpu", DriveRoot: "/content/drive/MyDrive/p", MountPath: "/content/drive", ColabCredentialFile: "/tmp/c.json", DriveCredentialFile: "/tmp/d.json"}); err != nil {
		t.Fatal(err)
	}
	serverClient := NewColabServerClient(colab.URL, colab.URL, colab.Client())
	factory := NewFactory(FactoryDependencies{Server: serverClient, RuntimeSpec: RuntimeSpec{Variant: "GPU"}, MountPreflight: &fakeMountPreflight{}})
	backendInstance, err := factory(context.Background(), backendpkg.FactoryConfig{ProjectDirectory: "/local/project", AuthConfigPath: authPath, SessionID: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	colabBackend, ok := backendInstance.(*Backend)
	if !ok {
		t.Fatalf("unexpected type: %T", backendInstance)
	}
	if colabBackend.Config.DriveRoot != "/content/drive/MyDrive/p" {
		t.Fatalf("session not loaded: %#v", colabBackend.Config)
	}
	if err := colabBackend.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1", ProjectDirectory: "/local/project"}); err != nil {
		t.Fatal(err)
	}
	// In the ephemeral-instance model, BeginRun does not assign a runtime.
	if assigned {
		t.Fatal("BeginRun must not assign a runtime in the ephemeral-instance model")
	}
	if err := colabBackend.EndRun(context.Background(), backendpkg.RunOutcome{RunID: "run-1", Status: "succeeded", ProjectDirectory: "/local/project"}); err != nil {
		t.Fatal(err)
	}
}
