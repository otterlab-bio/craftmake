// Command mockcolab is a dependency-free local emulator of the parts of the
// Google Colab control plane that the craftmake "colab" backend talks to:
//
//   - GET/POST /tun/m/assign                          (runtime assignment + XSRF)
//   - GET/POST /tun/m/unassign/<endpoint>             (release)
//   - GET/POST /tun/m/credentials-propagation/<ep>    (Drive "dfs_ephemeral" grant)
//   - GET      /v1/assignments                        (dangling-assignment sweep)
//   - POST     /token                                 (OAuth refresh-token exchange)
//   - GET      /api/kernels                           (kernel discovery)
//   - WS       /api/kernels/<id>/channels             (Jupyter kernel protocol)
//   - POST     /consent                               (simulates the user clicking "Allow")
//
// The emulated Jupyter kernel actually executes the notebook cells with the
// local bash/python3 (rewriting the runtime's /content prefix to a writable
// directory), so a full `craftmake action run --backend colab` can be verified
// end-to-end with no Google account involved.
//
// The Drive consent decision is persisted in a state file, so the "authorize
// once, then never again" behaviour can be verified across separate CLI
// invocations: the first call hands out an authorization URL, later calls see
// success=true with no user interaction.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type mock struct {
	contentRoot string
	runtimeRoot string
	logPath     string
	statePath   string
	baseURL     string
	delay       time.Duration

	mu          sync.Mutex
	consented   bool
	timerArmed  bool
	calls       []string
	requestSent bool
	cellSeq     int
}

// ---------------------------------------------------------------- consent state

func (m *mock) loadState() {
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return
	}
	var s struct {
		Consented bool `json:"consented"`
	}
	if json.Unmarshal(data, &s) == nil {
		m.consented = s.Consented
	}
}

func (m *mock) saveState() {
	_ = os.MkdirAll(filepath.Dir(m.statePath), 0o755)
	data, _ := json.MarshalIndent(map[string]any{"consented": m.consented}, "", "  ")
	_ = os.WriteFile(m.statePath, append(data, '\n'), 0o644)
}

func (m *mock) consentedNow() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.consented
}

// grantConsent flips the persisted "user allowed Drive access" bit, exactly the
// way Google remembers the consent for the account after the first approval.
func (m *mock) grantConsent() {
	m.mu.Lock()
	already := m.consented
	m.consented = true
	m.mu.Unlock()
	if !already {
		m.saveState()
		m.record("CONSENT granted (persisted to state file)")
	}
}

// scheduleAutoConsent emulates the user clicking "Allow" shortly after the
// authorization URL has been shown.
func (m *mock) scheduleAutoConsent() {
	m.mu.Lock()
	if m.timerArmed || m.consented {
		m.mu.Unlock()
		return
	}
	m.timerArmed = true
	m.mu.Unlock()
	go func() {
		time.Sleep(m.delay)
		m.grantConsent()
	}()
}

func (m *mock) record(line string) {
	entry := fmt.Sprintf("%s %s", time.Now().UTC().Format("15:04:05.000"), line)
	m.mu.Lock()
	m.calls = append(m.calls, entry)
	m.mu.Unlock()
	log.Printf("MOCK %s", line)
	if m.logPath != "" {
		if f, err := os.OpenFile(m.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString(entry + "\n")
			_ = f.Close()
		}
	}
}

// ------------------------------------------------------------------ HTTP plumbing

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (m *mock) handleToken(w http.ResponseWriter, r *http.Request) {
	m.record("POST /token (oauth refresh)")
	writeJSON(w, http.StatusOK, map[string]any{"access_token": "demo-access-token", "expires_in": 3600, "token_type": "Bearer"})
}

