package colab

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDrive is a minimal Drive v3 emulator: it keeps folders, files and resumable
// upload sessions in memory and serves the endpoints the REST store uses.
type fakeDrive struct {
	mu       sync.Mutex
	nextID   int
	entries  map[string]*fakeDriveEntry
	sessions map[string]*fakeDriveSession
	// uploads counts single-request uploads, so a test can prove that an
	// unchanged file was not transferred again.
	uploads int
	// chunkPuts counts chunk PUTs (status queries are not chunks).
	chunkPuts int
	// omitChecksum makes listings hide the checksum, like Google's native
	// document types which have no byte content to compare.
	omitChecksum bool
	// failChunks decides whether a chunk PUT fails, by 1-based chunk number.
	failChunks func(putNumber int) bool
	// commitThenFail keeps the bytes of a failing chunk, which is the "the
	// response was lost but the server stored it" case.
	commitThenFail bool
	// patchResumable records that a replace used the resumable update endpoint.
	patchResumable bool
}

type fakeDriveEntry struct {
	id, name, parent, mime string
	content                []byte
}

type fakeDriveSession struct {
	id       string
	name     string
	parent   string
	existing string
	total    int64
	received []byte
}

func newFakeDrive() *fakeDrive {
	drive := &fakeDrive{entries: map[string]*fakeDriveEntry{}, sessions: map[string]*fakeDriveSession{}}
	drive.entries["root"] = &fakeDriveEntry{id: "root", name: "root", mime: driveFolderMimeType}
	return drive
}

func (d *fakeDrive) id() string {
	d.nextID++
	return fmt.Sprintf("id-%d", d.nextID)
}

func (e *fakeDriveEntry) checksum() string {
	sum := md5.Sum(e.content)
	return hex.EncodeToString(sum[:])
}

func (d *fakeDrive) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/upload-session/"):
			d.handleSession(w, r)
		case strings.Contains(r.URL.Path, "/upload/drive/v3/files"):
			d.handleUpload(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/drive/v3/files":
			d.handleList(w, r)
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

func (d *fakeDrive) handleList(w http.ResponseWriter, r *http.Request) {
	parent, name, foldersOnly := parseFakeQuery(r.URL.Query().Get("q"))
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
		mapped := driveEntry{ID: entry.id, Name: entry.name, MimeType: entry.mime}
		// Folders and Google-native documents have no checksum; a binary file
		// reports one, which is what the incremental skip compares.
		if entry.mime != driveFolderMimeType && !d.omitChecksum {
			mapped.MD5Checksum = entry.checksum()
			mapped.Size = strconv.Itoa(len(entry.content))
		}
		files = append(files, mapped)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
}

func (d *fakeDrive) handleUpload(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files")
	existingID := strings.TrimPrefix(target, "/")
	if r.URL.Query().Get("uploadType") == "resumable" {
		d.handleResumableInit(w, r, existingID)
		return
	}
	d.uploads++
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
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

// handleResumableInit opens a session and answers with the URI the client must
// PUT chunks to, exactly like Drive's Location header.
func (d *fakeDrive) handleResumableInit(w http.ResponseWriter, r *http.Request, existingID string) {
	if r.Method == http.MethodPatch {
		d.patchResumable = true
	}
	var metadata struct {
		Name    string   `json:"name"`
		Parents []string `json:"parents"`
	}
	_ = json.NewDecoder(r.Body).Decode(&metadata)
	session := &fakeDriveSession{id: "session-" + d.id(), name: metadata.Name, existing: existingID}
	if len(metadata.Parents) > 0 {
		session.parent = metadata.Parents[0]
	}
	if existingID != "" {
		session.parent = d.entries[existingID].parent
	}
	if total := r.Header.Get("X-Upload-Content-Length"); total != "" {
		session.total, _ = strconv.ParseInt(total, 10, 64)
	}
	d.sessions[session.id] = session
	w.Header().Set("Location", "http://"+r.Host+"/upload-session/"+session.id)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func (d *fakeDrive) handleSession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/upload-session/")
	session, ok := d.sessions[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	contentRange := r.Header.Get("Content-Range")
	if strings.HasPrefix(contentRange, "bytes */") {
		// Status query: report what is committed, or the finished file.
		if int64(len(session.received)) >= session.total {
			d.finishSession(w, session)
			return
		}
		d.resumeIncomplete(w, session)
		return
	}
	start, _, total, ok := parseContentRange(contentRange)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// A chunk must start exactly where the upload stopped: anything else would
	// silently corrupt the file, so the fake refuses it like Drive does.
	if start != int64(len(session.received)) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid Content-Range"}}`))
		return
	}
	chunk, _ := io.ReadAll(r.Body)
	d.chunkPuts++
	failed := d.failChunks != nil && d.failChunks(d.chunkPuts)
	if failed && !d.commitThenFail {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"backend error"}}`))
		return
	}
	session.received = append(session.received, chunk...)
	session.total = total
	if failed {
		// The bytes were kept, but the client never learns that.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"backend error"}}`))
		return
	}
	if int64(len(session.received)) >= total {
		d.finishSession(w, session)
		return
	}
	d.resumeIncomplete(w, session)
}

