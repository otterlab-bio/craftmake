package colab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
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
)

// DriveStore persists a workspace tree in Google Drive.
type DriveStore interface {
	// Upload writes data to folderPath/name, replacing an existing file.
	Upload(ctx context.Context, folderPath, name string, data []byte) error
	// Walk lists folderPath recursively, calling visit with each file's path
	// relative to folderPath and its content.
	Walk(ctx context.Context, folderPath string, visit func(relativePath string, data []byte) error) error
}

// DriveRESTClient is a DriveStore backed by the Drive REST API.
type DriveRESTClient struct {
	APIBase        string
	UploadBase     string
	Client         HTTPDoer
	GetAccessToken func() (string, error)

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
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
}

// do performs an authorized request, decoding failures into actionable errors.
func (c *DriveRESTClient) do(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Response, error) {
	if c.GetAccessToken == nil {
		return nil, fmt.Errorf("Drive access token provider is required")
	}
	// Buffer the payload so a retry can resend it.
	var payload []byte
	if body != nil {
		buffered, readErr := io.ReadAll(body)
		if readErr != nil {
			return nil, readErr
		}
		payload = buffered
	}
	token, err := c.GetAccessToken()
	if err != nil {
		return nil, fmt.Errorf("get Drive access token: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("Drive access token is empty; run `craftmake colab drive login --session <session>`")
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	var response *http.Response
	var lastErr error
	for attempt := 1; attempt <= driveRequestAttempts; attempt++ {
		response, err = c.Client.Do(request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		body := strings.TrimSpace(string(detail))
		hint := ""
		switch {
		case response.StatusCode == http.StatusUnauthorized:
			hint = "; the Drive credential is no longer valid, run `craftmake colab drive login --session <session>` again"
		case isDriveRateLimit(response.StatusCode, body):
			// The bundled Drive client is rclone's public one, so its per-project
			// quota is shared with every other rclone user and can be exhausted
			// through no fault of this run.
			hint = "; the shared Drive OAuth client's quota is exhausted, which is transient - retrying, and configuring your own client (CRAFTMAKE_COLAB_DRIVE_CLIENT_ID/SECRET) removes the limit"
		case response.StatusCode == http.StatusForbidden:
			hint = "; the Drive credential lacks access to this file, or the OAuth client is not permitted for it"
		}
		lastErr = fmt.Errorf("Drive API %s %s returned %d%s: %s", method, endpoint, response.StatusCode, hint, body)
		if !isDriveRateLimit(response.StatusCode, body) || attempt == driveRequestAttempts {
			return nil, lastErr
		}
		request.Body = io.NopCloser(bytes.NewReader(payload))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * driveRetryBackoff):
		}
	}
	return nil, lastErr
}

// findChild returns the id of name inside parentID, or "" when absent.
func (c *DriveRESTClient) findChild(ctx context.Context, parentID, name string, foldersOnly bool) (string, error) {
	query := fmt.Sprintf("name = '%s' and '%s' in parents and trashed = false", escapeDriveQuery(name), parentID)
	if foldersOnly {
		query += fmt.Sprintf(" and mimeType = '%s'", driveFolderMimeType)
	}
	endpoint := c.APIBase + "/files?fields=files(id,name,mimeType)&pageSize=10&q=" + url.QueryEscape(query)
	response, err := c.do(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var payload struct {
		Files []driveEntry `json:"files"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode Drive file list: %w", err)
	}
	if len(payload.Files) == 0 {
		return "", nil
	}
	return payload.Files[0].ID, nil
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

// Upload writes data to folderPath/name, creating the file or replacing the
// existing content when a file of that name is already there.
func (c *DriveRESTClient) Upload(ctx context.Context, folderPath, name string, data []byte) error {
	folderID, err := c.EnsureFolder(ctx, folderPath)
	if err != nil {
		return err
	}
	existingID, err := c.findChild(ctx, folderID, name, false)
	if err != nil {
		return err
	}
	if existingID != "" {
		response, err := c.do(ctx, http.MethodPatch, c.UploadBase+"/files/"+url.PathEscape(existingID)+"?uploadType=media", bytes.NewReader(data), "application/octet-stream")
		if err != nil {
			return err
		}
		return response.Body.Close()
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadataHeader := textproto.MIMEHeader{}
	metadataHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metadataPart, err := writer.CreatePart(metadataHeader)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(map[string]any{"name": name, "parents": []string{folderID}})
	if err != nil {
		return err
	}
	if _, err := metadataPart.Write(metadata); err != nil {
		return err
	}
	fileHeader := textproto.MIMEHeader{}
	fileHeader.Set("Content-Type", "application/octet-stream")
	filePart, err := writer.CreatePart(fileHeader)
	if err != nil {
		return err
	}
	if _, err := filePart.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	response, err := c.do(ctx, http.MethodPost, c.UploadBase+"/files?uploadType=multipart&fields=id", &body, writer.FormDataContentType())
	if err != nil {
		return err
	}
	return response.Body.Close()
}

// Walk lists folderPath recursively and visits every file with its content.
func (c *DriveRESTClient) Walk(ctx context.Context, folderPath string, visit func(relativePath string, data []byte) error) error {
	folderID, err := c.EnsureFolder(ctx, folderPath)
	if err != nil {
		return err
	}
	return c.walk(ctx, folderID, "", visit, 0)
}

func (c *DriveRESTClient) walk(ctx context.Context, folderID, prefix string, visit func(string, []byte) error, depth int) error {
	if depth > 32 {
		return fmt.Errorf("Drive folder nesting is deeper than 32 levels at %q", prefix)
	}
	query := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
	endpoint := c.APIBase + "/files?fields=files(id,name,mimeType)&pageSize=1000&q=" + url.QueryEscape(query)
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
		if entry.MimeType == driveFolderMimeType {
			if err := c.walk(ctx, entry.ID, relative, visit, depth+1); err != nil {
				return err
			}
			continue
		}
		data, err := c.download(ctx, entry.ID)
		if err != nil {
			return err
		}
		if err := visit(relative, data); err != nil {
			return err
		}
	}
	return nil
}

func (c *DriveRESTClient) download(ctx context.Context, fileID string) ([]byte, error) {
	response, err := c.do(ctx, http.MethodGet, c.APIBase+"/files/"+url.PathEscape(fileID)+"?alt=media", nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(response.Body)
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
