package colab

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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
	cells := make([]string, 0, len(nb.Cells))
	for _, cell := range nb.Cells {
		if cell.CellType != "code" || strings.TrimSpace(cell.Source) == "" {
			continue
		}
		cells = append(cells, cell.Source)
	}
	return e.executeCells(ctx, runtime, cells)
}

// executeCells dials the runtime kernel, runs each cell in order and returns the
// accumulated output. It is the single place that talks to the kernel, shared
// by notebook execution and workspace sync.
func (e *JupyterWebSocketExecutor) executeCells(ctx context.Context, runtime Runtime, cells []string) (string, error) {
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
		// A freshly assigned runtime may hand out a kernel that is still
		// starting; connecting to its channels then hangs until the read
		// deadline. Wait for the kernel to report idle first (best effort).
		e.waitForKernelReady(ctx, runtime.ProxyURL, runtime.ProxyToken, kernelID)
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
	for _, cell := range cells {
		if strings.TrimSpace(cell) == "" {
			continue
		}
		msgID, err := e.sendExecuteRequest(conn, session, cell)
		if err != nil {
			return output.String(), &RemoteError{Kind: ErrorKernelDisconnected, Operation: "send execute_request", Err: err}
		}
		cellOutput, err := e.drainUntilReply(ctx, conn, session, msgID)
		if err != nil {
			return output.String(), err
		}
		output.WriteString(cellOutput)
	}
	return output.String(), nil
}

// WorkspaceTransport moves a workspace archive to and from the runtime kernel.
// Both directions are opt-in (`colab.sync_in` / `colab.sync_out`) because large
// payloads belong on Drive rather than in a kernel message.
type WorkspaceTransport interface {
	UploadWorkspace(context.Context, Runtime, []byte) error
	// DownloadWorkspace returns a tar.gz of the remote workspace root whose
	// member names are relative to that root.
	DownloadWorkspace(context.Context, Runtime, string, []string) ([]byte, error)
}

const syncOutBegin = "CRAFTMAKE_SYNC_OUT_BEGIN"
const syncOutEnd = "CRAFTMAKE_SYNC_OUT_END"

// UploadWorkspace writes an archive (member names are absolute remote paths)
// into the runtime filesystem.
func (e *JupyterWebSocketExecutor) UploadWorkspace(ctx context.Context, runtime Runtime, archive []byte) error {
	if len(archive) == 0 {
		return nil
	}
	if len(archive) > workspaceArchiveLimit() {
		return fmt.Errorf("workspace archive exceeds the %d byte sync limit", workspaceArchiveLimit())
	}
	cell := fmt.Sprintf(`import base64, io, os, tarfile
from pathlib import Path

_payload = base64.b64decode(%s)
# Colab's filesystem root is "/"; CRAFTMAKE_COLAB_ROOT lets an emulated runtime
# relocate the workspace prefix.
_base = Path(os.environ.get("CRAFTMAKE_COLAB_ROOT", "/"))
with tarfile.open(fileobj=io.BytesIO(_payload), mode="r:gz") as _tar:
    _count = 0
    for _member in _tar.getmembers():
        if not _member.isfile():
            continue
        _target = _base / _member.name
        _target.parent.mkdir(parents=True, exist_ok=True)
        _source = _tar.extractfile(_member)
        if _source is None:
            continue
        with open(_target, "wb") as _out:
            _out.write(_source.read())
        _count += 1
print("CRAFTMAKE_SYNC_IN_OK", _count)
`, pythonString(base64.StdEncoding.EncodeToString(archive)))
	output, err := e.executeCells(ctx, runtime, []string{cell})
	if err != nil {
		return err
	}
	if !strings.Contains(output, "CRAFTMAKE_SYNC_IN_OK") {
		// The kernel took the request but did not run the cell, which is what a
		// freshly assigned runtime does before its kernel is up. That is worth
		// another attempt rather than a protocol error, because the caller's
		// upload is idempotent.
		return &RemoteError{Kind: ErrorKernelNotReady, Operation: "upload workspace", Err: fmt.Errorf("runtime did not confirm the workspace upload: %q", strings.TrimSpace(output))}
	}
	return nil
}

// DownloadWorkspace archives the remote workspace root in the runtime and
// returns the decoded tar.gz.
func (e *JupyterWebSocketExecutor) DownloadWorkspace(ctx context.Context, runtime Runtime, remoteRoot string, excludes []string) ([]byte, error) {
	if strings.TrimSpace(remoteRoot) == "" {
		return nil, fmt.Errorf("workspace download requires a remote root")
	}
	excludeList := make([]string, 0, len(excludes))
	for _, item := range excludes {
		excludeList = append(excludeList, pythonString(item))
	}
	cell := fmt.Sprintf(`import base64, io, tarfile
from pathlib import Path

_root = Path(%s)
_excludes = [%s]
_buffer = io.BytesIO()
if _root.is_dir():
    with tarfile.open(fileobj=_buffer, mode="w:gz") as _tar:
        for _path in sorted(_root.rglob("*")):
            if not _path.is_file():
                continue
            _rel = _path.relative_to(_root).as_posix()
            if any(_rel == _item or _rel.startswith(_item + "/") for _item in _excludes):
                continue
            try:
                # Member names stay relative to the workspace root; the caller
                # joins them onto the logical remote root it configured.
                _tar.add(str(_path), arcname=_rel)
            except OSError:
                continue
print(%s)
print(base64.b64encode(_buffer.getvalue()).decode())
print(%s)
`, pythonString(remoteRoot), strings.Join(excludeList, ", "), pythonString(syncOutBegin), pythonString(syncOutEnd))
	output, err := e.executeCells(ctx, runtime, []string{cell})
	if err != nil {
		return nil, err
	}
	return decodeSyncOut(output)
}

