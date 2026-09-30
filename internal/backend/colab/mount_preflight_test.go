package colab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// preflightMockControlPlane emulates the Colab endpoints the Drive mount
// preflight uses and records what happened.
type preflightMockControlPlane struct {
	mu           sync.Mutex
	authorized   bool
	redirectURI  string
	assigns      int
	unassigns    int
	propagations []bool // dryRun flag per call
}

func (m *preflightMockControlPlane) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, TunEndpoint+"/assign"):
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "xsrf"})
				return
			}
			m.mu.Lock()
			m.assigns++
			m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"endpoint":         "probe-endpoint",
				"accelerator":      "T4",
				"variant":          "GPU",
				"machineShape":     0,
				"runtimeProxyInfo": map[string]any{"token": "pt", "url": ""},
			})
		case strings.HasPrefix(r.URL.Path, TunEndpoint+"/unassign/"):
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "ux"})
				return
			}
			m.mu.Lock()
			m.unassigns++
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, TunEndpoint+"/credentials-propagation/"):
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "cp"})
				return
			}
			m.mu.Lock()
			m.propagations = append(m.propagations, r.URL.Query().Get("dryrun") == "true")
			authorized, redirect := m.authorized, m.redirectURI
			m.mu.Unlock()
			if authorized {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "unauthorized_redirect_uri": redirect})
		default:
			http.NotFound(w, r)
		}
	}))
}

func (m *preflightMockControlPlane) counts() (int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.assigns, m.unassigns, len(m.propagations)
}

func TestServerMountPreflightAlreadyAuthorized(t *testing.T) {
	mock := &preflightMockControlPlane{authorized: true}
	server := mock.server(t)
	defer server.Close()

	preflight := &ServerMountPreflight{Client: NewColabServerClient(server.URL, server.URL, server.Client())}
	if err := preflight.CheckMount(context.Background(), DriveMountRequest{SessionID: "gpu", MountPath: "/content/drive"}); err != nil {
		t.Fatalf("expected authorization to be accepted, got %v", err)
	}
	assigns, unassigns, propagations := mock.counts()
	if assigns != 1 || unassigns != 1 || propagations != 1 {
		t.Fatalf("expected 1/1/1 assign/unassign/propagate, got %d/%d/%d", assigns, unassigns, propagations)
	}
	if !mock.propagations[0] {
		t.Fatal("preflight must probe with a dry run")
	}
}

func TestServerMountPreflightRequiresAuthorizationAndReleasesProbe(t *testing.T) {
	mock := &preflightMockControlPlane{redirectURI: "https://colab.research.google.com/consent?state=demo"}
	server := mock.server(t)
	defer server.Close()

	preflight := &ServerMountPreflight{Client: NewColabServerClient(server.URL, server.URL, server.Client())}
	err := preflight.CheckMount(context.Background(), DriveMountRequest{SessionID: "gpu", MountPath: "/content/drive"})
	if err == nil {
		t.Fatal("expected an authorization error")
	}
	var authErr *DriveAuthorizationRequiredError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected DriveAuthorizationRequiredError, got %T: %v", err, err)
	}
	if authErr.RedirectURI != "https://colab.research.google.com/consent?state=demo" {
		t.Fatalf("unexpected redirect URI %q", authErr.RedirectURI)
	}
	assigns, unassigns, _ := mock.counts()
	if assigns != 1 || unassigns != 1 {
		t.Fatalf("probe runtime must be released on the unauthorized path, got assign=%d unassign=%d", assigns, unassigns)
	}
}

func TestServerMountPreflightRequiresClientAndSession(t *testing.T) {
	if err := (&ServerMountPreflight{}).CheckMount(context.Background(), DriveMountRequest{SessionID: "gpu"}); err == nil {
		t.Fatal("expected an error without a client")
	}
	preflight := &ServerMountPreflight{Client: NewColabServerClient("http://127.0.0.1:1", "http://127.0.0.1:1", nil)}
	if err := preflight.CheckMount(context.Background(), DriveMountRequest{}); err == nil {
		t.Fatal("expected an error without a session id")
	}
}
