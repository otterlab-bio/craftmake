package colab

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Drive REST API transport.
//
// Mounting Google Drive in a runtime is not possible without the Colab frontend
// (see BuildDriveFSMountSource), but the Drive REST API accepts the same
// Drive-scoped credential, so workspace persistence goes through it instead.

const (
	// defaultDriveAPIBase is the Drive v3 files endpoint.
	defaultDriveAPIBase = "https://www.googleapis.com/drive/v3"
	// defaultDriveUploadBase is the Drive v3 upload endpoint.
	defaultDriveUploadBase = "https://www.googleapis.com/upload/drive/v3"
	// driveFolderMimeType identifies a Drive folder.
	driveFolderMimeType = "application/vnd.google-apps.folder"
	// driveMediaType is the content type of an uploaded workspace file.
	driveMediaType = "application/octet-stream"
	// driveResumeIncomplete is the resumable protocol's "keep going" status.
	// net/http has no constant for 308 because here it is not a redirect.
	driveResumeIncomplete = 308
)

// driveSimpleUploadLimit is the largest file uploaded in one request. Drive's
// multipart upload is only meant for small files, so anything larger goes
// through a resumable session. It is a variable so tests can lower it.
var driveSimpleUploadLimit int64 = 5 << 20

// driveChunkSize is the resumable upload chunk size. Drive requires a multiple
// of 256 KiB. It is a variable so tests can lower it.
var driveChunkSize int64 = 8 << 20

// DriveUploadResult reports what an upload did, so a run can say that a file was
// left alone rather than transferred.
type DriveUploadResult struct {
	ID   string
	Size int64
	// Skipped reports that Drive already held identical content.
	Skipped bool
	// Resumed reports that the transfer continued a previous partial upload.
	Resumed bool
}

// DriveStore persists a workspace tree in Google Drive.
type DriveStore interface {
	// Upload stores the file at localPath as folderPath/name. The content is
	// streamed from disk, a previous partial transfer is resumed when the server
	// still knows it, and a file whose content Drive already has is left alone.
	Upload(ctx context.Context, folderPath, name, localPath string) (DriveUploadResult, error)
	// Walk lists folderPath recursively, calling visit with each file's path
	// relative to folderPath, its size and a reader over its content. The reader
	// is only valid until visit returns, so it must be consumed or dropped.
	Walk(ctx context.Context, folderPath string, visit func(relativePath string, size int64, content io.Reader) error) error
}

// DriveRESTClient is a DriveStore backed by the Drive REST API.
type DriveRESTClient struct {
	APIBase        string
	UploadBase     string
	Client         HTTPDoer
	GetAccessToken func() (string, error)
	// SessionDir persists resumable uploads so a transfer interrupted by a crash
	// or a killed process continues instead of restarting. Empty disables it.
	SessionDir string

	mu        sync.Mutex
	folderIDs map[string]string
}

// NewDriveRESTClient builds a client; the bases are overridable for tests through
// the environment.
func NewDriveRESTClient(getAccessToken func() (string, error), client HTTPDoer) *DriveRESTClient {
	if client == nil {
		client = http.DefaultClient
	}
	apiBase := strings.TrimRight(strings.TrimSpace(envOr("CRAFTMAKE_DRIVE_API_URL", defaultDriveAPIBase)), "/")
	uploadBase := strings.TrimRight(strings.TrimSpace(envOr("CRAFTMAKE_DRIVE_UPLOAD_URL", defaultDriveUploadBase)), "/")
	return &DriveRESTClient{APIBase: apiBase, UploadBase: uploadBase, Client: client, GetAccessToken: getAccessToken, folderIDs: map[string]string{"": "root", ".": "root", "/": "root"}}
}

// driveRequestAttempts and driveRetryBackoff bound the retry of the shared Drive
// client's transient quota rejections. driveRetryBackoff is a variable so tests
// do not sleep.
const driveRequestAttempts = 4

var driveRetryBackoff = 3 * time.Second

// isDriveRateLimit reports whether a response is the shared-project quota
// rejection rather than a genuine permission problem: Drive reports it as 403
// with PERMISSION_DENIED, which is easy to misread.
func isDriveRateLimit(status int, body string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	return strings.Contains(body, "rateLimitExceeded") || strings.Contains(body, "RATE_LIMIT_EXCEEDED") || strings.Contains(body, "Quota exceeded")
}

type driveEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MimeType    string `json:"mimeType"`
	MD5Checksum string `json:"md5Checksum"`
	Size        string `json:"size"`
}

// isFolder reports whether the entry is a folder rather than a stored file.
func (e driveEntry) isFolder() bool { return e.MimeType == driveFolderMimeType }

// sizeBytes is the entry's size, or 0 when Drive did not report one (which is
// the case for the Google-native document types that have no byte content).
func (e driveEntry) sizeBytes() int64 {
	value, err := strconv.ParseInt(e.Size, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// driveEntryFields is the field selection used whenever an entry is looked up:
// the checksum and size are what make the incremental skip possible.
const driveEntryFields = "files(id,name,mimeType,md5Checksum,size)"

// errDriveCredential marks an unusable Drive credential. It is never worth
// retrying: the token endpoint would be hammered for a grant that cannot work
// until the operator logs in again.
var errDriveCredential = errors.New("Drive credential is unavailable")

// send performs one authorized request. The body is produced on demand so the
// caller can resend it, and the response is returned whatever its status, because
// the resumable protocol signals "not finished" with 308.
func (c *DriveRESTClient) send(ctx context.Context, method, endpoint string, newBody func() (io.Reader, error), contentType string, headers map[string]string) (*http.Response, error) {
	if c.GetAccessToken == nil {
		return nil, fmt.Errorf("%w: no access token provider is configured", errDriveCredential)
	}
	token, err := c.GetAccessToken()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDriveCredential, err)
	}
	if token == "" {
		return nil, fmt.Errorf("%w: the Drive access token is empty; run `craftmake colab drive login --session <session>`", errDriveCredential)
	}
	var body io.Reader
	if newBody != nil {
		body, err = newBody()
		if err != nil {
			return nil, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return c.Client.Do(request)
}

// doRetry performs an authorized request, retrying the transient rejections, and
// returns the response once it is a 2xx. The body is produced by newBody for every
// attempt, so nothing large is buffered.
func (c *DriveRESTClient) doRetry(ctx context.Context, method, endpoint string, newBody func() (io.Reader, error), contentType string, headers map[string]string) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= driveRequestAttempts; attempt++ {
		response, err := c.send(ctx, method, endpoint, newBody, contentType, headers)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, errDriveCredential) {
				return nil, err
			}
			lastErr = err
			// A POST that failed on the wire may still have created its file, so
			// repeating it could duplicate the upload.
			if !driveRetrySafe(method, 0, "") {
				return nil, lastErr
			}
		} else if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		} else {
			detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			response.Body.Close()
			body := strings.TrimSpace(string(detail))
			lastErr = driveStatusError(method, response.StatusCode, body, endpoint)
			if !driveRetrySafe(method, response.StatusCode, body) {
				return nil, lastErr
			}
		}
		if attempt == driveRequestAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * driveRetryBackoff):
		}
	}
	return nil, lastErr
}

// driveRetrySafe reports whether a failed attempt may be repeated.
//
// A rate-limit rejection never applied, and the methods the store repeats are
// idempotent, so those are safe. A POST that failed after it was sent is not: it
// may have created its file, and repeating it would duplicate the upload.
func driveRetrySafe(method string, status int, body string) bool {
	if isDriveRateLimit(status, body) {
		return true
	}
	if method == http.MethodPost {
		return false
	}
	return status == 0 || status >= 500
}

// driveStatusError turns a failed status into an actionable error. endpoint may
// carry a resumable session identifier, so callers that know a friendlier
// context pass their own message instead.
func driveStatusError(method string, status int, body, endpoint string) error {
	hint := ""
	switch {
	case status == http.StatusUnauthorized:
		hint = "; the Drive credential is no longer valid, run `craftmake colab drive login --session <session>` again"
	case isDriveRateLimit(status, body):
		// The bundled Drive client is rclone's public one, so its per-project
		// quota is shared with every other rclone user and can be exhausted
		// through no fault of this run.
		hint = "; the shared Drive OAuth client's quota is exhausted, which is transient - retrying, and configuring your own client (CRAFTMAKE_COLAB_DRIVE_CLIENT_ID/SECRET) removes the limit"
	case status == http.StatusForbidden:
		hint = "; the Drive credential lacks access to this file, or the OAuth client is not permitted for it"
	}
	return fmt.Errorf("Drive API %s %s returned %d%s: %s", method, endpoint, status, hint, body)
}

