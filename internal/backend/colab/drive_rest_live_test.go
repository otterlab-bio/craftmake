package colab

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live integration test for the Drive REST transport.
//
// The unit tests drive a local emulator, which cannot prove that the resumable
// protocol details - the Location header, the 308/Range acknowledgement, the
// md5Checksum Drive reports - behave as assumed against the real service. This
// test does exactly that, and is skipped unless it is asked for:
//
//	CRAFTMAKE_DRIVE_LIVE=1 \
//	CRAFTMAKE_DRIVE_LIVE_REFRESH_TOKEN=<refresh token> \
//	go test ./internal/backend/colab/ -run TestDriveRESTLive -v
//
// It writes into CRAFTMAKE_DRIVE_LIVE_FOLDER (default craftmake-rest-live-verify)
// and deletes what it created.

// The bundled Drive OAuth client, matching defaultDriveClientID/Secret in
// internal/cli/colab_drive_login.go, which is the source of truth for the auth
// flow. Google's installed-app client secrets are not confidential by design.
// Override either through the environment.
const liveDriveClientID = "202264815644.apps.googleusercontent.com"
const liveDriveClientSecret = "X4Z3ca8xfWDb1Voo-F9a7ZxJ"

func liveDriveTokenFetcher(t *testing.T) func() (string, error) {
	t.Helper()
	refreshToken := strings.TrimSpace(os.Getenv("CRAFTMAKE_DRIVE_LIVE_REFRESH_TOKEN"))
	if refreshToken == "" {
		t.Skip("CRAFTMAKE_DRIVE_LIVE_REFRESH_TOKEN is not set")
	}
	clientID := envOr("CRAFTMAKE_DRIVE_LIVE_CLIENT_ID", liveDriveClientID)
	clientSecret := envOr("CRAFTMAKE_DRIVE_LIVE_CLIENT_SECRET", liveDriveClientSecret)
	return func() (string, error) {
		form := url.Values{
			"client_id":     {clientID},
			"client_secret": {clientSecret},
			"grant_type":    {"refresh_token"},
			"refresh_token": {refreshToken},
		}
		response, err := http.PostForm("https://oauth2.googleapis.com/token", form)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		if response.StatusCode != http.StatusOK {
			return "", fmt.Errorf("token endpoint returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
		}
		var payload struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", err
		}
		if payload.AccessToken == "" {
			return "", fmt.Errorf("token endpoint returned no access token")
		}
		return payload.AccessToken, nil
	}
}

