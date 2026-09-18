package colab

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// LoopbackResult carries the authorization code captured by the loopback
// server, or the error that prevented capture.
type LoopbackResult struct {
	Code  string
	State string
	Err   error
}

// LoopbackServer is a minimal 127.0.0.1 OAuth redirect server. It binds an
// ephemeral port, serves the authorization callback, validates the state
// nonce, and returns the captured code. It never blocks the kernel waiting for
// input: it either receives the callback or times out.
type LoopbackServer struct {
	listener net.Listener
	state    string
	result   chan LoopbackResult
}

// StartLoopbackServer binds 127.0.0.1:0 and returns the redirect URI.
func StartLoopbackServer(state string) (*LoopbackServer, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("bind loopback: %w", err)
	}
	s := &LoopbackServer{listener: listener, state: state, result: make(chan LoopbackResult, 1)}
	redirectURI := "http://" + listener.Addr().String() + "/"
	go s.serve()
	return s, redirectURI, nil
}

func (s *LoopbackServer) serve() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	_ = http.Serve(s.listener, mux)
}

func (s *LoopbackServer) handle(w http.ResponseWriter, r *http.Request) {
	// Ignore non-root requests such as /favicon.ico without failing the callback wait.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	state := query.Get("state")
	code := query.Get("code")
	// If state doesn't match or code is missing, reject this HTTP request
	// but keep listening for the legitimate callback instead of killing the server.
	if state != s.state {
		http.Error(w, "Invalid state", http.StatusBadRequest)
		return
	}
	if code == "" {
		http.Error(w, "Missing code", http.StatusBadRequest)
		return
	}
	select {
	case s.result <- LoopbackResult{Code: code, State: state}:
	default:
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<html><body style='font-family:sans-serif;text-align:center;padding-top:40px'><h1>Authentication successful!</h1><p>You can close this tab and return to the terminal.</p></body></html>"))
}

// Wait returns the captured code or a timeout error.
func (s *LoopbackServer) Wait(ctx context.Context, timeout time.Duration) (LoopbackResult, error) {
	select {
	case result := <-s.result:
		return result, result.Err
	case <-ctx.Done():
		return LoopbackResult{}, ctx.Err()
	case <-time.After(timeout):
		return LoopbackResult{}, fmt.Errorf("authentication timed out")
	}
}

// Close stops the loopback server.
func (s *LoopbackServer) Close() error { return s.listener.Close() }

// RedirectURI returns the loopback redirect URI.
func (s *LoopbackServer) RedirectURI() string {
	return "http://" + s.listener.Addr().String() + "/"
}