// do performs an authorized request with a buffered body, decoding failures into
// actionable errors. Buffering is fine here because it only carries small
// payloads: metadata and files below driveSimpleUploadLimit.
func (c *DriveRESTClient) do(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Response, error) {
	var payload []byte
	if body != nil {
		buffered, readErr := io.ReadAll(body)
		if readErr != nil {
			return nil, readErr
		}
		payload = buffered
	}
	newBody := func() (io.Reader, error) { return bytes.NewReader(payload), nil }
	return c.doRetry(ctx, method, endpoint, newBody, contentType, nil)
}

// findChild returns the id of name inside parentID, or "" when absent.
func (c *DriveRESTClient) findChild(ctx context.Context, parentID, name string, foldersOnly bool) (string, error) {
	entry, err := c.findChildEntry(ctx, parentID, name, foldersOnly)
	if err != nil {
		return "", err
	}
	return entry.ID, nil
}

// findChildEntry returns the full entry for name inside parentID, including the
// checksum and size the incremental skip needs.
func (c *DriveRESTClient) findChildEntry(ctx context.Context, parentID, name string, foldersOnly bool) (driveEntry, error) {
	query := fmt.Sprintf("name = '%s' and '%s' in parents and trashed = false", escapeDriveQuery(name), parentID)
	if foldersOnly {
		query += fmt.Sprintf(" and mimeType = '%s'", driveFolderMimeType)
	}
	endpoint := c.APIBase + "/files?fields=" + url.QueryEscape(driveEntryFields) + "&pageSize=10&q=" + url.QueryEscape(query)
	response, err := c.do(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return driveEntry{}, err
	}
	defer response.Body.Close()
	var payload struct {
		Files []driveEntry `json:"files"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return driveEntry{}, fmt.Errorf("decode Drive file list: %w", err)
	}
	if len(payload.Files) == 0 {
		return driveEntry{}, nil
	}
	return payload.Files[0], nil
}

// EnsureFolder resolves a folder path relative to My Drive, creating missing
// folders. The empty path (or "/") is the Drive root.
func (c *DriveRESTClient) EnsureFolder(ctx context.Context, folderPath string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(folderPath), "/")
	c.mu.Lock()
	if id, ok := c.folderIDs[trimmed]; ok {
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	parentID := "root"
	current := ""
	if trimmed != "" {
		for _, segment := range strings.Split(trimmed, "/") {
			if segment == "" || segment == "." {
				continue
			}
			if current == "" {
				current = segment
			} else {
				current = current + "/" + segment
			}
			c.mu.Lock()
			cached, ok := c.folderIDs[current]
			c.mu.Unlock()
			if ok {
				parentID = cached
				continue
			}
			id, err := c.findChild(ctx, parentID, segment, true)
			if err != nil {
				return "", err
			}
			if id == "" {
				id, err = c.createFolder(ctx, parentID, segment)
				if err != nil {
					return "", err
				}
			}
			c.mu.Lock()
			c.folderIDs[current] = id
			c.mu.Unlock()
			parentID = id
		}
	}
	c.mu.Lock()
	c.folderIDs[trimmed] = parentID
	c.mu.Unlock()
	return parentID, nil
}

func (c *DriveRESTClient) createFolder(ctx context.Context, parentID, name string) (string, error) {
	payload, err := json.Marshal(map[string]any{"name": name, "mimeType": driveFolderMimeType, "parents": []string{parentID}})
	if err != nil {
		return "", err
	}
	response, err := c.do(ctx, http.MethodPost, c.APIBase+"/files?fields=id", bytes.NewReader(payload), "application/json")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var created driveEntry
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// Upload stores localPath as folderPath/name.
//
// Three things make it usable for real workspaces rather than only small files:
// the content is streamed from disk instead of buffered, a file whose checksum
// Drive already reports is not transferred at all, and anything above
// driveSimpleUploadLimit travels as a resumable chunked session that survives a
// dropped connection - and, when SessionDir is configured, a killed process.
func (c *DriveRESTClient) Upload(ctx context.Context, folderPath, name, localPath string) (DriveUploadResult, error) {
	result := DriveUploadResult{}
	if c.GetAccessToken == nil {
		return result, fmt.Errorf("Drive access token provider is required")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return result, err
	}
	if info.IsDir() {
		return result, fmt.Errorf("cannot upload %s: it is a directory", localPath)
	}
	size := info.Size()
	checksum, err := fileMD5(localPath)
	if err != nil {
		return result, err
	}
	folderID, err := c.EnsureFolder(ctx, folderPath)
	if err != nil {
		return result, err
	}
	existing, err := c.findChildEntry(ctx, folderID, name, false)
	if err != nil {
		return result, err
	}
	// A folder may share the name; only a stored file can be replaced.
	existingID := ""
	if existing.ID != "" && !existing.isFolder() {
		existingID = existing.ID
	}
	if existingID != "" && driveContentMatches(existing, size, checksum) {
		// Drive already holds exactly this content: uploading again would spend
		// time and the shared quota for nothing.
		result.ID, result.Size, result.Skipped = existingID, size, true
		return result, nil
	}

	if size <= driveSimpleUploadLimit {
		id, err := c.uploadSimple(ctx, existingID, folderID, name, localPath)
		if err != nil {
			return result, err
		}
		result.ID, result.Size = id, size
		return result, nil
	}
	id, resumed, err := c.uploadResumable(ctx, existingID, folderID, folderPath, name, localPath, size, checksum)
	if err != nil {
		return result, err
	}
	result.ID, result.Size, result.Resumed = id, size, resumed
	return result, nil
}

// driveContentMatches reports whether the remote entry is the same content as the
// local file. The size is checked when Drive reports one, so a checksum Drive
// computed for a partially synced entry cannot cause a false skip.
func driveContentMatches(entry driveEntry, size int64, checksum string) bool {
	if entry.MD5Checksum == "" || !strings.EqualFold(entry.MD5Checksum, checksum) {
		return false
	}
	if reported := entry.sizeBytes(); entry.Size != "" && reported != size {
		return false
	}
	return true
}

// uploadSimple sends a file in one request: multipart when creating it, a media
// update when replacing one.
func (c *DriveRESTClient) uploadSimple(ctx context.Context, existingID, folderID, name, localPath string) (string, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", err
	}
	if existingID != "" {
		response, err := c.do(ctx, http.MethodPatch, c.UploadBase+"/files/"+url.PathEscape(existingID)+"?uploadType=media&fields=id", bytes.NewReader(data), driveMediaType)
		if err != nil {
			return "", err
		}
		return decodeDriveID(response)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadataHeader := textproto.MIMEHeader{}
	metadataHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metadataPart, err := writer.CreatePart(metadataHeader)
	if err != nil {
		return "", err
	}
	metadata, err := json.Marshal(map[string]any{"name": name, "parents": []string{folderID}})
	if err != nil {
		return "", err
	}
	if _, err := metadataPart.Write(metadata); err != nil {
		return "", err
	}
	fileHeader := textproto.MIMEHeader{}
	fileHeader.Set("Content-Type", driveMediaType)
	filePart, err := writer.CreatePart(fileHeader)
	if err != nil {
		return "", err
	}
	if _, err := filePart.Write(data); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	response, err := c.do(ctx, http.MethodPost, c.UploadBase+"/files?uploadType=multipart&fields=id", &body, writer.FormDataContentType())
	if err != nil {
		return "", err
	}
	return decodeDriveID(response)
}

func decodeDriveID(response *http.Response) (string, error) {
	defer response.Body.Close()
	var created driveEntry
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("decode Drive upload response: %w", err)
	}
	return created.ID, nil
}

// driveUploadSession is a resumable upload remembered on disk, so a transfer that
// a crash or a killed process interrupted continues from where it stopped instead
// of starting over. The URI is a temporary capability for this one upload.
type driveUploadSession struct {
	URI      string `json:"uri"`
	Folder   string `json:"folder"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	MD5      string `json:"md5"`
	Uploaded int64  `json:"uploaded"`
}

// sessionPath keys a session by everything that identifies the transfer, so a
// changed file never resumes into an unrelated upload.
func (c *DriveRESTClient) sessionPath(folderPath, name string, size int64, checksum string) string {
	if strings.TrimSpace(c.SessionDir) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", folderPath, name, size, checksum)))
	return filepath.Join(c.SessionDir, hex.EncodeToString(digest[:16])+".json")
}

func (c *DriveRESTClient) loadSession(folderPath, name string, size int64, checksum string) *driveUploadSession {
	filePath := c.sessionPath(folderPath, name, size, checksum)
	if filePath == "" {
		return nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	var session driveUploadSession
	if err := json.Unmarshal(data, &session); err != nil || session.URI == "" {
		return nil
	}
	return &session
}

func (c *DriveRESTClient) saveSession(session *driveUploadSession) {
	filePath := c.sessionPath(session.Folder, session.Name, session.Size, session.MD5)
	if filePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(session)
	if err != nil {
		return
	}
	// The session URI is a capability for this upload, so keep it owner-only.
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		return
	}
	_ = os.Chmod(filePath, 0o600)
}

func (c *DriveRESTClient) clearSession(session *driveUploadSession) {
	filePath := c.sessionPath(session.Folder, session.Name, session.Size, session.MD5)
	if filePath == "" {
		return
	}
	_ = os.Remove(filePath)
}

// maxResumeCycles bounds how often a failing chunk is resynced before the upload
// is reported as stopped, so a server that keeps failing cannot spin forever.
const maxResumeCycles = 6

// uploadResumable transfers a file in chunks, continuing a previous partial
// upload when the server still knows it.
//
// A failed chunk is always followed by a status query before anything else is
// sent: the server may have kept the chunk whose response was lost, and sending it
// again would append the same bytes twice. The status query is what makes the
// transfer safe to retry, and it is also what lets a later run pick up where this
// one stopped.
func (c *DriveRESTClient) uploadResumable(ctx context.Context, existingID, folderID, folderPath, name, localPath string, size int64, checksum string) (string, bool, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return "", false, err
	}
	defer file.Close()

	resumed := false
	offset := int64(0)
	session := c.loadSession(folderPath, name, size, checksum)
	if session != nil {
		committed, offsetErr := c.sessionOffset(ctx, session.URI, size)
		if offsetErr != nil || committed < 0 {
			// The session expired or became unreachable: start a fresh one.
			session = nil
		} else {
			offset = committed
			resumed = committed > 0
		}
	}
	if session == nil {
		uri, err := c.initiateSession(ctx, existingID, folderID, name, size)
		if err != nil {
			return "", false, err
		}
		session = &driveUploadSession{URI: uri, Folder: folderPath, Name: name, Size: size, MD5: checksum}
		c.saveSession(session)
	}

	failures := 0
	for offset < size {
		length := driveChunkSize
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		response, chunkErr := c.putChunk(ctx, session.URI, file, offset, length, size)
		if chunkErr == nil {
			failures = 0
			switch response.StatusCode {
			case http.StatusOK, http.StatusCreated:
				id, decodeErr := decodeDriveID(response)
				if decodeErr != nil {
					return "", resumed, decodeErr
				}
				c.clearSession(session)
				return id, resumed, nil
			case driveResumeIncomplete:
				next, reported := driveRangeNext(response)
				response.Body.Close()
				if !reported {
					// A 308 acknowledges the chunk but without a Range there is
					// no evidence of how much arrived, so ask instead of assuming.
					if probed, offsetErr := c.sessionOffset(ctx, session.URI, size); offsetErr == nil {
						next, reported = probed, true
					}
				}
				if !reported || next <= offset {
					// No measurable progress: count a cycle and back off, so a
					// server that keeps answering 308 cannot spin this loop.
					failures++
					if failures >= maxResumeCycles {
						return "", resumed, fmt.Errorf("Drive upload of %q made no progress at %d of %d bytes%s", name, offset, size, c.resumeHint())
					}
					select {
					case <-ctx.Done():
						return "", resumed, ctx.Err()
					case <-time.After(time.Duration(failures) * driveRetryBackoff):
					}
					continue
				}
				offset = next
				session.Uploaded = offset
				c.saveSession(session)
				continue
			default:
				detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
				response.Body.Close()
				chunkErr = fmt.Errorf("Drive upload of %q hit an unexpected status %d: %s", name, response.StatusCode, strings.TrimSpace(string(detail)))
			}
		}

		failures++
		if committed, offsetErr := c.sessionOffset(ctx, session.URI, size); offsetErr == nil {
			if committed >= size {
				// Every byte arrived and only a response was lost, so the upload
				// is really finished: resolve the file it produced.
				if entry, lookupErr := c.findChildEntry(ctx, folderID, name, false); lookupErr == nil && entry.ID != "" {
					c.clearSession(session)
					return entry.ID, true, nil
				}
			}
			offset = committed
			if committed > 0 {
				resumed = true
			}
			session.Uploaded = offset
			c.saveSession(session)
		}
		if failures >= maxResumeCycles {
			return "", resumed, fmt.Errorf("Drive upload of %q stopped at %d of %d bytes%s: %w", name, offset, size, c.resumeHint(), chunkErr)
		}
		select {
		case <-ctx.Done():
			return "", resumed, ctx.Err()
		case <-time.After(time.Duration(failures) * driveRetryBackoff):
		}
	}
	// A 308 can report the final byte without a completion response, so resolve
	// the file rather than reporting a bogus failure.
	if entry, lookupErr := c.findChildEntry(ctx, folderID, name, false); lookupErr == nil && entry.ID != "" {
		c.clearSession(session)
		return entry.ID, resumed, nil
	}
	return "", resumed, fmt.Errorf("Drive upload of %q ended without a file id", name)
}

