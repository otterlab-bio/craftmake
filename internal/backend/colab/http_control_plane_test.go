package colab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPControlPlaneAssignsAndReleasesRuntime(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing auth header")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/runtime/assign" {
			_ = json.NewEncoder(w).Encode(map[string]string{"runtime_id": "runtime-1"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := NewHTTPControlPlane(server.URL, "secret", server.Client())
	runtime, err := client.AcquireRuntime(context.Background(), RuntimeRequest{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ID != "runtime-1" {
		t.Fatalf("unexpected runtime: %#v", runtime)
	}
	if err := client.ReleaseRuntime(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/runtime/assign" || paths[1] != "/runtime/unassign" {
		t.Fatalf("unexpected paths: %#v", paths)
	}
}
