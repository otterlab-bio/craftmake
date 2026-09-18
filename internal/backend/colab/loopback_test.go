package colab

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestLoopbackServerCapturesCodeAndValidatesState(t *testing.T) {
	server, redirectURI, err := StartLoopbackServer("nonce=abc")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	resultCh := make(chan LoopbackResult, 1)
	go func() {
		result, _ := server.Wait(context.Background(), 5*time.Second)
		resultCh <- result
	}()

	// Simulate the browser callback with the correct state.
	resp, err := http.Get(redirectURI + "?code=the-code&state=nonce%3Dabc")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	result := <-resultCh
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Code != "the-code" {
		t.Fatalf("code = %q", result.Code)
	}
}

func TestLoopbackServerRejectsWrongStateAndStrayRequests(t *testing.T) {
	server, redirectURI, err := StartLoopbackServer("nonce=abc")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	resultCh := make(chan LoopbackResult, 1)
	go func() {
		result, _ := server.Wait(context.Background(), 5*time.Second)
		resultCh <- result
	}()

	// 1. Stray /favicon.ico should return 404 and not kill the server.
	resp1, err := http.Get(redirectURI + "favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusNotFound {
		t.Fatalf("favicon status = %d, want 404", resp1.StatusCode)
	}

	// 2. Wrong state returns 400 and does not kill the server.
	resp2, err := http.Get(redirectURI + "?code=bad&state=nonce%3Dwrong")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state status = %d, want 400", resp2.StatusCode)
	}

	// 3. Subsequent legitimate callback succeeds.
	resp3, err := http.Get(redirectURI + "?code=valid-code&state=nonce%3Dabc")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("legit callback status = %d, want 200", resp3.StatusCode)
	}

	result := <-resultCh
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Code != "valid-code" {
		t.Fatalf("code = %q, want %q", result.Code, "valid-code")
	}
}

func TestLoopbackServerTimesOut(t *testing.T) {
	server, _, err := StartLoopbackServer("nonce=abc")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	_, err = server.Wait(context.Background(), 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout")
	}
}