func (m *mock) handleAssign(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		m.record("GET  /tun/m/assign (xsrf)")
		writeJSON(w, http.StatusOK, map[string]any{"token": "xsrf-assign"})
		return
	}
	m.record("POST /tun/m/assign (claim runtime)")
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoint":       "demo-runtime-1",
		"accelerator":    "T4",
		"variant":        "GPU",
		"machineShape":   0,
		"runtimeVersion": "2026.09",
		"runtimeProxyInfo": map[string]any{
			"token": "demo-proxy-token",
			"url":   m.baseURL,
		},
	})
}

func (m *mock) handleUnassign(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		m.record("GET  /tun/m/unassign (xsrf)")
		writeJSON(w, http.StatusOK, map[string]any{"token": "xsrf-unassign"})
		return
	}
	m.record("POST /tun/m/unassign (release runtime)")
	w.WriteHeader(http.StatusNoContent)
}

func (m *mock) handlePropagate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dryRun := q.Get("dryrun") == "true"
	authType := q.Get("authtype")
	if r.Method == http.MethodGet {
		m.record(fmt.Sprintf("GET  /tun/m/credentials-propagation (xsrf, authtype=%s)", authType))
		writeJSON(w, http.StatusOK, map[string]any{"token": "xsrf-propagate"})
		return
	}
	if m.consentedNow() {
		m.record(fmt.Sprintf("POST /tun/m/credentials-propagation dryrun=%v authtype=%s -> success=true (no user interaction)", dryRun, authType))
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
		return
	}
	m.record(fmt.Sprintf("POST /tun/m/credentials-propagation dryrun=%v authtype=%s -> success=false (authorization required)", dryRun, authType))
	m.scheduleAutoConsent()
	writeJSON(w, http.StatusOK, map[string]any{
		"success":                   false,
		"unauthorized_redirect_uri": m.baseURL + "/consent",
	})
}

func (m *mock) handleConsent(w http.ResponseWriter, r *http.Request) {
	m.record("GET  /consent (user pressed Allow)")
	m.grantConsent()
	w.Header().Set("Content-Type", "text/html")
	_, _ = io.WriteString(w, "<html><body>Google Drive access granted. You can close this tab.</body></html>")
}

func (m *mock) handleListAssignments(w http.ResponseWriter, r *http.Request) {
	m.record("GET  /v1/assignments")
	writeJSON(w, http.StatusOK, map[string]any{"assignments": []any{}})
}

func (m *mock) handleLog(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"consented": m.consented, "calls": m.calls})
}

func (m *mock) handleKernels(w http.ResponseWriter, r *http.Request) {
	m.record("GET  /api/kernels (kernel discovery)")
	writeJSON(w, http.StatusOK, []map[string]any{{"id": "demo-kernel", "name": "python3"}})
}