// liveDriveDelete removes files the test created, so a live run leaves no litter.
func liveDriveDelete(t *testing.T, token func() (string, error), folder string) {
	t.Helper()
	accessToken, err := token()
	if err != nil {
		t.Logf("cleanup: no access token: %v", err)
		return
	}
	client := http.DefaultClient
	query := fmt.Sprintf("name = '%s' and mimeType = '%s' and trashed = false", escapeDriveQuery(folder), driveFolderMimeType)
	listURL := "https://www.googleapis.com/drive/v3/files?fields=files(id)&q=" + url.QueryEscape(query)
	request, _ := http.NewRequest(http.MethodGet, listURL, nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := client.Do(request)
	if err != nil {
		t.Logf("cleanup: list failed: %v", err)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	response.Body.Close()
	var listed struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &listed); err != nil || len(listed.Files) == 0 {
		return
	}
	for _, entry := range listed.Files {
		deleteRequest, _ := http.NewRequest(http.MethodDelete, "https://www.googleapis.com/drive/v3/files/"+entry.ID, nil)
		deleteRequest.Header.Set("Authorization", "Bearer "+accessToken)
		if deleteResponse, err := client.Do(deleteRequest); err == nil {
			deleteResponse.Body.Close()
		}
	}
}

func TestDriveRESTLiveChunkedUploadSkipAndRestore(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CRAFTMAKE_DRIVE_LIVE")) == "" {
		t.Skip("set CRAFTMAKE_DRIVE_LIVE=1 to run against the real Drive API")
	}
	originalChunk, originalLimit, originalBackoff := driveChunkSize, driveSimpleUploadLimit, driveRetryBackoff
	// Keep the real 5 MiB threshold but a smaller chunk, so a 12 MiB file still
	// exercises several chunks without moving 24 MiB.
	driveChunkSize = 4 << 20
	// The bundled rclone client's quota is shared with every rclone user, so a
	// live run can be rejected while the project is saturated. Wait it out rather
	// than reading it as a bug in this code.
	driveRetryBackoff = 20 * time.Second
	t.Cleanup(func() {
		driveChunkSize, driveSimpleUploadLimit, driveRetryBackoff = originalChunk, originalLimit, originalBackoff
	})

	token := liveDriveTokenFetcher(t)
	store := NewDriveRESTClient(token, nil)
	folder := envOr("CRAFTMAKE_DRIVE_LIVE_FOLDER", "craftmake-rest-live-verify")
	t.Cleanup(func() { liveDriveDelete(t, token, folder) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// A 12 MiB payload: above the 5 MiB single-request limit, several chunks.
	payload := make([]byte, 12<<20)
	for index := range payload {
		payload[index] = byte(index % 251)
	}
	localPath := filepath.Join(t.TempDir(), "checkpoint.bin")
	if err := os.WriteFile(localPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	result, err := store.Upload(ctx, folder, "checkpoint.bin", localPath)
	if err != nil {
		t.Fatalf("live chunked upload: %v", err)
	}
	if result.Skipped {
		t.Fatal("the first upload cannot be a skip")
	}
	if result.Size != int64(len(payload)) {
		t.Fatalf("uploaded size = %d, want %d", result.Size, len(payload))
	}
	t.Logf("live upload of %d bytes took %s (id %s)", result.Size, time.Since(started).Round(time.Millisecond), result.ID)

	// The real service must report the checksum the skip decision relies on.
	second, err := store.Upload(ctx, folder, "checkpoint.bin", localPath)
	if err != nil {
		t.Fatalf("live re-upload: %v", err)
	}
	if !second.Skipped {
		t.Fatalf("real Drive did not report a matching md5Checksum, so the incremental skip cannot work (id %s)", second.ID)
	}
	t.Logf("live re-upload was skipped by checksum")

	// Streaming restore must reproduce the bytes exactly.
	var restored bytes.Buffer
	found := false
	if err := store.Walk(ctx, folder, func(relativePath string, size int64, content io.Reader) error {
		if relativePath != "checkpoint.bin" {
			return nil
		}
		found = true
		if size != int64(len(payload)) {
			t.Fatalf("restored size = %d, want %d", size, len(payload))
		}
		_, err := io.Copy(&restored, content)
		return err
	}); err != nil {
		t.Fatalf("live restore: %v", err)
	}
	if !found {
		t.Fatal("the uploaded file was not found by Walk")
	}
	if !bytes.Equal(restored.Bytes(), payload) {
		sum := md5.Sum(restored.Bytes())
		want := md5.Sum(payload)
		t.Fatalf("restored content differs: md5 %s want %s", hex.EncodeToString(sum[:]), hex.EncodeToString(want[:]))
	}
	t.Logf("live restore of %d bytes matched byte for byte", restored.Len())

	// Changed content must be uploaded, not skipped.
	payload[len(payload)-1] ^= 0xff
	if err := os.WriteFile(localPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := store.Upload(ctx, folder, "checkpoint.bin", localPath)
	if err != nil {
		t.Fatalf("live replacement: %v", err)
	}
	if third.Skipped {
		t.Fatal("changed content must not be skipped")
	}
	// And the replacement must not have created a second file.
	names := 0
	if err := store.Walk(ctx, folder, func(string, int64, io.Reader) error {
		names++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if names != 1 {
		t.Fatalf("expected one file after replacement, got %d", names)
	}
	t.Logf("live replacement reused the existing file")
}

// isChunkPut reports whether a request sends a chunk of a resumable upload (as
// opposed to the status query, which carries "bytes */<total>").
func isChunkPut(r *http.Request) bool {
	if r.Method != http.MethodPut {
		return false
	}
	contentRange := r.Header.Get("Content-Range")
	return contentRange != "" && !strings.HasPrefix(contentRange, "bytes */")
}

// interruptedChunkDoer refuses chunk PUTs once a fixed number have been
// accepted, so a test can interrupt an upload at an exact point instead of
// racing a timer. It also counts the chunk PUTs it saw.
type interruptedChunkDoer struct {
	inner     HTTPDoer
	allowance int
	accepted  int
	chunkPuts int
}

func (d *interruptedChunkDoer) Do(request *http.Request) (*http.Response, error) {
	if !isChunkPut(request) {
		return d.inner.Do(request)
	}
	d.chunkPuts++
	if d.accepted >= d.allowance {
		return nil, fmt.Errorf("injected interruption: refusing chunk PUT")
	}
	response, err := d.inner.Do(request)
	if err == nil && (response.StatusCode == driveResumeIncomplete || (response.StatusCode >= 200 && response.StatusCode < 300)) {
		d.accepted++
	}
	return response, err
}

// TestDriveRESTLiveResumeAfterInterruption is the live proof for the resume path:
// an upload is interrupted against the real API after a couple of chunks, and a
// fresh client with the same session directory finishes it without re-sending
// what the server already holds.
//
// The unit tests cover this against the emulator; this one exists because the
// stored session, the server's committed offset and the resumed Content-Range
// are exactly the details an emulator can get wrong.
func TestDriveRESTLiveResumeAfterInterruption(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CRAFTMAKE_DRIVE_LIVE")) == "" {
		t.Skip("set CRAFTMAKE_DRIVE_LIVE=1 to run against the real Drive API")
	}
	originalChunk, originalBackoff := driveChunkSize, driveRetryBackoff
	driveChunkSize = 4 << 20
	// The interruption is injected locally, so there is no quota window to wait
	// out; keep the retry cycles quick.
	driveRetryBackoff = 2 * time.Second
	t.Cleanup(func() { driveChunkSize, driveRetryBackoff = originalChunk, originalBackoff })

	token := liveDriveTokenFetcher(t)
	folder := envOr("CRAFTMAKE_DRIVE_LIVE_FOLDER", "craftmake-rest-live-verify")
	t.Cleanup(func() { liveDriveDelete(t, token, folder) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// 24 MiB at 4 MiB per chunk: six chunks, so an interruption after two leaves
	// four to send.
	const size = 24 << 20
	payload := make([]byte, size)
	for index := range payload {
		payload[index] = byte((index * 7) % 251)
	}
	localPath := filepath.Join(t.TempDir(), "resume.bin")
	if err := os.WriteFile(localPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionDir := t.TempDir()

	// 1. An upload that is cut off after two accepted chunks.
	doer := &interruptedChunkDoer{inner: http.DefaultClient, allowance: 2}
	first := NewDriveRESTClient(token, doer)
	first.SessionDir = sessionDir
	_, err := first.Upload(ctx, folder, "resume.bin", localPath)
	if err == nil {
		t.Fatal("the interrupted upload must report a failure")
	}
	if !strings.Contains(err.Error(), "continues it") {
		t.Fatalf("the failure must say the session is kept for a later run, got: %v", err)
	}
	t.Logf("interrupted upload reported: %v", err)

	// 2. The session must be on disk, with the server's committed offset.
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one saved session, got %v", entries)
	}
	raw, err := os.ReadFile(filepath.Join(sessionDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var session driveUploadSession
	if err := json.Unmarshal(raw, &session); err != nil {
		t.Fatal(err)
	}
	if session.Uploaded <= 0 || session.Uploaded >= size {
		t.Fatalf("saved session offset = %d, want a partial offset", session.Uploaded)
	}
	t.Logf("interrupted after %d of %d bytes; session saved", session.Uploaded, size)

	// 3. A fresh client must finish it, sending only the missing chunks.
	counting := &interruptedChunkDoer{inner: http.DefaultClient, allowance: 1 << 30}
	second := NewDriveRESTClient(token, counting)
	second.SessionDir = sessionDir
	result, err := second.Upload(ctx, folder, "resume.bin", localPath)
	if err != nil {
		t.Fatalf("resumed upload: %v", err)
	}
	if !result.Resumed {
		t.Fatal("the second run must report that it resumed")
	}
	remaining := int((size - session.Uploaded + driveChunkSize - 1) / driveChunkSize)
	if counting.chunkPuts != remaining {
		t.Fatalf("the resumed run sent %d chunks, want exactly the %d missing ones", counting.chunkPuts, remaining)
	}
	t.Logf("resumed run sent %d of the 6 chunks, skipping the %d bytes already stored", counting.chunkPuts, session.Uploaded)

	// 4. The resumed upload must reproduce the bytes exactly.
	var restored bytes.Buffer
	if err := second.Walk(ctx, folder, func(relativePath string, receivedSize int64, content io.Reader) error {
		if relativePath != "resume.bin" {
			return nil
		}
		if receivedSize != int64(size) {
			t.Fatalf("restored size = %d, want %d", receivedSize, size)
		}
		_, err := io.Copy(&restored, content)
		return err
	}); err != nil {
		t.Fatalf("live restore after resume: %v", err)
	}
	if !bytes.Equal(restored.Bytes(), payload) {
		sum, want := md5.Sum(restored.Bytes()), md5.Sum(payload)
		t.Fatalf("resumed content differs: md5 %s want %s", hex.EncodeToString(sum[:]), hex.EncodeToString(want[:]))
	}
	t.Logf("resumed upload restored %d bytes byte for byte", restored.Len())

	// 5. A finished upload must forget its session.
	entries, err = os.ReadDir(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the session file must be cleared after success: %v", entries)
	}
}
