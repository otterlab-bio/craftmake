package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	colabpkg "github.com/otterlab-bio/craftmake/internal/backend/colab"
)

func TestBuildColabBackendLoadsSessionConfig(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := colabpkg.UpsertSessionAuth(authPath, colabpkg.SessionAuth{SessionID: "gpu", DriveRoot: "/content/drive/MyDrive/project", MountPath: "/content/drive", ColabCredentialFile: "/tmp/colab.json", DriveCredentialFile: "/tmp/drive.json"}); err != nil {
		t.Fatal(err)
	}
	b, err := buildColabBackend(context.Background(), colabBackendConfig{SessionID: "gpu", AuthConfig: authPath, ProjectDirectory: "/local/project"})
	if err != nil {
		t.Fatal(err)
	}
	colabBackend, ok := b.(*colabpkg.Backend)
	if !ok {
		t.Fatalf("expected *colab.Backend, got %T", b)
	}
	if colabBackend.Config.SessionID != "gpu" || colabBackend.Config.DriveRoot != "/content/drive/MyDrive/project" {
		t.Fatalf("session config not loaded: %#v", colabBackend.Config)
	}
}

func TestBuildColabBackendRejectsMissingSession(t *testing.T) {
	if _, err := buildColabBackend(context.Background(), colabBackendConfig{AuthConfig: "/tmp/none"}); err == nil {
		t.Fatal("expected error when session is missing")
	}
}

func TestResolveColabRefreshTokenFromCredentialFile(t *testing.T) {
	credFile := filepath.Join(t.TempDir(), "gpu.json")
	if err := os.WriteFile(credFile, []byte("refresh-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := colabpkg.SessionAuth{ColabCredentialFile: credFile}
	token, ok := resolveColabRefreshToken(auth)
	if !ok || token != "refresh-from-file" {
		t.Fatalf("token = %q ok=%v", token, ok)
	}
}

func TestResolveColabRefreshTokenFallsBackToEnv(t *testing.T) {
	_ = os.Setenv("CRAFTMAKE_TEST_REFRESH", "refresh-from-env")
	defer os.Unsetenv("CRAFTMAKE_TEST_REFRESH")
	auth := colabpkg.SessionAuth{ColabRefreshTokenEnv: "CRAFTMAKE_TEST_REFRESH"}
	token, ok := resolveColabRefreshToken(auth)
	if !ok || token != "refresh-from-env" {
		t.Fatalf("token = %q ok=%v", token, ok)
	}
}

func TestResolveColabRefreshTokenMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, ok := resolveColabRefreshToken(colabpkg.SessionAuth{}); ok {
		t.Fatal("expected no token")
	}
}
func TestResolveColabRefreshTokenPrioritizesLocalDefaultFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".config", "craftmake", "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "my-session.json"), []byte("local-token-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := colabpkg.SessionAuth{SessionID: "my-session"}
	token, ok := resolveColabRefreshToken(auth)
	if !ok || token != "local-token-123" {
		t.Fatalf("token = %q ok=%v, want local-token-123", token, ok)
	}
}

// TestTokenManagerRefreshCarriesClientID verifies the TokenManager refresh
// request includes the configured client_id (fixing invalid_request).
func TestTokenManagerRefreshCarriesClientID(t *testing.T) {
	var gotClientID string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotClientID = r.PostForm.Get("client_id")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "expires_in": 3600, "token_type": "Bearer"})
	}))
	defer tokenServer.Close()
	manager := &colabpkg.TokenManager{Config: colabpkg.TokenConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret", TokenURL: tokenServer.URL}}
	manager.SetRefreshToken("refresh-1")
	token, err := manager.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-1" {
		t.Fatalf("token = %q", token)
	}
	if gotClientID != "test-client-id" {
		t.Fatalf("client_id = %q, want configured client id", gotClientID)
	}
	if gotClientID == "" {
		t.Fatalf("client_id should be configured: %q", gotClientID)
	}
}
