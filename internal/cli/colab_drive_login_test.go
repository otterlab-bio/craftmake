package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	action "github.com/otterlab-bio/craftmake/internal/adapters/action"
	colabpkg "github.com/otterlab-bio/craftmake/internal/backend/colab"
)

func TestResolveDriveOAuthCredentialsPrecedence(t *testing.T) {
	t.Setenv("CRAFTMAKE_COLAB_DRIVE_CLIENT_ID", "")
	t.Setenv("CRAFTMAKE_COLAB_DRIVE_CLIENT_SECRET", "")

	clientID, secret, err := resolveDriveOAuthCredentials("", "")
	if err != nil {
		t.Fatalf("the bundled Drive client must resolve: %v", err)
	}
	if clientID != defaultDriveClientID || secret != defaultDriveClientSecret {
		t.Fatalf("expected the bundled Drive client, got %q", clientID)
	}

	t.Setenv("CRAFTMAKE_COLAB_DRIVE_CLIENT_ID", "env-drive-id")
	t.Setenv("CRAFTMAKE_COLAB_DRIVE_CLIENT_SECRET", "env-drive-secret")
	clientID, secret, err = resolveDriveOAuthCredentials("", "")
	if err != nil || clientID != "env-drive-id" || secret != "env-drive-secret" {
		t.Fatalf("environment must override the bundled Drive client: %q %q %v", clientID, secret, err)
	}

	clientID, secret, err = resolveDriveOAuthCredentials("flag-drive-id", "flag-drive-secret")
	if err != nil || clientID != "flag-drive-id" || secret != "flag-drive-secret" {
		t.Fatalf("flags must override the environment: %q %q %v", clientID, secret, err)
	}
}

