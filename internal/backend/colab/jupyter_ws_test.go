package colab

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestJupyterStreamAccumulation verifies drainUntilReply accumulates stream text
// before the execute_reply terminator.
func TestJupyterStreamAccumulation(t *testing.T) {
	clientConn, kernel := net.Pipe()
	defer clientConn.Close()
	defer kernel.Close()
	client := &minimalWSConn{conn: clientConn, reader: bufio.NewReader(clientConn)}
	kernelEnd := &minimalWSConn{conn: kernel, reader: bufio.NewReader(kernel)}
	go func() {
		_, _ = kernelEnd.ReadText()
		stream := jupyterMsg{Header: jupyterHeader{MsgID: uuid.NewString(), Session: "s", Username: "u", Date: "t", MsgType: "stream", Version: "5.3"}, Metadata: map[string]any{}, Content: map[string]any{"text": "stdout-line\n"}, Channel: "iopub"}
		sdata, _ := json.Marshal(stream)
		_ = kernelEnd.sendServerText(sdata)
		reply := jupyterMsg{Header: jupyterHeader{MsgID: uuid.NewString(), Session: "s", Username: "u", Date: "t", MsgType: "execute_reply", Version: "5.3"}, Metadata: map[string]any{}, Content: map[string]any{"status": "ok"}, Channel: "shell"}
		rdata, _ := json.Marshal(reply)
		_ = kernelEnd.sendServerText(rdata)
		idle := jupyterMsg{Header: jupyterHeader{MsgID: uuid.NewString(), Session: "s", Username: "u", Date: "t", MsgType: "status", Version: "5.3"}, Metadata: map[string]any{}, Content: map[string]any{"execution_state": "idle"}, Channel: "iopub"}
		idata, _ := json.Marshal(idle)
		_ = kernelEnd.sendServerText(idata)
	}()
	executor := &JupyterWebSocketExecutor{SessionID: "sess"}
	msgID, err := executor.sendExecuteRequest(client, "sess", "print('x')")
	if err != nil {
		t.Fatal(err)
	}
	output, err := executor.drainUntilReply(context.Background(), client, "sess", msgID)
	if err != nil {
		t.Fatal(err)
	}
	if output != "stdout-line\n" {
		t.Fatalf("stream accumulation = %q, want %q", output, "stdout-line\n")
	}
}

// TestJupyterSignVerify verifies HMAC signing round-trips and rejects tampered
// signatures.
func TestJupyterSignVerify(t *testing.T) {
	key := "secret"
	content := map[string]any{"status": "ok", "execution_count": 1}
	sig := jupyterSign(key, content)
	if sig == "" {
		t.Fatal("expected a signature")
	}
	msg := jupyterMsg{Header: jupyterHeader{MsgType: "execute_reply"}, Content: content, Signature: sig}
	if !jupyterVerify(key, msg) {
		t.Fatal("expected valid signature")
	}
	bad := jupyterMsg{Header: jupyterHeader{MsgType: "execute_reply"}, Content: content, Signature: "deadbeef"}
	if jupyterVerify(key, bad) {
		t.Fatal("expected invalid signature to be rejected")
	}
	if !jupyterVerify("", msg) {
		t.Fatal("empty key should disable verification")
	}
}

// TestJupyterOutputAfterExecuteReplyIsCaptured pins the fix for a defect found
// against the real service: shell and iopub are independent channels, so a
// kernel can deliver execute_reply before the cell's stream/error output and
// idle status. Returning at the reply silently dropped that output.
func TestJupyterOutputAfterExecuteReplyIsCaptured(t *testing.T) {
	clientConn, kernel := net.Pipe()
	defer clientConn.Close()
	defer kernel.Close()
	client := &minimalWSConn{conn: clientConn, reader: bufio.NewReader(clientConn)}
	kernelEnd := &minimalWSConn{conn: kernel, reader: bufio.NewReader(kernel)}
	go func() {
		_, _ = kernelEnd.ReadText()
		send := func(msgType, channel string, content map[string]any) {
			msg := jupyterMsg{Header: jupyterHeader{MsgID: uuid.NewString(), Session: "s", Username: "u", Date: "t", MsgType: msgType, Version: "5.3"}, Metadata: map[string]any{}, Content: content, Channel: channel}
			data, _ := json.Marshal(msg)
			_ = kernelEnd.sendServerText(data)
		}
		// Reply first, output afterwards: the order a real kernel may use.
		send("execute_reply", "shell", map[string]any{"status": "ok"})
		send("stream", "iopub", map[string]any{"text": "late-output\n"})
		send("status", "iopub", map[string]any{"execution_state": "idle"})
	}()
	executor := &JupyterWebSocketExecutor{SessionID: "sess"}
	msgID, err := executor.sendExecuteRequest(client, "sess", "print('x')")
	if err != nil {
		t.Fatal(err)
	}
	output, err := executor.drainUntilReply(context.Background(), client, "sess", msgID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "late-output") {
		t.Fatalf("output delivered after execute_reply was dropped: %q", output)
	}
}