// ------------------------------------------------------------------ Jupyter kernel

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func (m *mock) handleKernelChannel(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	accept := wsAccept(r.Header.Get("Sec-WebSocket-Key"))
	fmt.Fprintf(bufrw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err := bufrw.Flush(); err != nil {
		conn.Close()
		return
	}
	m.record("WS   /api/kernels/demo-kernel/channels (kernel attached)")
	go func() {
		defer conn.Close()
		m.serveKernel(bufrw.Reader, conn)
	}()
}

func (m *mock) serveKernel(reader *bufio.Reader, conn net.Conn) {
	for {
		opcode, payload, err := readClientFrame(reader)
		if err != nil {
			return
		}
		switch opcode {
		case 0x8: // close
			return
		case 0x9: // ping -> pong
			_ = writeServerFrame(conn, 0xa, payload)
		case 0x1: // text
			var msg struct {
				Header struct {
					MsgType string `json:"msg_type"`
				} `json:"header"`
				Content map[string]any `json:"content"`
			}
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			code, _ := msg.Content["code"].(string)
			if msg.Header.MsgType == "execute_request" && strings.TrimSpace(code) != "" {
				m.runCell(conn, code)
			}
		}
	}
}

func (m *mock) runCell(conn net.Conn, code string) {
	rewritten := m.rewriteContentPaths(code)
	stdout, stderr := m.executeCell(rewritten)
	m.record(fmt.Sprintf("EXEC cell (%s, %d bytes)", cellKind(code), len(code)))
	if stdout != "" {
		m.sendStream(conn, stdout)
	}
	if stderr != "" {
		m.sendStream(conn, stderr)
	}
	// A real Colab kernel asks the frontend to propagate Drive credentials when
	// user code touches Drive (drive.mount). Emit that request once so the
	// craftmake AuthConsentHandler path is exercised end to end.
	if m.emitColabRequestFor(code) {
		m.mu.Lock()
		first := !m.requestSent
		m.requestSent = true
		m.mu.Unlock()
		if first {
			m.record("REQ  colab_request authType=dfs_ephemeral (kernel asks for Drive credentials)")
			m.sendColabRequest(conn)
		}
	}
	m.sendExecuteReply(conn)
}

func (m *mock) emitColabRequestFor(code string) bool {
	return strings.Contains(code, "google.colab") || strings.Contains(code, "drive.mount")
}

func cellKind(code string) string {
	if strings.HasPrefix(strings.TrimSpace(code), "%%bash") {
		return "%%bash"
	}
	return "python"
}

// rewriteContentPaths maps the Colab runtime's /content prefix onto a writable
// local directory, because this emulator does not run inside a Colab VM.
func (m *mock) rewriteContentPaths(code string) string {
	code = strings.ReplaceAll(code, "/content/", m.contentRoot+"/")
	code = strings.ReplaceAll(code, "'/content'", "'"+m.contentRoot+"'")
	code = strings.ReplaceAll(code, `"/content"`, `"`+m.contentRoot+`"`)
	return code
}

func (m *mock) executeCell(code string) (string, string) {
	if err := os.MkdirAll(filepath.Join(m.contentRoot, ".cells"), 0o755); err != nil {
		return "", err.Error()
	}
	var cmd *exec.Cmd
	if strings.HasPrefix(strings.TrimSpace(code), "%%bash") {
		script := code
		if idx := strings.Index(script, "\n"); idx >= 0 {
			script = script[idx+1:]
		}
		cmd = exec.Command("bash", "-c", script)
	} else {
		m.mu.Lock()
		m.cellSeq++
		seq := m.cellSeq
		m.mu.Unlock()
		path := filepath.Join(m.contentRoot, ".cells", fmt.Sprintf("cell-%03d.py", seq))
		if err := os.WriteFile(path, []byte(code), 0o644); err != nil {
			return "", err.Error()
		}
		cmd = exec.Command("python3", path)
	}
	cmd.Dir = m.contentRoot
	cmd.Env = append(os.Environ(), "CRAFTMAKE_COLAB_ROOT="+m.runtimeRoot)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() == 0 {
			stderr.WriteString(err.Error() + "\n")
		}
	}
	return stdout.String(), stderr.String()
}

func (m *mock) sendStream(conn net.Conn, text string) {
	m.sendJupyter(conn, "stream", map[string]any{}, map[string]any{"name": "stdout", "text": text})
}

func (m *mock) sendColabRequest(conn net.Conn) {
	m.sendJupyter(conn, "colab_request", map[string]any{"colab_msg_id": "demo-colab-msg-1"}, map[string]any{
		"request": map[string]any{"authType": "dfs_ephemeral", "kind": "credentials"},
	})
}

func (m *mock) sendExecuteReply(conn net.Conn) {
	m.sendJupyter(conn, "execute_reply", map[string]any{}, map[string]any{"status": "ok", "execution_count": 1})
	// Real kernels follow the reply with the iopub idle status, which is the
	// definitive end of a cell's output.
	m.sendJupyter(conn, "status", map[string]any{}, map[string]any{"execution_state": "idle"})
}

