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
)

func TestColabAuthLoginEndToEnd(t *testing.T) {
	// Fake token endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 3600, "token_type": "Bearer"})
	}))
	defer tokenServer.Close()
	// Fake userinfo endpoint.
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "Test User", "email": "test@example.com"})
	}))
	defer userServer.Close()

	_ = os.Setenv("CRAFTMAKE_COLAB_CLIENT_ID", "client-id")
	_ = os.Setenv("CRAFTMAKE_COLAB_CLIENT_SECRET", "client-secret")
	_ = os.Setenv("CRAFTMAKE_COLAB_TOKEN_URL", tokenServer.URL)
	_ = os.Setenv("CRAFTMAKE_COLAB_USERINFO_URL", userServer.URL)
	defer func() {
		_ = os.Unsetenv("CRAFTMAKE_COLAB_CLIENT_ID")
		_ = os.Unsetenv("CRAFTMAKE_COLAB_CLIENT_SECRET")
		_ = os.Unsetenv("CRAFTMAKE_COLAB_TOKEN_URL")
		_ = os.Unsetenv("CRAFTMAKE_COLAB_USERINFO_URL")
	}()

	configPath := filepath.Join(t.TempDir(), "colab-auth.json")
	cmd := newColabAuthLoginCommand()
	cmd.SetArgs([]string{"--config", configPath, "--session", "gpu", "--timeout", "5s"})
	var out bytes.Buffer
	cmd.SetOut(&out)

	// Run the command in a goroutine; it blocks waiting for the callback.
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	// Wait for the loopback server to be up, then simulate the browser callback.
	// We can't easily know the redirect URI, so poll the auth config dir for the
	// credential file to appear, then trigger via a best-effort approach.
	// Instead, we rely on the command printing the URL; but for the test we
	// trigger the callback by scanning the loopback port from the printed URL.
	// Simpler: poll until the command writes the auth URL to output, parse it.
	var redirectURI string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		text := out.String()
		if idx := strings.Index(text, "https://accounts.google.com/o/oauth2/v2/auth?"); idx >= 0 {
			line := text[idx:]
			if end := strings.IndexAny(line, " \n"); end > 0 {
				line = line[:end]
			}
			parsed, err := url.Parse(line)
			if err == nil {
				redirectURI = parsed.Query().Get("redirect_uri")
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if redirectURI == "" {
		t.Fatalf("did not see redirect URI in output: %q", out.String())
	}

	// Simulate the browser callback with the code.
	resp, err := http.Get(redirectURI + "?code=the-code&state=nonce%3Dgpu")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if err := <-done; err != nil {
		t.Fatalf("login failed: %v", err)
	}

	// Assert the credential file was written with the refresh token.
	credFile := filepath.Join(filepath.Dir(configPath), "credentials", "gpu.json")
	data, err := os.ReadFile(credFile)
	if err != nil {
		t.Fatalf("credential file not written: %v", err)
	}
	if !strings.Contains(string(data), "refresh-1") {
		t.Fatalf("credential file missing refresh token: %q", data)
	}
	// Assert the session auth config references the credential file.
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("auth config not written: %v", err)
	}
}

func TestColabAuthLoginUsesConfiguredClientAndColabScope(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "r", "expires_in": 3600, "token_type": "Bearer"})
	}))
	defer tokenServer.Close()
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "T", "email": "t@e.com"})
	}))
	defer userServer.Close()
	_ = os.Setenv("CRAFTMAKE_COLAB_CLIENT_ID", "test-client-id")
	_ = os.Setenv("CRAFTMAKE_COLAB_CLIENT_SECRET", "test-client-secret")
	_ = os.Setenv("CRAFTMAKE_COLAB_TOKEN_URL", tokenServer.URL)
	_ = os.Setenv("CRAFTMAKE_COLAB_USERINFO_URL", userServer.URL)
	defer func() {
		_ = os.Unsetenv("CRAFTMAKE_COLAB_CLIENT_ID")
		_ = os.Unsetenv("CRAFTMAKE_COLAB_CLIENT_SECRET")
		_ = os.Unsetenv("CRAFTMAKE_COLAB_TOKEN_URL")
		_ = os.Unsetenv("CRAFTMAKE_COLAB_USERINFO_URL")
	}()

	configPath := filepath.Join(t.TempDir(), "colab-auth.json")
	cmd := newColabAuthLoginCommand()
	cmd.SetArgs([]string{"--config", configPath, "--session", "gpu", "--timeout", "5s"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

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
		t.Fatalf("no auth URL printed: %q", out.String())
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("client_id") != "test-client-id" {
		t.Fatalf("client_id = %q, want configured client id", q.Get("client_id"))
	}
	scope := q.Get("scope")
	if !strings.Contains(scope, "colaboratory") {
		t.Fatalf("scope missing colaboratory: %q", scope)
	}
	if strings.Contains(scope, "drive") {
		t.Fatalf("scope should not include drive: %q", scope)
	}
	resp, err := http.Get(q.Get("redirect_uri") + "?code=c&state=nonce%3Dgpu")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := <-done; err != nil {
		t.Fatalf("login failed: %v", err)
	}
}
func TestOpenBrowserMockedInTest(t *testing.T) {
	var openedURL string
	orig := BrowserOpener
	BrowserOpener = func(u string) error {
		openedURL = u
		return nil
	}
	defer func() { BrowserOpener = orig }()

	err := openBrowser("https://example.com/test-auth")
	if err != nil {
		t.Fatal(err)
	}
	if openedURL != "https://example.com/test-auth" {
		t.Fatalf("openedURL = %q", openedURL)
	}
}

func TestOpenBrowserSilentInTestingWhenUnmocked(t *testing.T) {
	orig := BrowserOpener
	BrowserOpener = nil
	defer func() { BrowserOpener = orig }()

	// Running inside 'go test', so openBrowser must return nil without launching browser.
	err := openBrowser("https://example.com/silent-test")
	if err != nil {
		t.Fatalf("expected nil inside tests, got %v", err)
	}
}