// TestJupyterDisplayDataAccumulation verifies rich output (display_data /
// execute_result) contributes its text/plain representation, which the README
// promises and the step-log materializer relies on.
func TestJupyterDisplayDataAccumulation(t *testing.T) {
	clientConn, kernel := net.Pipe()
	defer clientConn.Close()
	defer kernel.Close()
	client := &minimalWSConn{conn: clientConn, reader: bufio.NewReader(clientConn)}
	kernelEnd := &minimalWSConn{conn: kernel, reader: bufio.NewReader(kernel)}
	go func() {
		_, _ = kernelEnd.ReadText()
		display := jupyterMsg{
			Header:   jupyterHeader{MsgID: uuid.NewString(), Session: "s", Username: "u", Date: "t", MsgType: "display_data", Version: "5.3"},
			Metadata: map[string]any{},
			Content:  map[string]any{"data": map[string]any{"text/plain": "displayed-value\n"}},
			Channel:  "iopub",
		}
		data, _ := json.Marshal(display)
		_ = kernelEnd.sendServerText(data)
		reply := jupyterMsg{Header: jupyterHeader{MsgID: uuid.NewString(), Session: "s", Username: "u", Date: "t", MsgType: "execute_reply", Version: "5.3"}, Metadata: map[string]any{}, Content: map[string]any{"status": "ok"}, Channel: "shell"}
		rdata, _ := json.Marshal(reply)
		_ = kernelEnd.sendServerText(rdata)
		idle := jupyterMsg{Header: jupyterHeader{MsgID: uuid.NewString(), Session: "s", Username: "u", Date: "t", MsgType: "status", Version: "5.3"}, Metadata: map[string]any{}, Content: map[string]any{"execution_state": "idle"}, Channel: "iopub"}
		idata, _ := json.Marshal(idle)
		_ = kernelEnd.sendServerText(idata)
	}()
	executor := &JupyterWebSocketExecutor{SessionID: "sess"}
	msgID, err := executor.sendExecuteRequest(client, "sess", "df.head()")
	if err != nil {
		t.Fatal(err)
	}
	output, err := executor.drainUntilReply(context.Background(), client, "sess", msgID)
	if err != nil {
		t.Fatal(err)
	}
	if output != "displayed-value\n" {
		t.Fatalf("display_data accumulation = %q, want %q", output, "displayed-value\n")
	}
}

// TestJupyterExecutorInterruptIdle verifies Interrupt is a no-op when no
// connection is active.
func TestJupyterExecutorInterruptIdle(t *testing.T) {
	executor := &JupyterWebSocketExecutor{}
	if err := executor.Interrupt(context.Background(), Runtime{}); err != nil {
		t.Fatal(err)
	}
}

// TestJupyterExecutorInterruptActive asserts Interrupt writes a close-control frame
// on the active connection, which the kernel side reads back.
func TestJupyterExecutorInterruptActive(t *testing.T) {
	clientConn, kernel := net.Pipe()
	defer clientConn.Close()
	defer kernel.Close()
	client := &minimalWSConn{conn: clientConn, reader: bufio.NewReader(clientConn)}
	kernelEnd := &minimalWSConn{conn: kernel, reader: bufio.NewReader(kernel)}
	executor := &JupyterWebSocketExecutor{}
	executor.mu.Lock()
	executor.active = client
	executor.mu.Unlock()

	interrupted := make(chan opcodeResult, 1)
	go func() {
		opcode, _, _, err := kernelEnd.readFrame()
		interrupted <- opcodeResult{opcode: opcode, err: err}
	}()
	if err := executor.Interrupt(context.Background(), Runtime{}); err != nil {
		t.Fatal(err)
	}
	result := <-interrupted
	if result.err != nil {
		t.Fatalf("kernel read close frame: %v", result.err)
	}
	if result.opcode != wsControlClose {
		t.Fatalf("expected close control frame, got opcode %d", result.opcode)
	}
}

type opcodeResult struct {
	opcode byte
	err    error
}

