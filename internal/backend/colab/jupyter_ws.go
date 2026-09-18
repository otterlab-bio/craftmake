package colab

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// jupyterExecuteReplyTimeout bounds how long the executor waits for an
// execute_reply after sending each cell.
const jupyterExecuteReplyTimeout = 300 * time.Second

// jupyterMsg is a single Jupyter kernel-protocol message envelope.
type jupyterMsg struct {
	Header       jupyterHeader   `json:"header"`
	ParentHeader json.RawMessage `json:"parent_header"`
	Metadata     map[string]any  `json:"metadata"`
	Content      any             `json:"content"`
	Bufers       []any           `json:"buffers,omitempty"`
	Channel      string          `json:"channel"`
	Signature    string          `json:"signature,omitempty"`
}

type jupyterHeader struct {
	MsgID    string `json:"msg_id"`
	Session  string `json:"session"`
	Username string `json:"username"`
	Date     string `json:"date"`
	MsgType  string `json:"msg_type"`
	Version  string `json:"version"`
}

func newJupyterHeader(session, msgType string) jupyterHeader {
	return jupyterHeader{MsgID: uuid.NewString(), Session: session, Username: "craftmake", Date: time.Now().UTC().Format(time.RFC3339Nano), MsgType: msgType, Version: "5.3"}
}

// jupyterSign computes the Jupyter HMAC-SHA256 signature over the JSON-encoded
// content, hex-encoded. It returns empty when there is no key (unsigned).
func jupyterSign(key string, content any) string {
	if key == "" {
		return ""
	}
	data, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// jupyterVerify checks an inbound message signature against the shared key. When
// no key is configured signing is considered disabled and returns true.
func jupyterVerify(key string, msg jupyterMsg) bool {
	if key == "" {
		return true
	}
	want := jupyterSign(key, msg.Content)
	if want == "" {
		return false
	}
	return hmac.Equal([]byte(want), []byte(msg.Signature))
}

// JupyterWebSocketExecutor executes a notebook's code cells over the Jupyter
// kernel WebSocket channels endpoint. Runtime.ID is the ws(s) channels URL. It
// accumulates stream/display_data/error output and returns it so the existing
// TaskResult decoder can find the sentinel. HMACKey, when set, is used to sign
// outbound messages and verify inbound ones.
type JupyterWebSocketExecutor struct {
	SessionID          string
	Client             *http.Client
	HMACKey            string
	ColabClient        *ColabServerClient
	Endpoint           string
	AuthConsentHandler func(ctx context.Context, authType, redirectURI string) error

	mu     sync.Mutex
	active *minimalWSConn
}

// Interrupt requests the in-flight execution stop by signaling the active
// WebSocket connection. It is a no-op when no execution is active, so it is
// idempotent and safe to call from a cancel path.
func (e *JupyterWebSocketExecutor) Interrupt(_ context.Context, _ Runtime) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil {
		return nil
	}
	return e.active.Interrupt()
}

func (e *JupyterWebSocketExecutor) ExecuteNotebook(ctx context.Context, runtime Runtime, notebook []byte) (string, error) {
	var nb Notebook
	if err := json.Unmarshal(notebook, &nb); err != nil {
		return "", fmt.Errorf("decode notebook: %w", err)
	}
	session := e.SessionID
	if session == "" {
		session = "craftmake"
	}
	if runtime.ID != "" {
		e.Endpoint = runtime.ID
	}

	targetWS := runtime.ID
	headers := map[string]string{}
	if runtime.ProxyToken != "" {
		headers[HeaderProxyToken] = runtime.ProxyToken
		headers[HeaderClientAgent] = "vscode"
	}

	if runtime.ProxyURL != "" && (strings.HasPrefix(runtime.ProxyURL, "http://") || strings.HasPrefix(runtime.ProxyURL, "https://")) {
		kernelID, err := e.resolveKernel(ctx, runtime.ProxyURL, runtime.ProxyToken)
		if err != nil {
			return "", &RemoteError{Kind: ErrorKernelDisconnected, Operation: "resolve kernel", Err: err}
		}
		targetWS = formatChannelsWSURL(runtime.ProxyURL, kernelID, session)
	}

	if targetWS == "" {
		return "", fmt.Errorf("Jupyter channels WebSocket URL is required")
	}

	conn, err := DialWebSocketWithHeaders(ctx, targetWS, headers, e.Client)
	if err != nil {
		return "", classifyDialError(err)
	}
	e.mu.Lock()
	e.active = conn
	e.mu.Unlock()
	defer func() {
		_ = conn.Close()
		e.mu.Lock()
		e.active = nil
		e.mu.Unlock()
	}()
	var output strings.Builder
	for _, cell := range nb.Cells {
		if cell.CellType != "code" || strings.TrimSpace(cell.Source) == "" {
			continue
		}
		msgID, err := e.sendExecuteRequest(conn, session, cell.Source)
		if err != nil {
			return "", &RemoteError{Kind: ErrorKernelDisconnected, Operation: "send execute_request", Err: err}
		}
		cellOutput, err := e.drainUntilReply(ctx, conn, session, msgID)
		if err != nil {
			return "", err
		}
		output.WriteString(cellOutput)
	}
	return output.String(), nil
}