// resumeHint explains what happens to a stopped transfer, depending on whether the
// session was persisted.
func (c *DriveRESTClient) resumeHint() string {
	if strings.TrimSpace(c.SessionDir) == "" {
		return ""
	}
	return "; the resumable session was saved, so a later run continues it"
}

// initiateSession opens a resumable session and returns its URI.
func (c *DriveRESTClient) initiateSession(ctx context.Context, existingID, folderID, name string, size int64) (string, error) {
	metadata := map[string]any{"name": name}
	method := http.MethodPost
	endpoint := c.UploadBase + "/files?uploadType=resumable&fields=id"
	if existingID != "" {
		// Replacing content: the file already has its parents.
		method = http.MethodPatch
		endpoint = c.UploadBase + "/files/" + url.PathEscape(existingID) + "?uploadType=resumable&fields=id"
	} else {
		metadata["parents"] = []string{folderID}
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	headers := map[string]string{
		"X-Upload-Content-Length": strconv.FormatInt(size, 10),
		"X-Upload-Content-Type":   driveMediaType,
	}
	response, err := c.doRetry(ctx, method, endpoint, func() (io.Reader, error) { return bytes.NewReader(payload), nil }, "application/json", headers)
	if err != nil {
		return "", fmt.Errorf("Drive could not start a resumable upload for %q: %w", name, err)
	}
	defer response.Body.Close()
	location := response.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("Drive returned no resumable session for %q", name)
	}
	return location, nil
}