// TestJupyterExecutorAuthFailure verifies a 401/403 on the WebSocket upgrade
// is classified as ErrorAuthRequired, not a generic disconnect.
func TestJupyterExecutorAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	wsURL := "ws://" + strings.TrimPrefix(server.URL, "http://") + "/channels"
	notebook, _ := (&Notebook{Cells: []NotebookCell{{CellType: "code", Source: "1+1"}}}).JSON()
	executor := &JupyterWebSocketExecutor{SessionID: "s"}
	_, err := executor.ExecuteNotebook(context.Background(), Runtime{ID: wsURL}, notebook)
	if err == nil {
		t.Fatal("expected error")
	}
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("expected RemoteError, got %T", err)
	}
	if remote.Kind != ErrorAuthRequired {
		t.Fatalf("expected auth-required, got %s", remote.Kind)
	}
}
func TestJupyterResolveKernelAndFormatChannels(t *testing.T) {
	// 1. Existing kernels returned from GET /api/kernels
	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/kernels" && r.Method == http.MethodGet {
			if r.Header.Get(HeaderProxyToken) != "ptok" {
				t.Errorf("missing proxy token header")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "kernel-existing"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server1.Close()

	executor := &JupyterWebSocketExecutor{SessionID: "sess-1", Client: server1.Client()}
	kid, err := executor.resolveKernel(context.Background(), server1.URL, "ptok")
	if err != nil {
		t.Fatal(err)
	}
	if kid != "kernel-existing" {
		t.Fatalf("resolveKernel = %q, want %q", kid, "kernel-existing")
	}

	wsURL := formatChannelsWSURL(server1.URL, kid, "sess-1")
	if !strings.HasPrefix(wsURL, "ws://") || !strings.Contains(wsURL, "/api/kernels/kernel-existing/channels?session_id=sess-1") {
		t.Fatalf("formatChannelsWSURL = %q", wsURL)
	}

	// 2. Fallback to POST /api/sessions when /api/kernels returns empty
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/kernels" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		if r.URL.Path == "/api/sessions" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"kernel": map[string]any{"id": "kernel-created"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server2.Close()

	executor2 := &JupyterWebSocketExecutor{SessionID: "sess-2", Client: server2.Client()}
	kid2, err := executor2.resolveKernel(context.Background(), server2.URL, "ptok2")
	if err != nil {
		t.Fatal(err)
	}
	if kid2 != "kernel-created" {
		t.Fatalf("resolveKernel created = %q, want %q", kid2, "kernel-created")
	}
}

// TestJupyterMissingIdleDoesNotPoisonNextCell reproduces the live failure where
// a cell whose iopub idle status never arrived left the drain deadline in the
// past, so the *next* cell on the same connection failed instantly with an i/o
// timeout even though the kernel was healthy.
func TestJupyterMissingIdleDoesNotPoisonNextCell(t *testing.T) {
	original := ioDrainWindow
	ioDrainWindow = 50 * time.Millisecond
	defer func() { ioDrainWindow = original }()

	clientConn, kernel := net.Pipe()
	defer clientConn.Close()
	defer kernel.Close()
	client := &minimalWSConn{conn: clientConn, reader: bufio.NewReader(clientConn)}
	kernelEnd := &minimalWSConn{conn: kernel, reader: bufio.NewReader(kernel)}
	go func() {
		send := func(msgType, channel string, content map[string]any) {
			msg := jupyterMsg{Header: jupyterHeader{MsgID: uuid.NewString(), Session: "s", Username: "u", Date: "t", MsgType: msgType, Version: "5.3"}, Metadata: map[string]any{}, Content: content, Channel: channel}
			data, _ := json.Marshal(msg)
			_ = kernelEnd.sendServerText(data)
		}
		// Cell 1: reply without the idle status (as the real proxy can do).
		_, _ = kernelEnd.ReadText()
		send("stream", "iopub", map[string]any{"text": "cell-1\n"})
		send("execute_reply", "shell", map[string]any{"status": "ok"})
		// Cell 2: a healthy cell on the same connection.
		_, _ = kernelEnd.ReadText()
		send("stream", "iopub", map[string]any{"text": "cell-2\n"})
		send("execute_reply", "shell", map[string]any{"status": "ok"})
		send("status", "iopub", map[string]any{"execution_state": "idle"})
	}()
	executor := &JupyterWebSocketExecutor{SessionID: "sess"}
	firstID, err := executor.sendExecuteRequest(client, "sess", "print('1')")
	if err != nil {
		t.Fatal(err)
	}
	first, err := executor.drainUntilReply(context.Background(), client, "sess", firstID)
	if err != nil {
		t.Fatalf("cell 1: %v", err)
	}
	if !strings.Contains(first, "cell-1") {
		t.Fatalf("cell 1 output missing: %q", first)
	}
	secondID, err := executor.sendExecuteRequest(client, "sess", "print('2')")
	if err != nil {
		t.Fatal(err)
	}
	second, err := executor.drainUntilReply(context.Background(), client, "sess", secondID)
	if err != nil {
		t.Fatalf("cell 2 must not fail because the previous cell never reported idle: %v", err)
	}
	if !strings.Contains(second, "cell-2") {
		t.Fatalf("cell 2 output missing: %q", second)
	}
}
