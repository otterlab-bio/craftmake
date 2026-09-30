package colab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDrive is a minimal Drive v3 emulator: it keeps folders and files in memory
// and serves the endpoints the REST store uses.
type fakeDrive struct {
	mu      sync.Mutex
	nextID  int
	entries map[string]*fakeDriveEntry
	uploads int
}

type fakeDriveEntry struct {
	id, name, parent, mime string
	content                []byte
}

func newFakeDrive() *fakeDrive {
	drive := &fakeDrive{entries: map[string]*fakeDriveEntry{}}
	drive.entries["root"] = &fakeDriveEntry{id: "root", name: "root", mime: driveFolderMimeType}
	return drive
}

func (d *fakeDrive) id() string {
	d.nextID++
	return fmt.Sprintf("id-%d", d.nextID)
}

func (d *fakeDrive) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if strings.Contains(r.URL.Path, "/upload/drive/v3/files") {
			d.handleUpload(w, r)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/drive/v3/files":
			query := r.URL.Query().Get("q")
			parent, name, foldersOnly := parseFakeQuery(query)
			files := []driveEntry{}
			for _, entry := range d.entries {
				if entry.parent != parent {
					continue
				}
				if name != "" && entry.name != name {
					continue
				}
				if foldersOnly && entry.mime != driveFolderMimeType {
					continue
				}
				files = append(files, driveEntry{ID: entry.id, Name: entry.name, MimeType: entry.mime})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
		case r.Method == http.MethodPost && r.URL.Path == "/drive/v3/files":
			var payload struct {
				Name    string   `json:"name"`
				Mime    string   `json:"mimeType"`
				Parents []string `json:"parents"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			parent := "root"
			if len(payload.Parents) > 0 {
				parent = payload.Parents[0]
			}
			entry := &fakeDriveEntry{id: d.id(), name: payload.Name, parent: parent, mime: payload.Mime}
			d.entries[entry.id] = entry
			_ = json.NewEncoder(w).Encode(map[string]any{"id": entry.id})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/drive/v3/files/"):
			id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
			entry, ok := d.entries[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.URL.Query().Get("alt") == "media" {
				_, _ = w.Write(entry.content)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": entry.id, "name": entry.name, "mimeType": entry.mime})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (d *fakeDrive) handleUpload(w http.ResponseWriter, r *http.Request) {
	d.uploads++
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	target := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files")
	existingID := strings.TrimPrefix(target, "/")
	if mediaType == "multipart/form-data" {
		reader := multipart.NewReader(r.Body, params["boundary"])
		var metadata map[string]any
		var content []byte
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(part)
			if strings.Contains(part.Header.Get("Content-Type"), "application/json") {
				_ = json.Unmarshal(body, &metadata)
			} else {
				content = body
			}
		}
		parents, _ := metadata["parents"].([]any)
		parent := "root"
		if len(parents) > 0 {
			parent, _ = parents[0].(string)
		}
		name, _ := metadata["name"].(string)
		entry := &fakeDriveEntry{id: d.id(), name: name, parent: parent, content: content}
		d.entries[entry.id] = entry
		_ = json.NewEncoder(w).Encode(map[string]any{"id": entry.id})
		return
	}
	entry, ok := d.entries[existingID]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	entry.content, _ = io.ReadAll(r.Body)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": entry.id})
}

// parseFakeQuery understands the subset of Drive query syntax the store emits.
func parseFakeQuery(query string) (parent, name string, foldersOnly bool) {
	if index := strings.Index(query, "name = '"); index >= 0 {
		rest := query[index+len("name = '"):]
		if end := strings.Index(rest, "'"); end >= 0 {
			name = rest[:end]
		}
	}
	if index := strings.Index(query, "' in parents"); index >= 0 {
		start := strings.LastIndex(query[:index], "'")
		if start >= 0 {
			parent = query[start+1 : index]
		}
	}
	foldersOnly = strings.Contains(query, driveFolderMimeType)
	return parent, name, foldersOnly
}

func newTestStore(t *testing.T, drive *fakeDrive) *DriveRESTClient {
	t.Helper()
	server := httptest.NewServer(drive.handler())
	t.Cleanup(server.Close)
	store := NewDriveRESTClient(func() (string, error) { return "test-token", nil }, server.Client())
	store.APIBase = server.URL + "/drive/v3"
	store.UploadBase = server.URL + "/upload/drive/v3"
	return store
}

func TestDriveRESTUploadCreatesFoldersAndReplacesFiles(t *testing.T) {
	drive := newFakeDrive()
	store := newTestStore(t, drive)
	ctx := context.Background()

	if err := store.Upload(ctx, "craftmake/outputs", "out.txt", []byte("first")); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// A second upload must replace the existing file, not duplicate it.
	if err := store.Upload(ctx, "craftmake/outputs", "out.txt", []byte("second")); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	files := 0
	var content string
	for _, entry := range drive.entries {
		if entry.name == "out.txt" {
			files++
			content = string(entry.content)
		}
	}
	if files != 1 {
		t.Fatalf("expected one file after replacement, got %d", files)
	}
	if content != "second" {
		t.Fatalf("content = %q, want the replacement", content)
	}
	// Folder resolution must be cached: the second upload adds no new folders.
	folders := 0
	for _, entry := range drive.entries {
		if entry.mime == driveFolderMimeType {
			folders++
		}
	}
	if folders != 3 { // root + craftmake + outputs
		t.Fatalf("expected 3 folders (root, craftmake, outputs), got %d", folders)
	}
}

func TestDriveRESTWalkRestoresNestedTree(t *testing.T) {
	drive := newFakeDrive()
	store := newTestStore(t, drive)
	ctx := context.Background()
	for name, data := range map[string]string{
		"craftmake/work/input.txt":        "in",
		"craftmake/outputs/out.txt":       "out",
		"craftmake/outputs/deep/deep.txt": "deep",
	} {
		if err := store.Upload(ctx, path.Dir(name), path.Base(name), []byte(data)); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
	}
	got := map[string]string{}
	if err := store.Walk(ctx, "craftmake", func(relativePath string, data []byte) error {
		got[relativePath] = string(data)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := map[string]string{
		"work/input.txt":        "in",
		"outputs/out.txt":       "out",
		"outputs/deep/deep.txt": "deep",
	}
	if len(got) != len(want) {
		t.Fatalf("walk returned %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("walk[%q] = %q, want %q", key, got[key], value)
		}
	}
}

func TestDriveRESTUnauthorizedIsActionable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid Credentials"}}`))
	}))
	defer server.Close()
	store := NewDriveRESTClient(func() (string, error) { return "stale", nil }, server.Client())
	store.APIBase = server.URL
	err := store.Upload(context.Background(), "craftmake", "x.txt", []byte("x"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "colab drive login") {
		t.Fatalf("401 must point at the re-login command, got: %v", err)
	}
}

