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

func TestServerControlPlaneAcquireAndReleaseMapToAssignAndUnassign(t *testing.T) {
	var assigned, unassigned string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, TunEndpoint+"/assign") {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"token":"xsrf"}`))
				return
			}
			assigned = r.Header.Get(HeaderXSRF)
			_ = json.NewEncoder(w).Encode(map[string]any{"endpoint": "https://proxy.example.test", "accelerator": "T4", "variant": "GPU", "runtimeProxyInfo": map[string]any{"token": "pt", "url": "https://proxy.example.test"}})
			return
		}
		if strings.HasPrefix(r.URL.Path, TunEndpoint+"/unassign/") {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "ux"})
				return
			}
			unassigned = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := NewColabServerClient(server.URL, server.URL, server.Client())
	plane := &ServerControlPlane{Client: client}
	runtime, err := plane.AcquireRuntime(context.Background(), RuntimeRequest{RunID: "run-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ID != "https://proxy.example.test" || assigned != "xsrf" {
		t.Fatalf("unexpected acquire: %#v assigned=%q", runtime, assigned)
	}
	if err := plane.ReleaseRuntime(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(unassigned, "/https://proxy.example.test") && !strings.Contains(unassigned, "proxy.example.test") {
		t.Fatalf("unexpected unassign path: %q", unassigned)
	}
}

func TestServerControlPlaneMapsAcceleratorToVariant(t *testing.T) {
	cases := []struct {
		abstract    string
		wantVariant string
		wantAccel   string
	}{
		{"gpu", "GPU", "T4"},
		{"tpu", "TPU", ""},
		{"cpu", "", ""},
		{"", "", ""},
		{"A100", "GPU", "A100"},
	}
	for _, tc := range cases {
		var assignQuery string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, TunEndpoint+"/assign") {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"token":"xsrf"}`))
					return
				}
				assignQuery = r.URL.RawQuery
				_ = json.NewEncoder(w).Encode(map[string]any{"endpoint": "https://proxy.example.test", "runtimeProxyInfo": map[string]any{"token": "pt", "url": "https://proxy.example.test"}})
				return
			}
			http.NotFound(w, r)
		}))
		client := NewColabServerClient(server.URL, server.URL, server.Client())
		plane := &ServerControlPlane{Client: client}
		if _, err := plane.AcquireRuntime(context.Background(), RuntimeRequest{RunID: "run-abc", Accelerator: tc.abstract}); err != nil {
			t.Fatal(err)
		}
		q, _ := url.ParseQuery(assignQuery)
		if q.Get("variant") != tc.wantVariant {
			t.Fatalf("abstract %q: variant = %q, want %q (query %q)", tc.abstract, q.Get("variant"), tc.wantVariant, assignQuery)
		}
		if q.Get("accelerator") != tc.wantAccel {
			t.Fatalf("abstract %q: accelerator = %q, want %q (query %q)", tc.abstract, q.Get("accelerator"), tc.wantAccel, assignQuery)
		}
		server.Close()
	}
}

func TestProxyNotebookExecutorPostsWithProxyToken(t *testing.T) {
	var token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get(HeaderProxyToken)
		_, _ = w.Write([]byte("out"))
	}))
	defer server.Close()
	executor := &ProxyNotebookExecutor{Client: server.Client(), BearerToken: "proxy-secret"}
	output, err := executor.ExecuteNotebook(context.Background(), Runtime{ID: server.URL}, []byte(`{"cells":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if output != "out" || token != "proxy-secret" {
		t.Fatalf("unexpected execution: out=%q token=%q", output, token)
	}
}
