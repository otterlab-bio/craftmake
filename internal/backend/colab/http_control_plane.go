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

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type HTTPControlPlane struct {
	BaseURL     string
	Client      HTTPDoer
	BearerToken string
	AcquirePath string
	ReleasePath string
}

func NewHTTPControlPlane(baseURL, bearerToken string, client HTTPDoer) *HTTPControlPlane {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPControlPlane{BaseURL: strings.TrimRight(baseURL, "/"), Client: client, BearerToken: bearerToken, AcquirePath: "/runtime/assign", ReleasePath: "/runtime/unassign"}
}

func (c *HTTPControlPlane) AcquireRuntime(ctx context.Context, request RuntimeRequest) (Runtime, error) {
	var response struct {
		ID        string `json:"id"`
		RuntimeID string `json:"runtime_id"`
	}
	if err := c.postJSON(ctx, c.AcquirePath, request, &response); err != nil {
		return Runtime{}, err
	}
	id := response.ID
	if id == "" {
		id = response.RuntimeID
	}
	if id == "" {
		return Runtime{}, &RemoteError{Kind: ErrorProtocolMismatch, Operation: "acquire runtime", Err: fmt.Errorf("response did not contain runtime id")}
	}
	return Runtime{ID: id}, nil
}

func (c *HTTPControlPlane) ReleaseRuntime(ctx context.Context, runtime Runtime) error {
	return c.postJSON(ctx, c.ReleasePath, map[string]string{"runtime_id": runtime.ID}, nil)
}

func (c *HTTPControlPlane) postJSON(ctx context.Context, path string, payload any, result any) error {
	if c.BaseURL == "" {
		return fmt.Errorf("Colab control-plane base URL is required")
	}
	joined, err := url.JoinPath(c.BaseURL, path)
	if err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joined, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}
	response, err := c.Client.Do(req)
	if err != nil {
		return &RemoteError{Kind: ErrorKernelDisconnected, Operation: "control-plane request", Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &RemoteError{Kind: ClassifyHTTPStatus(response.StatusCode), Operation: "control-plane request", StatusCode: response.StatusCode, Err: fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))}
	}
	if result == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		return &RemoteError{Kind: ErrorProtocolMismatch, Operation: "decode control-plane response", Err: err}
	}
	return nil
}
