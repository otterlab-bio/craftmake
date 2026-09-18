package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	colab "github.com/otterlab-bio/craftmake/internal/backend/colab"
)

func TestColabAuthCLIConfiguresAndMountsNamedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	configure := newColabAuthConfigureCommand()
	configure.SetArgs([]string{"--config", path, "--session", "gpu", "--drive-root", "/content/drive/MyDrive/craftmake", "--colab-credential-file", "/tmp/colab.json", "--drive-credential-file", "/tmp/drive.json"})
	var output bytes.Buffer
	configure.SetOut(&output)
	if err := configure.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	mount := newColabDriveMountCommand()
	mount.SetArgs([]string{"--config", path, "--session", "gpu"})
	mount.SetOut(&output)
	if err := mount.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "\"status\": \"ready-for-backend-mount\"") {
		t.Fatalf("unexpected output: %s", output.String())
	}
	doctor := newColabDoctorCommand()
	doctor.SetArgs([]string{"--config", path, "--session", "gpu"})
	doctor.SetOut(&output)
	if err := doctor.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "\"ready\": true") {
		t.Fatalf("unexpected doctor output: %s", output.String())
	}
}

func TestColabDriveMountAuthorizeWithAlreadyAuthorizedSession(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fake-acc", "expires_in": 3600})
			return
		case strings.Contains(r.URL.Path, "/assign"):
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "xsrf"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"endpoint":         "probe-ep",
				"runtimeProxyInfo": map[string]any{"token": "ptok", "url": "https://proxy"},
			})
		case strings.Contains(r.URL.Path, "/credentials-propagation/"):
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "cp-xsrf"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		case strings.Contains(r.URL.Path, "/unassign/"):
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "un-xsrf"})
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	t.Setenv("CRAFTMAKE_COLAB_DOMAIN", ts.URL)
	t.Setenv("CRAFTMAKE_COLAB_GAPI_DOMAIN", ts.URL)
	t.Setenv("CRAFTMAKE_COLAB_CLIENT_ID", "test-client-id")
	t.Setenv("CRAFTMAKE_COLAB_CLIENT_SECRET", "test-client-secret")
	t.Setenv("CRAFTMAKE_COLAB_TOKEN_URL", ts.URL+"/token")
	t.Setenv("CRAFTMAKE_TEST_REFRESH", "ref-tok")

	path := filepath.Join(t.TempDir(), "auth.json")
	auth := colab.SessionAuth{
		SessionID:            "gpu",
		DriveRoot:            "/content/drive/MyDrive/craftmake",
		MountPath:            "/content/drive",
		ColabRefreshTokenEnv: "CRAFTMAKE_TEST_REFRESH",
		DriveRefreshTokenEnv: "CRAFTMAKE_TEST_REFRESH",
	}
	if err := colab.UpsertSessionAuth(path, auth); err != nil {
		t.Fatal(err)
	}

	mount := newColabDriveMountCommand()
	mount.SetArgs([]string{"--config", path, "--session", "gpu", "--authorize", "--timeout", "1s"})
	var output bytes.Buffer
	mount.SetOut(&output)
	if err := mount.Execute(); err != nil {
		t.Fatalf("mount execute failed: %v\nOutput: %s", err, output.String())
	}
	if !strings.Contains(output.String(), "Google Drive is already authorized for session") {
		t.Fatalf("unexpected output: %s", output.String())
	}
}