func TestResolveDriveRefreshTokenSources(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRAFTMAKE_DRIVE_REFRESH_TOKEN", "")

	credFile := filepath.Join(t.TempDir(), "gpu-drive.json")
	if err := os.WriteFile(credFile, []byte("drive-token-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, ok := resolveDriveRefreshToken(colabpkg.SessionAuth{DriveCredentialFile: credFile}); !ok || token != "drive-token-from-file" {
		t.Fatalf("credential file token = %q ok=%v", token, ok)
	}

	t.Setenv("CRAFTMAKE_DRIVE_REFRESH_TOKEN", "drive-token-from-env")
	if token, ok := resolveDriveRefreshToken(colabpkg.SessionAuth{}); !ok || token != "drive-token-from-env" {
		t.Fatalf("environment token = %q ok=%v", token, ok)
	}

	t.Setenv("CRAFTMAKE_DRIVE_REFRESH_TOKEN", "")
	if _, ok := resolveDriveRefreshToken(colabpkg.SessionAuth{}); ok {
		t.Fatal("expected no Drive token")
	}
}

// TestColabDriveLoginRecordsDriveCredential runs the real command against a mock
// authorization server and checks the session keeps its Colab credential while
// gaining a Drive one.
func TestColabDriveLoginRecordsDriveCredential(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "drive-access", "refresh_token": "drive-refresh", "expires_in": 3600, "token_type": "Bearer"})
	}))
	defer tokenServer.Close()
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "Drive User", "email": "drive@example.com"})
	}))
	defer userServer.Close()
	t.Setenv("CRAFTMAKE_COLAB_TOKEN_URL", tokenServer.URL)
	t.Setenv("CRAFTMAKE_COLAB_USERINFO_URL", userServer.URL)

	configPath := filepath.Join(t.TempDir(), "colab-auth.json")
	colabCred := filepath.Join(filepath.Dir(configPath), "credentials", "gpu.json")
	if err := os.MkdirAll(filepath.Dir(colabCred), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(colabCred, []byte("colab-refresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := colabpkg.UpsertSessionAuth(configPath, colabpkg.SessionAuth{SessionID: "gpu", DriveRoot: "/content/drive/MyDrive/craftmake", MountPath: "/content/drive", ColabCredentialFile: colabCred, DriveCredentialFile: colabCred}); err != nil {
		t.Fatal(err)
	}

	command := newColabDriveLoginCommand()
	command.SetArgs([]string{"--config", configPath, "--session", "gpu", "--timeout", "5s"})
	var out bytes.Buffer
	command.SetOut(&out)
	done := make(chan error, 1)
	go func() { done <- command.Execute() }()

	var authURL string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		text := out.String()
		if idx := strings.Index(text, "https://accounts.google.com/o/oauth2/v2/auth?"); idx >= 0 {
			line := text[idx:]
			if end := strings.IndexAny(line, " \n"); end > 0 {
				line = line[:end]
			}
			authURL = line
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if authURL == "" {
		t.Fatalf("no Drive auth URL printed: %q", out.String())
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("client_id"); got != defaultDriveClientID {
		t.Fatalf("client_id = %q, want the bundled Drive client", got)
	}
	if scope := parsed.Query().Get("scope"); !strings.Contains(scope, "auth/drive") {
		t.Fatalf("scope %q must include Drive access", scope)
	}
	resp, err := http.Get(parsed.Query().Get("redirect_uri") + "?code=c&state=nonce%3Dgpu")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := <-done; err != nil {
		t.Fatalf("drive login failed: %v", err)
	}

	driveCred := filepath.Join(filepath.Dir(configPath), "credentials", "gpu-drive.json")
	data, err := os.ReadFile(driveCred)
	if err != nil {
		t.Fatalf("Drive credential file not written: %v", err)
	}
	if !strings.Contains(string(data), "drive-refresh") {
		t.Fatalf("unexpected Drive credential content: %q", data)
	}
	session, err := colabpkg.LoadSessionAuth(configPath, "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if session.DriveCredentialFile != driveCred || session.DriveOAuthClientID != defaultDriveClientID {
		t.Fatalf("Drive credential not recorded on the session: %#v", session)
	}
	if session.DriveAccountEmail != "drive@example.com" {
		t.Fatalf("Drive account email not recorded: %#v", session)
	}
	if session.ColabCredentialFile != colabCred {
		t.Fatalf("the Colab credential must survive a Drive login: %#v", session)
	}

	// logout forgets the Drive credential but keeps the session and its Colab one.
	logout := newColabDriveLogoutCommand()
	logout.SetArgs([]string{"--config", configPath, "--session", "gpu"})
	if err := logout.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(driveCred); !os.IsNotExist(err) {
		t.Fatalf("Drive credential file should be removed, got %v", err)
	}
	session, err = colabpkg.LoadSessionAuth(configPath, "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if session.DriveCredentialFile != "" || session.DriveOAuthClientID != "" {
		t.Fatalf("Drive credential fields should be cleared: %#v", session)
	}
	if session.ColabCredentialFile != colabCred {
		t.Fatalf("logout must not touch the Colab credential: %#v", session)
	}
}

// TestDriveAccessTokenManagerGuardsClientSwitch covers the same failure mode as
// the Colab token: a Drive refresh token cannot be redeemed by another client.
func TestDriveAccessTokenManagerGuardsClientSwitch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRAFTMAKE_COLAB_DRIVE_CLIENT_ID", "current-drive-client")
	t.Setenv("CRAFTMAKE_COLAB_DRIVE_CLIENT_SECRET", "current-drive-secret")
	t.Setenv("CRAFTMAKE_DRIVE_REFRESH_TOKEN", "")

	credFile := filepath.Join(t.TempDir(), "gpu-drive.json")
	if err := os.WriteFile(credFile, []byte("drive-refresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := driveAccessTokenManager(colabpkg.SessionAuth{SessionID: "gpu", DriveCredentialFile: credFile, DriveOAuthClientID: "original-drive-client"})
	if err == nil {
		t.Fatal("expected a Drive client-switch error")
	}
	for _, want := range []string{"original-drive-client", "current-drive-client", "craftmake colab drive login --session gpu"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}

	manager, token, err := driveAccessTokenManager(colabpkg.SessionAuth{SessionID: "gpu", DriveCredentialFile: credFile, DriveOAuthClientID: "current-drive-client"})
	if err != nil || manager == nil || token != "drive-refresh" {
		t.Fatalf("matching client should build a manager with its token: %v", err)
	}
	if manager, token, err := driveAccessTokenManager(colabpkg.SessionAuth{SessionID: "gpu"}); err != nil || manager != nil || token != "" {
		t.Fatalf("a session without Drive credentials should yield no manager: %v", err)
	}
}

// TestColabDriveTransportRequiresCredential pins the actionable failure when the
// REST transport is requested without a Drive credential.
func TestColabDriveTransportRequiresCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRAFTMAKE_DRIVE_REFRESH_TOKEN", "")
	store, err := newColabDriveStore(colabpkg.SessionAuth{SessionID: "gpu"})
	if err == nil {
		t.Fatal("expected an error without a Drive credential")
	}
	if store != nil {
		t.Fatal("no store should be built without a credential")
	}
	if !strings.Contains(err.Error(), "craftmake colab drive login --session gpu") {
		t.Fatalf("error must give the exact command, got: %v", err)
	}

	// With a credential the store is built and refreshes through the manager.
	t.Setenv("CRAFTMAKE_DRIVE_REFRESH_TOKEN", "drive-token")
	store, err = newColabDriveStore(colabpkg.SessionAuth{SessionID: "gpu"})
	if err != nil || store == nil {
		t.Fatalf("expected a store with a credential: %v", err)
	}
}

// TestDriveTransportReachesBackendConfig pins that an action's drive_transport
// reaches the constructed backend, which is what enables the REST upload.
func TestDriveTransportReachesBackendConfig(t *testing.T) {
	config := resolveColabBackendConfig(commonOptions{colab: &action.ColabSpec{
		Session:        "gpu",
		SyncIn:         true,
		SyncOut:        true,
		DriveTransport: "rest",
		DriveFolder:    "my-folder",
	}}, "", "", false, t.TempDir())
	if config.DriveTransport != "rest" || config.DriveFolder != "my-folder" {
		t.Fatalf("action drive transport not propagated: %#v", config)
	}
	if got := colabpkg.NormalizeDriveTransport(config.DriveTransport); got != colabpkg.DriveTransportREST {
		t.Fatalf("normalised transport = %q, want rest", got)
	}
}