func (e *JupyterWebSocketExecutor) sendExecuteRequest(conn *minimalWSConn, session, code string) (string, error) {
	msgID := uuid.NewString()
	header := jupyterHeader{MsgID: msgID, Session: session, Username: "craftmake", Date: time.Now().UTC().Format(time.RFC3339Nano), MsgType: "execute_request", Version: "5.3"}
	content := map[string]any{"code": code, "silent": false, "store_history": true, "user_expressions": map[string]any{}, "allow_stdin": true, "stop_on_error": false}
	msg := jupyterMsg{Header: header, ParentHeader: json.RawMessage("{}"), Metadata: map[string]any{}, Content: content, Channel: "shell"}
	msg.Signature = jupyterSign(e.HMACKey, content)
	data, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	if err := conn.WriteText(data); err != nil {
		return "", err
	}
	return msgID, nil
}

// drainUntilReply reads iopub/shell messages until the execute_reply for the
// given cell. It accumulates stream/display_data/error output and terminates on
// execute_reply. Inbound signatures are verified when HMACKey is configured.
func (e *JupyterWebSocketExecutor) drainUntilReply(ctx context.Context, conn *minimalWSConn, session, msgID string) (string, error) {
	var output strings.Builder
	for {
		select {
		case <-ctx.Done():
			return output.String(), ctx.Err()
		default:
		}
		raw, err := conn.ReadText()
		if err != nil {
			return output.String(), &RemoteError{Kind: ErrorKernelDisconnected, Operation: "read kernel message", Err: err}
		}
		var msg jupyterMsg
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			continue
		}
		if !jupyterVerify(e.HMACKey, msg) {
			continue
		}
		switch msg.Header.MsgType {
		case "stream":
			if content, ok := msg.Content.(map[string]any); ok {
				writeContentText(&output, content, "text")
			}
			if content, ok := msg.Content.(map[string]any); ok {
				if data, ok := content["data"].(map[string]any); ok {
					writeContentText(&output, data, "text/plain")
				}
			}
		case "error":
			if content, ok := msg.Content.(map[string]any); ok {
				writeErrorTraceback(&output, content)
			}
		case "colab_request":
			e.handleColabRequest(ctx, conn, session, msg)
		case "execute_reply":
			return output.String(), nil
		}
	}
}

func (e *JupyterWebSocketExecutor) handleColabRequest(ctx context.Context, conn *minimalWSConn, session string, msg jupyterMsg) {
	var colabMsgID any
	if meta := msg.Metadata; meta != nil {
		colabMsgID = meta["colab_msg_id"]
	}
	var authType string
	if content, ok := msg.Content.(map[string]any); ok {
		if req, ok := content["request"].(map[string]any); ok {
			if at, ok := req["authType"].(string); ok {
				authType = at
			}
		}
	}
	var propErr error
	if e.ColabClient != nil && e.Endpoint != "" {
		dryRes, err := e.ColabClient.PropagateCredentials(ctx, e.Endpoint, authType, true)
		if err == nil && dryRes.Success {
			_, propErr = e.ColabClient.PropagateCredentials(ctx, e.Endpoint, authType, false)
		} else if err == nil && dryRes.UnauthorizedRedirectURI != "" {
			if e.AuthConsentHandler != nil {
				if consentErr := e.AuthConsentHandler(ctx, authType, dryRes.UnauthorizedRedirectURI); consentErr == nil {
					_, propErr = e.ColabClient.PropagateCredentials(ctx, e.Endpoint, authType, false)
				} else {
					propErr = consentErr
				}
			} else {
				propErr = fmt.Errorf("authorization consent required for %s: %s", authType, dryRes.UnauthorizedRedirectURI)
			}
		} else {
			propErr = err
		}
	}
	val := map[string]any{
		"type":         "colab_reply",
		"colab_msg_id": colabMsgID,
	}
	if propErr != nil {
		val["error"] = propErr.Error()
	}
	reply := jupyterMsg{
		Header:       newJupyterHeader(session, "input_reply"),
		ParentHeader: json.RawMessage("{}"),
		Metadata:     map[string]any{},
		Content: map[string]any{
			"value": val,
		},
		Channel: "stdin",
	}
	reply.Signature = jupyterSign(e.HMACKey, reply.Content)
	data, _ := json.Marshal(reply)
	_ = conn.WriteText(data)
}