func TestDriveRESTRequiresToken(t *testing.T) {
	store := NewDriveRESTClient(func() (string, error) { return "", nil }, nil)
	if err := store.Upload(context.Background(), "", "x.txt", []byte("x")); err == nil || !strings.Contains(err.Error(), "drive login") {
		t.Fatalf("expected an actionable missing-token error, got %v", err)
	}
}

func TestNormalizeDriveTransport(t *testing.T) {
	cases := map[string]string{
		"":         DriveTransportKernel,
		"kernel":   DriveTransportKernel,
		"whatever": DriveTransportKernel,
		"rest":     DriveTransportREST,
		"api":      DriveTransportREST,
		"drive":    DriveTransportREST,
	}
	for input, want := range cases {
		if got := NormalizeDriveTransport(input); got != want {
			t.Fatalf("NormalizeDriveTransport(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestDriveRESTRetriesSharedQuotaRejection pins the fix for the live failure
// where the bundled rclone client's shared per-project quota returned 403
// PERMISSION_DENIED with reason rateLimitExceeded, which is transient.
func TestDriveRESTRetriesSharedQuotaRejection(t *testing.T) {
	original := driveRetryBackoff
	driveRetryBackoff = time.Millisecond
	defer func() { driveRetryBackoff = original }()

	var listAttempts int
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listAttempts++
			if listAttempts <= 2 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Quota exceeded for quota metric 'Queries'","errors":[{"reason":"rateLimitExceeded"}]}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
		case http.MethodPost:
			created = true
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "folder-1"})
		}
	}))
	defer server.Close()
	store := NewDriveRESTClient(func() (string, error) { return "token", nil }, server.Client())
	store.APIBase = server.URL
	// A quota rejection must be retried, not surfaced.
	if _, err := store.EnsureFolder(context.Background(), "craftmake"); err != nil {
		t.Fatalf("shared quota rejection should be retried: %v", err)
	}
	if listAttempts != 3 {
		t.Fatalf("the rejected lookups must be retried: %d attempts", listAttempts)
	}
	if !created {
		t.Fatal("the folder should be created once the quota clears")
	}
}

// TestDriveRESTQuotaErrorIsHonest keeps the message from blaming permissions.
func TestDriveRESTQuotaErrorIsHonest(t *testing.T) {
	original := driveRetryBackoff
	driveRetryBackoff = time.Millisecond
	defer func() { driveRetryBackoff = original }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"Quota exceeded","errors":[{"reason":"rateLimitExceeded"}]}}`))
	}))
	defer server.Close()
	store := NewDriveRESTClient(func() (string, error) { return "token", nil }, server.Client())
	store.APIBase = server.URL
	_, err := store.EnsureFolder(context.Background(), "craftmake")
	if err == nil {
		t.Fatal("expected the quota error to surface after retries")
	}
	if !strings.Contains(err.Error(), "shared Drive OAuth client's quota") {
		t.Fatalf("quota exhaustion must be described as such, got: %v", err)
	}
	if strings.Contains(err.Error(), "lacks access") {
		t.Fatalf("quota exhaustion must not be reported as a permission problem: %v", err)
	}
}
