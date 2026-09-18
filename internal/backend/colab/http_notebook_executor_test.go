package colab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestHTTPNotebookExecutorPostsAndReturnsDecodableOutput(t *testing.T) {
	var auth, contentType, runtimeID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, contentType, runtimeID = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path
		var body struct {
			RuntimeID string          `json:"runtime_id"`
			Notebook  json.RawMessage `json:"notebook"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		payload := fmt.Sprintf("CRAFTMAKE_TASK_RESULT_BEGIN\n{\"protocol_version\":%d,\"run_id\":\"run-1\",\"task_id\":\"task-1\",\"attempt\":1,\"status\":\"succeeded\",\"steps\":[]}\nCRAFTMAKE_TASK_RESULT_END\n", protocol.Version)
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()
	executor := NewHTTPNotebookExecutor(server.URL, "token", server.Client())
	notebook, _ := json.Marshal(map[string]any{"cells": []any{}})
	output, err := executor.ExecuteNotebook(context.Background(), Runtime{ID: "rt-1"}, notebook)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer token" || contentType != "application/json" || runtimeID != "/kernel/execute" {
		t.Fatalf("unexpected request: auth=%q type=%q path=%q", auth, contentType, runtimeID)
	}
	result, err := DecodeTaskResult(output)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID != "run-1" || result.TaskID != "task-1" {
		t.Fatalf("unexpected result: %#v", result)
	}
}