// decodeSyncOut extracts the base64 payload printed between the sync-out
// sentinels.
func decodeSyncOut(output string) ([]byte, error) {
	lines := strings.Split(output, "\n")
	begin, end := -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case syncOutBegin:
			begin = i
		case syncOutEnd:
			if begin >= 0 {
				end = i
			}
		}
	}
	if begin < 0 || end <= begin {
		return nil, &RemoteError{Kind: ErrorProtocolMismatch, Operation: "download workspace", Err: fmt.Errorf("runtime did not return a workspace archive")}
	}
	payload := strings.TrimSpace(strings.Join(lines[begin+1:end], ""))
	if payload == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, &RemoteError{Kind: ErrorProtocolMismatch, Operation: "decode workspace archive", Err: err}
	}
	if len(decoded) > workspaceArchiveLimit() {
		return nil, fmt.Errorf("downloaded workspace archive exceeds the %d byte sync limit", workspaceArchiveLimit())
	}
	return decoded, nil
}

var _ WorkspaceTransport = (*JupyterWebSocketExecutor)(nil)

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

// ioDrainWindow is how long the reader keeps draining iopub messages after the
// execute_reply for a cell. Shell (execute_reply) and iopub (stream/error/idle)
// are independent channels, so the reply can arrive before the output. It is a
// variable so tests do not have to wait for it.
var ioDrainWindow = 3 * time.Second

// drainUntilReply reads iopub/shell messages until the execution of the cell is
// finished. A real kernel may deliver execute_reply before the iopub output, so
// the reader keeps draining until the kernel reports `status: idle` for the
// request, bounded by ioDrainWindow after the reply. It accumulates
// stream/display_data/error output. Inbound signatures are verified when
// HMACKey is configured.
func (e *JupyterWebSocketExecutor) drainUntilReply(ctx context.Context, conn *minimalWSConn, session, msgID string) (string, error) {
	// The drain deadline is only armed after the reply; clear it on every exit
	// path so a cell whose idle status never arrives cannot leave a deadline in
	// the past and make the next cell fail instantly.
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	var output strings.Builder
	replied := false
	var drainDeadline time.Time
	for {
		select {
		case <-ctx.Done():
			return output.String(), ctx.Err()
		default:
		}
		if replied && !drainDeadline.IsZero() && !time.Now().Before(drainDeadline) {
			return output.String(), nil
		}
		if replied {
			_ = conn.SetReadDeadline(drainDeadline)
		}
		raw, err := conn.ReadText()
		if err != nil {
			if replied {
				// The drain window elapsed after the reply: the cell is done.
				return output.String(), nil
			}
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
		case "display_data", "execute_result":
			// Rich output (display, matplotlib, pandas) carries its text
			// representation under content.data["text/plain"].
			if content, ok := msg.Content.(map[string]any); ok {
				if data, ok := content["data"].(map[string]any); ok {
					writeContentText(&output, data, "text/plain")
				}
			}
		case "error":
			if content, ok := msg.Content.(map[string]any); ok {
				writeErrorTraceback(&output, content)
			}
		case "status":
			// iopub idle marks the end of all output for this execution.
			if content, ok := msg.Content.(map[string]any); ok {
				if state, _ := content["execution_state"].(string); state == "idle" && replied {
					return output.String(), nil
				}
			}
		case "colab_request":
			e.handleColabRequest(ctx, conn, session, msg)
		case "execute_reply":
			replied = true
			drainDeadline = time.Now().Add(ioDrainWindow)
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

// kernelReadyTimeout bounds how long execution waits for a starting kernel.
const kernelReadyTimeout = 30 * time.Second

// waitForKernelReady polls the Jupyter REST API until the kernel reports idle.
// It is best effort: when the endpoint is unavailable it returns immediately so
// callers are not blocked on a proxy that does not expose kernel state.
func (e *JupyterWebSocketExecutor) waitForKernelReady(ctx context.Context, proxyURL, proxyToken, kernelID string) {
	if kernelID == "" {
		return
	}
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	deadline := time.Now().Add(kernelReadyTimeout)
	url := strings.TrimRight(proxyURL, "/") + "/api/kernels/" + url.PathEscape(kernelID)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		req.Header.Set(HeaderProxyToken, proxyToken)
		req.Header.Set(HeaderClientAgent, "vscode")
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			resp.Body.Close()
			return
		}
		ready := false
		if resp.StatusCode == http.StatusOK {
			var kernel struct {
				ExecutionState string `json:"execution_state"`
				State          string `json:"state"`
			}
			if jsonErr := json.NewDecoder(resp.Body).Decode(&kernel); jsonErr == nil {
				state := kernel.ExecutionState
				if state == "" {
					state = kernel.State
				}
				switch state {
				case "":
					// The proxy answers 200 without a recognizable state; do not
					// burn the budget guessing, just proceed.
					resp.Body.Close()
					return
				case "idle":
					ready = true
				}
			}
		} else {
			// Any other status means this proxy does not expose kernel state in
			// the Jupyter shape; stop polling instead of burning the budget.
			resp.Body.Close()
			return
		}
		resp.Body.Close()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
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
