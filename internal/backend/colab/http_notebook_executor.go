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

type HTTPNotebookExecutor struct {
	BaseURL     string
	Client      HTTPDoer
	BearerToken string
	ExecutePath string
}

func NewHTTPNotebookExecutor(baseURL, bearerToken string, client HTTPDoer) *HTTPNotebookExecutor {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPNotebookExecutor{BaseURL: strings.TrimRight(baseURL, "/"), Client: client, BearerToken: bearerToken, ExecutePath: "/kernel/execute"}
}

func (e *HTTPNotebookExecutor) ExecuteNotebook(ctx context.Context, runtime Runtime, notebook []byte) (string, error) {
	if e.BaseURL == "" {
		return "", fmt.Errorf("Colab notebook executor base URL is required")
	}
	joined, err := url.JoinPath(e.BaseURL, e.ExecutePath)
	if err != nil {
		return "", err
	}
	payload := struct {
		RuntimeID string          `json:"runtime_id"`
		Notebook  json.RawMessage `json:"notebook"`
	}{RuntimeID: runtime.ID, Notebook: notebook}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joined, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.BearerToken)
	}
	response, err := e.Client.Do(req)
	if err != nil {
		return "", &RemoteError{Kind: ErrorKernelDisconnected, Operation: "execute notebook", Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", &RemoteError{Kind: ClassifyHTTPStatus(response.StatusCode), Operation: "execute notebook", StatusCode: response.StatusCode, Err: fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))}
	}
	output, err := io.ReadAll(response.Body)
	if err != nil {
		return "", &RemoteError{Kind: ErrorTransferFailed, Operation: "read notebook output", Err: err}
	}
	return string(output), nil
}