func writeContentText(output *strings.Builder, content map[string]any, key string) {
	if text, ok := content[key].(string); ok {
		output.WriteString(text)
	}
}

func writeErrorTraceback(output *strings.Builder, content map[string]any) {
	if trace, ok := content["traceback"].([]any); ok {
		for _, line := range trace {
			if s, ok := line.(string); ok {
				output.WriteString(s)
				output.WriteString("\n")
			}
		}
	}
}

func (e *JupyterWebSocketExecutor) resolveKernel(ctx context.Context, proxyURL, proxyToken string) (string, error) {
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	reqURL := strings.TrimRight(proxyURL, "/") + "/api/kernels"

	// 1. Try GET <proxyURL>/api/kernels
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err == nil {
		req.Header.Set(HeaderProxyToken, proxyToken)
		req.Header.Set(HeaderClientAgent, "vscode")
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var kernels []struct {
					ID string `json:"id"`
				}
				if jsonErr := json.NewDecoder(resp.Body).Decode(&kernels); jsonErr == nil && len(kernels) > 0 && kernels[0].ID != "" {
					return kernels[0].ID, nil
				}
			}
		}
	}

	// 2. Try POST <proxyURL>/api/sessions
	sessURL := strings.TrimRight(proxyURL, "/") + "/api/sessions"
	sessPayload := map[string]any{
		"name":   "craftmake",
		"path":   "/craftmake",
		"type":   "notebook",
		"kernel": map[string]string{"name": "python3"},
	}
	data, _ := json.Marshal(sessPayload)
	sessReq, err := http.NewRequestWithContext(ctx, http.MethodPost, sessURL, bytes.NewReader(data))
	if err == nil {
		sessReq.Header.Set(HeaderProxyToken, proxyToken)
		sessReq.Header.Set(HeaderClientAgent, "vscode")
		sessReq.Header.Set("Content-Type", "application/json")
		sessReq.Header.Set("Accept", "application/json")
		sessResp, err := client.Do(sessReq)
		if err == nil {
			defer sessResp.Body.Close()
			if sessResp.StatusCode == http.StatusOK || sessResp.StatusCode == http.StatusCreated {
				var sessionInfo struct {
					Kernel struct {
						ID string `json:"id"`
					} `json:"kernel"`
				}
				if jsonErr := json.NewDecoder(sessResp.Body).Decode(&sessionInfo); jsonErr == nil && sessionInfo.Kernel.ID != "" {
					return sessionInfo.Kernel.ID, nil
				}
			}
		}
	}

	// 3. Fallback: try POST <proxyURL>/api/kernels
	kernReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader([]byte(`{"name":"python3"}`)))
	if err != nil {
		return "", err
	}
	kernReq.Header.Set(HeaderProxyToken, proxyToken)
	kernReq.Header.Set(HeaderClientAgent, "vscode")
	kernReq.Header.Set("Content-Type", "application/json")
	kernReq.Header.Set("Accept", "application/json")

	kernResp, err := client.Do(kernReq)
	if err != nil {
		return "", fmt.Errorf("create kernel on proxy %q: %w", proxyURL, err)
	}
	defer kernResp.Body.Close()
	if kernResp.StatusCode < 200 || kernResp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(kernResp.Body, 512))
		return "", fmt.Errorf("create kernel HTTP %d: %s", kernResp.StatusCode, strings.TrimSpace(string(b)))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(kernResp.Body).Decode(&created); err != nil || created.ID == "" {
		return "", fmt.Errorf("decode created kernel response: %w", err)
	}
	return created.ID, nil
}

func formatChannelsWSURL(proxyURL, kernelID, sessionID string) string {
	wsURL := strings.TrimRight(proxyURL, "/")
	if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
	} else if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
	}
	return fmt.Sprintf("%s/api/kernels/%s/channels?session_id=%s", wsURL, url.PathEscape(kernelID), url.QueryEscape(sessionID))
}

// classifyDialError maps a handshake failure to a classified RemoteError. An
// HTTP 401/403 during the upgrade indicates expired/unauthorized credentials.
func classifyDialError(err error) error {
	if remote, ok := err.(*RemoteError); ok {
		return remote
	}
	return &RemoteError{Kind: ErrorKernelDisconnected, Operation: "connect Jupyter kernel", Err: err}
}

var _ NotebookExecutor = (*JupyterWebSocketExecutor)(nil)
var _ NotebookInterruptor = (*JupyterWebSocketExecutor)(nil)
