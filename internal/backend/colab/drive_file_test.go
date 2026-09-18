package colab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDriveFileClientGetAndPutOverHTTP(t *testing.T) {
	var receivedPath string
	var receivedBytes []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		receivedPath, receivedBytes = body.Path, body.Content
		if r.URL.Path == "/drive/read" {
			_, _ = w.Write([]byte("remote-data"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := NewDriveFileClient(server.URL, "token", server.Client())
	if err := client.Put(context.Background(), "/remote/file.txt", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if receivedPath != "/remote/file.txt" || string(receivedBytes) != "payload" {
		t.Fatalf("unexpected put: path=%q bytes=%q", receivedPath, receivedBytes)
	}
	data, err := client.Get(context.Background(), "/remote/out.txt")
	if err != nil {
		t.Fatal(err)
	}
	if receivedPath != "/remote/out.txt" || string(data) != "remote-data" {
		t.Fatalf("unexpected get: path=%q data=%q", receivedPath, data)
	}
}