// putChunk sends one chunk. A 308 ("keep going") is returned to the caller as a
// protocol outcome rather than an error, and anything else non-2xx becomes an
// error whose message never carries the session URI, which is a capability for
// this upload - a transport error would otherwise quote the request URL.
//
// It deliberately does not retry: the caller resyncs with the server after a
// failure, because a chunk whose response was lost may already have been kept.
func (c *DriveRESTClient) putChunk(ctx context.Context, sessionURI string, file *os.File, offset, length, total int64) (*http.Response, error) {
	headers := map[string]string{
		"Content-Range": fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, total),
	}
	// The body is produced on demand, so the bytes come from the file rather than
	// from memory.
	newBody := func() (io.Reader, error) { return io.NewSectionReader(file, offset, length), nil }
	response, err := c.send(ctx, http.MethodPut, sessionURI, newBody, driveMediaType, headers)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, redactSession(fmt.Errorf("Drive chunk at offset %d of %d failed: %w", offset, total, err), sessionURI)
	}
	if response.StatusCode == driveResumeIncomplete || (response.StatusCode >= 200 && response.StatusCode < 300) {
		return response, nil
	}
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	response.Body.Close()
	return nil, fmt.Errorf("Drive rejected the chunk at offset %d of %d (status %d): %s", offset, total, response.StatusCode, strings.TrimSpace(string(detail)))
}