func (d *fakeDrive) resumeIncomplete(w http.ResponseWriter, session *fakeDriveSession) {
	if len(session.received) > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(session.received)-1))
	}
	w.WriteHeader(driveResumeIncomplete)
}

func (d *fakeDrive) finishSession(w http.ResponseWriter, session *fakeDriveSession) {
	entry, ok := d.entries[session.existing]
	if !ok {
		entry = &fakeDriveEntry{id: d.id(), name: session.name, parent: session.parent}
		d.entries[entry.id] = entry
	}
	entry.content = append([]byte{}, session.received...)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": entry.id})
}

// parseContentRange reads "bytes start-end/total".
func parseContentRange(value string) (start, end, total int64, ok bool) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, false
	}
	rest := strings.TrimPrefix(value, "bytes ")
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return 0, 0, 0, false
	}
	total, err := strconv.ParseInt(rest[slash+1:], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	bounds := strings.SplitN(rest[:slash], "-", 2)
	if len(bounds) != 2 {
		return 0, 0, 0, false
	}
	start, err = strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	end, err = strconv.ParseInt(bounds[1], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	return start, end, total, true
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

// fastDriveRetries removes the retry sleeps so resume tests stay quick.
func fastDriveRetries(t *testing.T) {
	t.Helper()
	original := driveRetryBackoff
	driveRetryBackoff = time.Millisecond
	t.Cleanup(func() { driveRetryBackoff = original })
}

// smallChunks makes a small test file take the chunked path: it lowers both the
// chunk size and the single-request limit, so the file really is "too big".
func smallChunks(t *testing.T, chunk int64) {
	t.Helper()
	originalChunk, originalLimit := driveChunkSize, driveSimpleUploadLimit
	driveChunkSize = chunk
	driveSimpleUploadLimit = chunk
	t.Cleanup(func() {
		driveChunkSize = originalChunk
		driveSimpleUploadLimit = originalLimit
	})
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return target
}

func (d *fakeDrive) fileNames() map[string][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	files := map[string][]byte{}
	for _, entry := range d.entries {
		if entry.mime != driveFolderMimeType {
			files[entry.name] = entry.content
		}
	}
	return files
}

func (d *fakeDrive) folderCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	folders := 0
	for _, entry := range d.entries {
		if entry.mime == driveFolderMimeType {
			folders++
		}
	}
	return folders
}

func TestDriveRESTUploadCreatesFoldersAndReplacesFiles(t *testing.T) {
	drive := newFakeDrive()
	store := newTestStore(t, drive)
	ctx := context.Background()

	if _, err := store.Upload(ctx, "craftmake/outputs", "out.txt", writeTempFile(t, "out.txt", "first")); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// A second upload of different content must replace the existing file, not
	// duplicate it.
	if _, err := store.Upload(ctx, "craftmake/outputs", "out.txt", writeTempFile(t, "out2.txt", "second")); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	files := drive.fileNames()
	if len(files) != 1 || string(files["out.txt"]) != "second" {
		t.Fatalf("expected one replaced file, got %#v", files)
	}
	// Folder resolution must be cached: the second upload adds no new folders.
	if folders := drive.folderCount(); folders != 3 { // root + craftmake + outputs
		t.Fatalf("expected 3 folders (root, craftmake, outputs), got %d", folders)
	}
}

// TestDriveRESTSkipsIdenticalContent is the point of the incremental upload: an
// unchanged file costs one listing, not a transfer.
func TestDriveRESTSkipsIdenticalContent(t *testing.T) {
	drive := newFakeDrive()
	store := newTestStore(t, drive)
	ctx := context.Background()
	source := writeTempFile(t, "data.bin", "unchanged-content")

	first, err := store.Upload(ctx, "craftmake", "data.bin", source)
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if first.Skipped {
		t.Fatal("the first upload cannot be a skip")
	}
	transfers := drive.uploads

	second, err := store.Upload(ctx, "craftmake", "data.bin", source)
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if !second.Skipped {
		t.Fatal("identical content must be skipped")
	}
	if second.Size != int64(len("unchanged-content")) {
		t.Fatalf("skipped upload reported size %d", second.Size)
	}
	if drive.uploads != transfers {
		t.Fatalf("a skipped file must not be transferred again (%d -> %d)", transfers, drive.uploads)
	}

	// Changed content must be uploaded again.
	changed := writeTempFile(t, "data2.bin", "different-content!")
	third, err := store.Upload(ctx, "craftmake", "data.bin", changed)
	if err != nil {
		t.Fatalf("changed upload: %v", err)
	}
	if third.Skipped {
		t.Fatal("changed content must not be skipped")
	}
	if got := string(drive.fileNames()["data.bin"]); got != "different-content!" {
		t.Fatalf("Drive content = %q", got)
	}
}

// TestDriveRESTUploadsWhenChecksumIsMissing keeps the skip honest: without a
// checksum from Drive there is nothing to compare, so the file is sent.
func TestDriveRESTUploadsWhenChecksumIsMissing(t *testing.T) {
	drive := newFakeDrive()
	store := newTestStore(t, drive)
	ctx := context.Background()
	source := writeTempFile(t, "doc.txt", "same-bytes")
	if _, err := store.Upload(ctx, "craftmake", "doc.txt", source); err != nil {
		t.Fatal(err)
	}
	drive.omitChecksum = true
	result, err := store.Upload(ctx, "craftmake", "doc.txt", source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped {
		t.Fatal("a file without a Drive checksum must be uploaded rather than assumed identical")
	}
}

// TestDriveRESTLargeFileUsesChunkedResumableUpload covers the chunked path.
func TestDriveRESTLargeFileUsesChunkedResumableUpload(t *testing.T) {
	drive := newFakeDrive()
	store := newTestStore(t, drive)
	smallChunks(t, 4)
	fastDriveRetries(t)
	ctx := context.Background()
	content := "0123456789ab" // 12 bytes -> 3 chunks of 4
	source := writeTempFile(t, "big.bin", content)

	result, err := store.Upload(ctx, "craftmake", "big.bin", source)
	if err != nil {
		t.Fatalf("chunked upload: %v", err)
	}
	if drive.chunkPuts != 3 {
		t.Fatalf("expected 3 chunk PUTs, got %d", drive.chunkPuts)
	}
	if got := string(drive.fileNames()["big.bin"]); got != content {
		t.Fatalf("Drive content = %q, want %q", got, content)
	}
	if result.Resumed {
		t.Fatal("a fresh upload is not a resume")
	}
	if drive.uploads != 0 {
		t.Fatalf("a large file must not use a single-request upload (%d)", drive.uploads)
	}
}

// TestDriveRESTLargeUploadReplacesExistingFile checks the resumable *update*
// endpoint is used when the file already exists, so no duplicate is created.
func TestDriveRESTLargeUploadReplacesExistingFile(t *testing.T) {
	drive := newFakeDrive()
	store := newTestStore(t, drive)
	smallChunks(t, 4)
	fastDriveRetries(t)
	ctx := context.Background()
	if _, err := store.Upload(ctx, "craftmake", "big.bin", writeTempFile(t, "old.bin", "old-content-here")); err != nil {
		t.Fatal(err)
	}
	result, err := store.Upload(ctx, "craftmake", "big.bin", writeTempFile(t, "new.bin", "new-content-here"))
	if err != nil {
		t.Fatalf("resumable replace: %v", err)
	}
	if !drive.patchResumable {
		t.Fatal("replacing a large file must use the resumable update endpoint")
	}
	files := drive.fileNames()
	if len(files) != 1 || string(files["big.bin"]) != "new-content-here" {
		t.Fatalf("expected one replaced file, got %#v", files)
	}
	if result.Resumed {
		t.Fatal("a fresh replacement is not a resume")
	}
}

// TestDriveRESTRecoversWhenTheServerKeptAChunk is the corruption guard: the
// server stored a chunk but the response was lost, so resending it blindly would
// duplicate bytes. The client must ask for the committed offset instead.
func TestDriveRESTRecoversWhenTheServerKeptAChunk(t *testing.T) {
	drive := newFakeDrive()
	drive.commitThenFail = true
	drive.failChunks = func(putNumber int) bool { return putNumber == 2 }
	store := newTestStore(t, drive)
	smallChunks(t, 4)
	fastDriveRetries(t)
	ctx := context.Background()
	content := "0123456789ab"
	source := writeTempFile(t, "big.bin", content)

	result, err := store.Upload(ctx, "craftmake", "big.bin", source)
	if err != nil {
		t.Fatalf("upload after a lost response: %v", err)
	}
	if got := string(drive.fileNames()["big.bin"]); got != content {
		t.Fatalf("Drive content = %q, want %q (a resent chunk would corrupt it)", got, content)
	}
	if drive.chunkPuts != 3 {
		t.Fatalf("the kept chunk must not be sent again: %d chunk PUTs", drive.chunkPuts)
	}
	if !result.Resumed {
		t.Fatal("continuing past a chunk the server kept is a resume")
	}
}

// TestDriveRESTResumesAcrossRuns covers a killed process: the session is on disk,
// so the next run finishes the transfer instead of starting over.
func TestDriveRESTResumesAcrossRuns(t *testing.T) {
	drive := newFakeDrive()
	smallChunks(t, 4)
	fastDriveRetries(t)
	ctx := context.Background()
	content := "0123456789abcdef" // 16 bytes -> 4 chunks
	source := writeTempFile(t, "checkpoint.bin", content)
	sessionDir := t.TempDir()

	// The first run stops after the first chunk.
	drive.failChunks = func(putNumber int) bool { return putNumber >= 2 }
	first := newTestStore(t, drive)
	first.SessionDir = sessionDir
	if _, err := first.Upload(ctx, "craftmake", "checkpoint.bin", source); err == nil {
		t.Fatal("the interrupted upload must report a failure")
	}
	if drive.chunkPuts < 2 {
		t.Fatalf("the first run should have sent at least one chunk, got %d", drive.chunkPuts)
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("the resumable session must be remembered: %v", err)
	}
	uploaded := len(drive.fileNames()["checkpoint.bin"])
	if uploaded != 0 {
		t.Fatalf("the interrupted upload must not have produced a file, got %d bytes", uploaded)
	}

	// The next run continues from the committed offset.
	drive.failChunks = nil
	drive.chunkPuts = 0
	second := newTestStore(t, drive)
	second.SessionDir = sessionDir
	result, err := second.Upload(ctx, "craftmake", "checkpoint.bin", source)
	if err != nil {
		t.Fatalf("resumed upload: %v", err)
	}
	if !result.Resumed {
		t.Fatal("the second run must report that it resumed")
	}
	if drive.chunkPuts != 3 {
		t.Fatalf("the resumed run should send only the missing chunks (3), got %d", drive.chunkPuts)
	}
	if got := string(drive.fileNames()["checkpoint.bin"]); got != content {
		t.Fatalf("Drive content = %q, want %q", got, content)
	}
	// A finished upload must forget its session, so the next run starts clean.
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the session file must be cleared after success: %v", entries)
	}
}

// TestDriveRESTAbandonsAnExpiredSession keeps a stale session from wedging a run.
func TestDriveRESTAbandonsAnExpiredSession(t *testing.T) {
	drive := newFakeDrive()
	smallChunks(t, 4)
	fastDriveRetries(t)
	ctx := context.Background()
	content := "0123456789ab"
	source := writeTempFile(t, "big.bin", content)
	sessionDir := t.TempDir()

	store := newTestStore(t, drive)
	store.SessionDir = sessionDir
	// A session the server no longer knows about.
	checksum, err := fileMD5(source)
	if err != nil {
		t.Fatal(err)
	}
	store.saveSession(&driveUploadSession{
		URI: store.APIBase + "/upload-session/gone", Folder: "craftmake",
		Name: "big.bin", Size: int64(len(content)), MD5: checksum, Uploaded: 4,
	})
	result, err := store.Upload(ctx, "craftmake", "big.bin", source)
	if err != nil {
		t.Fatalf("an expired session must not fail the upload: %v", err)
	}
	if got := string(drive.fileNames()["big.bin"]); got != content {
		t.Fatalf("Drive content = %q, want %q", got, content)
	}
	if result.Resumed {
		t.Fatal("nothing was resumed: the session was gone")
	}
}

// TestDriveRESTStoppedUploadReportsProgress keeps the failure actionable, and
// keeps the session URI (a capability) out of the message.
func TestDriveRESTStoppedUploadReportsProgress(t *testing.T) {
	drive := newFakeDrive()
	drive.failChunks = func(int) bool { return true }
	store := newTestStore(t, drive)
	smallChunks(t, 4)
	fastDriveRetries(t)
	sessionDir := t.TempDir()
	store.SessionDir = sessionDir
	content := "0123456789ab"

	_, err := store.Upload(context.Background(), "craftmake", "big.bin", writeTempFile(t, "big.bin", content))
	if err == nil {
		t.Fatal("a permanently failing upload must report an error")
	}
	message := err.Error()
	if !strings.Contains(message, "stopped at 0 of 12 bytes") {
		t.Fatalf("the error should say how far it got: %v", err)
	}
	if !strings.Contains(message, "continues it") {
		t.Fatalf("the error should say the session is reusable: %v", err)
	}
	for _, entry := range drive.sessions {
		if strings.Contains(message, "session-") {
			t.Fatalf("the session URI must not leak into the error: %v", err)
		}
		_ = entry
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
		source := writeTempFile(t, path.Base(name), data)
		if _, err := store.Upload(ctx, path.Dir(name), path.Base(name), source); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
	}
	got := map[string]string{}
	sizes := map[string]int64{}
	if err := store.Walk(ctx, "craftmake", func(relativePath string, size int64, content io.Reader) error {
		data, err := io.ReadAll(content)
		if err != nil {
			return err
		}
		got[relativePath] = string(data)
		sizes[relativePath] = size
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
		if sizes[key] != int64(len(value)) {
			t.Fatalf("walk[%q] size = %d, want %d", key, sizes[key], len(value))
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
	_, err := store.Upload(context.Background(), "craftmake", "x.txt", writeTempFile(t, "x.txt", "x"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "colab drive login") {
		t.Fatalf("401 must point at the re-login command, got: %v", err)
	}
}

func TestDriveRESTRequiresToken(t *testing.T) {
	store := NewDriveRESTClient(func() (string, error) { return "", nil }, nil)
	_, err := store.Upload(context.Background(), "", "x.txt", writeTempFile(t, "x.txt", "x"))
	if err == nil || !strings.Contains(err.Error(), "drive login") {
		t.Fatalf("expected an actionable missing-token error, got %v", err)
	}
}

func TestDriveRESTUploadMissingFileIsReported(t *testing.T) {
	store := newTestStore(t, newFakeDrive())
	_, err := store.Upload(context.Background(), "craftmake", "gone.txt", filepath.Join(t.TempDir(), "missing.txt"))
	if err == nil {
		t.Fatal("uploading a file that does not exist must fail")
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
	fastDriveRetries(t)

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
	fastDriveRetries(t)

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
