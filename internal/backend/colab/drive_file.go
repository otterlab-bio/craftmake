package colab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type DriveFileClient struct {
	BaseURL     string
	Client      HTTPDoer
	BearerToken string
	// GetAccessToken, when set, supplies a fresh OAuth access token for each
	// request; BearerToken is used as a static fallback.
	GetAccessToken func() (string, error)
	ReadPath       string
	WritePath      string
}

func NewDriveFileClient(baseURL, bearerToken string, client HTTPDoer) *DriveFileClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &DriveFileClient{BaseURL: strings.TrimRight(baseURL, "/"), Client: client, BearerToken: bearerToken, ReadPath: "/drive/read", WritePath: "/drive/write"}
}

// authorization returns the bearer token for a request, preferring a freshly
// fetched access token when a provider is configured.
func (c *DriveFileClient) authorization() string {
	if c.GetAccessToken != nil {
		if token, err := c.GetAccessToken(); err == nil && token != "" {
			return token
		}
	}
	return c.BearerToken
}

// Get downloads the file at remotePath and returns its bytes.
func (c *DriveFileClient) Get(ctx context.Context, remotePath string) ([]byte, error) {
	if c.BaseURL == "" {
		return nil, fmt.Errorf("Drive file client base URL is required")
	}
	joined, err := url.JoinPath(c.BaseURL, c.ReadPath)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: remotePath})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joined, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := c.authorization(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.Client.Do(req)
	if err != nil {
		return nil, &RemoteError{Kind: ErrorKernelDisconnected, Operation: "read Drive file", Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, &RemoteError{Kind: ClassifyHTTPStatus(response.StatusCode), Operation: "read Drive file", StatusCode: response.StatusCode, Err: fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))}
	}
	return io.ReadAll(response.Body)
}

// Put uploads the provided bytes to remotePath, creating parent directories as needed.
func (c *DriveFileClient) Put(ctx context.Context, remotePath string, content []byte) error {
	if c.BaseURL == "" {
		return fmt.Errorf("Drive file client base URL is required")
	}
	joined, err := url.JoinPath(c.BaseURL, c.WritePath)
	if err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Path    string `json:"path"`
		Content []byte `json:"content"`
	}{Path: remotePath, Content: content})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joined, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := c.authorization(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.Client.Do(req)
	if err != nil {
		return &RemoteError{Kind: ErrorKernelDisconnected, Operation: "write Drive file", Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &RemoteError{Kind: ClassifyHTTPStatus(response.StatusCode), Operation: "write Drive file", StatusCode: response.StatusCode, Err: fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))}
	}
	return nil
}