// redactSession keeps the resumable session URI out of an error message.
func redactSession(err error, sessionURI string) error {
	if err == nil || sessionURI == "" {
		return err
	}
	message := err.Error()
	if !strings.Contains(message, sessionURI) {
		return err
	}
	return errors.New(strings.ReplaceAll(message, sessionURI, "<resumable-session>"))
}

// sessionOffset asks the server how much of the upload it has committed. It
// reports -1 when the session is gone, which means the upload must start over.
func (c *DriveRESTClient) sessionOffset(ctx context.Context, sessionURI string, total int64) (int64, error) {
	headers := map[string]string{"Content-Range": fmt.Sprintf("bytes */%d", total)}
	response, err := c.send(ctx, http.MethodPut, sessionURI, func() (io.Reader, error) { return http.NoBody, nil }, "", headers)
	if err != nil {
		return -1, redactSession(err, sessionURI)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK, http.StatusCreated:
		// The server already has the whole file.
		return total, nil
	case driveResumeIncomplete:
		if next, ok := driveRangeNext(response); ok {
			return next, nil
		}
		return 0, nil
	case http.StatusNotFound, http.StatusGone:
		return -1, nil
	default:
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return -1, fmt.Errorf("Drive did not report the resumable upload's progress (status %d): %s", response.StatusCode, strings.TrimSpace(string(detail)))
	}
}