func (m *mock) sendJupyter(conn net.Conn, msgType string, metadata, content map[string]any) {
	msg := map[string]any{
		"header": map[string]any{
			"msg_id":   fmt.Sprintf("demo-%d", time.Now().UnixNano()),
			"session":  "craftmake",
			"username": "kernel",
			"date":     time.Now().UTC().Format(time.RFC3339Nano),
			"msg_type": msgType,
			"version":  "5.3",
		},
		"parent_header": map[string]any{},
		"metadata":      metadata,
		"content":       content,
		"channel":       "iopub",
	}
	data, _ := json.Marshal(msg)
	if err := writeServerFrame(conn, 0x1, data); err != nil {
		return
	}
}

func readClientFrame(reader *bufio.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

// writeServerFrame writes an unmasked server frame (RFC 6455 requires servers
// not to mask).
func writeServerFrame(w io.Writer, opcode byte, payload []byte) error {
	var header []byte
	switch n := len(payload); {
	case n < 126:
		header = []byte{0x80 | opcode, byte(n)}
	case n <= 65535:
		header = []byte{0x80 | opcode, 126, 0, 0}
		binary.BigEndian.PutUint16(header[2:], uint16(n))
	default:
		header = make([]byte, 10)
		header[0], header[1] = 0x80|opcode, 127
		binary.BigEndian.PutUint64(header[2:], uint64(n))
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ------------------------------------------------------------------------ main

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	readyFile := flag.String("ready-file", "", "file to write the base URL to once listening")
	statePath := flag.String("state", "consent.json", "persisted Drive consent state file")
	logPath := flag.String("log", "", "file to append control-plane call log to (JSON lines)")
	contentRoot := flag.String("content-root", "", "writable directory used as the runtime's /content root")
	runtimeRoot := flag.String("runtime-root", "", "value exported as CRAFTMAKE_COLAB_ROOT to executed cells (defaults to the content root)")
	consentDelay := flag.Duration("consent-delay", 600*time.Millisecond, "simulated delay before the user clicks Allow")
	flag.Parse()
	log.SetFlags(log.Ltime)

	if *contentRoot == "" {
		dir, err := os.MkdirTemp("", "mockcolab-content-")
		if err != nil {
			log.Fatalf("content root: %v", err)
		}
		*contentRoot = dir
	}
	if err := os.MkdirAll(*contentRoot, 0o755); err != nil {
		log.Fatalf("content root: %v", err)
	}
	if *runtimeRoot == "" {
		// Cell payloads use absolute runtime paths, so the directory standing in
		// for "/" is the parent of the emulated /content directory.
		*runtimeRoot = filepath.Dir(*contentRoot)
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	baseURL := "http://" + listener.Addr().String()

	m := &mock{contentRoot: *contentRoot, runtimeRoot: *runtimeRoot, logPath: *logPath, statePath: *statePath, baseURL: baseURL, delay: *consentDelay}
	m.loadState()
	if *logPath != "" {
		_ = os.MkdirAll(filepath.Dir(*logPath), 0o755)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", m.handleToken)
	mux.HandleFunc("/tun/m/assign", m.handleAssign)
	mux.HandleFunc("/tun/m/unassign/", m.handleUnassign)
	mux.HandleFunc("/tun/m/credentials-propagation/", m.handlePropagate)
	mux.HandleFunc("/v1/assignments", m.handleListAssignments)
	mux.HandleFunc("/api/kernels", m.handleKernels)
	mux.HandleFunc("/api/kernels/", m.handleKernelChannel)
	mux.HandleFunc("/consent", m.handleConsent)
	mux.HandleFunc("/log", m.handleLog)

	if *readyFile != "" {
		_ = os.MkdirAll(filepath.Dir(*readyFile), 0o755)
		if err := os.WriteFile(*readyFile, []byte(baseURL+"\n"), 0o644); err != nil {
			log.Fatalf("ready file: %v", err)
		}
	}
	log.Printf("mock Colab control plane listening on %s (consent state: %s, content root: %s)", baseURL, *statePath, *contentRoot)
	if err := http.Serve(listener, mux); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
