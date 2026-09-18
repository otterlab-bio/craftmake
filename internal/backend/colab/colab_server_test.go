package colab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNotebookHashIsWebSafeBase64SHA256(t *testing.T) {
	h := notebookHash("run-1")
	if len(h) != 44 {
		t.Fatalf("nbh length = %d, want 44 (NBH-Regex): %q", len(h), h)
	}
	if strings.ContainsAny(h, "+/=") {
		t.Fatalf("nbh not web-safe: %q", h)
	}
	if notebookHash("run-1") != h {
		t.Fatal("nbh not deterministic")
	}
	if notebookHash("run-2") == h {
		t.Fatal("different inputs should differ")
	}
}

func TestStripXSSIRemovesColabPrefix(t *testing.T) {
	if got := string(stripXSSI([]byte(")]}'\n{\"a\":1}"))); got != "{\"a\":1}" {
		t.Fatalf("stripXSSI = %q", got)
	}
	if got := string(stripXSSI([]byte("{\"a\":1}"))); got != "{\"a\":1}" {
		t.Fatalf("stripXSSI no-prefix = %q", got)
	}
}

func TestColabServerClientAssignParamsMatchColabVSCode(t *testing.T) {
	var assignURL string
	var appName, extVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, TunEndpoint+"/assign") {
			assignURL = r.URL.RawQuery
			appName = r.Header.Get(HeaderVSAppName)
			extVersion = r.Header.Get(HeaderVSExtVersion)
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "xsrf"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"endpoint": "proxy.example.test", "runtimeProxyInfo": map[string]any{"token": "pt", "url": "https://proxy.example.test"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := NewColabServerClient(server.URL, server.URL, server.Client())
	client.AppName = "craftmake"
	client.ExtensionVersion = "0.1.0"
	_, err := client.Assign(context.Background(), RuntimeSpec{NotebookHash: notebookHash("run-1"), Accelerator: "T4", Version: "2025.10"})
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(assignURL)
	if q.Get("accelerator") != "T4" {
		t.Fatalf("accelerator param = %q, want T4", q.Get("accelerator"))
	}
	if q.Get("runtime_version_label") != "2025.10" {
		t.Fatalf("runtime_version_label = %q", q.Get("runtime_version_label"))
	}
	if q.Get("acc") != "" {
		t.Fatalf("should not use acc param: %q", q.Get("acc"))
	}
	if q.Get("version") != "" {
		t.Fatalf("should not use version param: %q", q.Get("version"))
	}
	if q.Get("variant") != "" {
		t.Fatalf("variant should be omitted for DEFAULT: %q", q.Get("variant"))
	}
	if q.Get("shape") != "" {
		t.Fatalf("shape should be omitted for STANDARD: %q", q.Get("shape"))
	}
	if q.Get("authuser") != "0" {
		t.Fatalf("authuser = %q, want 0", q.Get("authuser"))
	}
	if appName != "craftmake" || extVersion != "0.1.0" {
		t.Fatalf("headers: app=%q ext=%q", appName, extVersion)
	}
}

func TestColabServerClientAssignKeepsAliveRefreshesAndUnassigns(t *testing.T) {
	var xsrfSeen, authSeen string
	colab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen = r.Header.Get(HeaderAuthorization)
		if !strings.HasPrefix(r.URL.Path, TunEndpoint+"/assign") && !strings.HasPrefix(r.URL.Path, TunEndpoint+"/unassign/") && !strings.HasSuffix(r.URL.Path, "/keep-alive/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if strings.Contains(r.URL.Path, "/assign") {
				_ = json.NewEncoder(w).Encode(map[string]any{"acc": "T4", "nbh": "abc", "p": false, "token": "xsrf-1", "variant": "VARIANT_GPU"})
				return
			}
			if strings.Contains(r.URL.Path, "/unassign/") {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "unassign-xsrf"})
				return
			}
			w.WriteHeader(http.StatusOK) // keep-alive
			return
		case http.MethodPost:
			xsrfSeen = r.Header.Get(HeaderXSRF)
			if strings.Contains(r.URL.Path, "/assign") {
				_ = json.NewEncoder(w).Encode(map[string]any{"accelerator": "T4", "endpoint": "proxy.example.test", "variant": "GPU", "machineShape": 0, "runtimeProxyInfo": map[string]any{"token": "proxy-tok", "tokenExpiresInSeconds": 3600, "url": "https://proxy.example.test"}})
				return
			}
			w.WriteHeader(http.StatusNoContent) // unassign
		}
	}))
	defer colab.Close()
	gapi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runtime-proxy-token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "fresh", "tokenTtl": "3600s", "url": "https://proxy.example.test"})
			return
		}
		if r.URL.Path == "/v1/user-info" {
			_ = json.NewEncoder(w).Encode(map[string]any{"subscriptionTier": "SUBSCRIPTION_TIER_NONE", "eligibleAccelerators": []any{}, "ineligibleAccelerators": []any{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer gapi.Close()
	client := NewColabServerClient(colab.URL, gapi.URL, colab.Client())
	client.GetAccessToken = func() (string, error) { return "access-tok", nil }
	assignment, err := client.Assign(context.Background(), RuntimeSpec{NotebookHash: "abc", Variant: "GPU", Accelerator: "T4"})
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Endpoint != "proxy.example.test" || authSeen != "Bearer access-tok" || xsrfSeen != "xsrf-1" {
		t.Fatalf("unexpected assign: %#v xsrf=%q auth=%q", assignment, xsrfSeen, authSeen)
	}
	if err := client.KeepAlive(context.Background(), assignment.Endpoint); err != nil {
		t.Fatal(err)
	}
	proxy, err := client.RefreshProxy(context.Background(), assignment.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if proxy.Token != "fresh" {
		t.Fatalf("unexpected proxy: %#v", proxy)
	}
	if _, err := client.GetUserInfo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Unassign(context.Background(), assignment.Endpoint); err != nil {
		t.Fatal(err)
	}
}