// driveRangeNext reads the next byte offset to send from the Range header of a
// 308 response: "bytes=0-123" means 0..123 arrived, so 124 comes next.
func driveRangeNext(response *http.Response) (int64, bool) {
	value := response.Header.Get("Range")
	index := strings.LastIndex(value, "-")
	if index < 0 {
		return 0, false
	}
	committed, err := strconv.ParseInt(strings.TrimSpace(value[index+1:]), 10, 64)
	if err != nil {
		return 0, false
	}
	return committed + 1, true
}

// Walk lists folderPath recursively and visits every file with a reader over its
// content, so a large file is written straight to disk rather than buffered.
func (c *DriveRESTClient) Walk(ctx context.Context, folderPath string, visit func(relativePath string, size int64, content io.Reader) error) error {
	folderID, err := c.EnsureFolder(ctx, folderPath)
	if err != nil {
		return err
	}
	return c.walk(ctx, folderID, "", visit, 0)
}

func (c *DriveRESTClient) walk(ctx context.Context, folderID, prefix string, visit func(string, int64, io.Reader) error, depth int) error {
	if depth > 32 {
		return fmt.Errorf("Drive folder nesting is deeper than 32 levels at %q", prefix)
	}
	query := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
	endpoint := c.APIBase + "/files?fields=" + url.QueryEscape(driveEntryFields) + "&pageSize=1000&q=" + url.QueryEscape(query)
	response, err := c.do(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return err
	}
	var payload struct {
		Files []driveEntry `json:"files"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&payload)
	response.Body.Close()
	if decodeErr != nil {
		return fmt.Errorf("decode Drive file list: %w", decodeErr)
	}
	for _, entry := range payload.Files {
		relative := entry.Name
		if prefix != "" {
			relative = path.Join(prefix, entry.Name)
		}
		if entry.isFolder() {
			if err := c.walk(ctx, entry.ID, relative, visit, depth+1); err != nil {
				return err
			}
			continue
		}
		download, err := c.do(ctx, http.MethodGet, c.APIBase+"/files/"+url.PathEscape(entry.ID)+"?alt=media", nil, "")
		if err != nil {
			return err
		}
		visitErr := visit(relative, entry.sizeBytes(), download.Body)
		download.Body.Close()
		if visitErr != nil {
			return visitErr
		}
	}
	return nil
}

// fileMD5 streams the file through MD5, so a large file is never held in memory.
func fileMD5(localPath string) (string, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// escapeDriveQuery escapes a single-quoted Drive query literal.
func escapeDriveQuery(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `'`, `\'`)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
