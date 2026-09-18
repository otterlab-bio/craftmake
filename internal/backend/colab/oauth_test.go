package colab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPKCEAuthorizationURLIncludesChallenge(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	if verifier == "" || challenge != PKCEChallenge(verifier) {
		t.Fatalf("unexpected PKCE: verifier=%q challenge=%q", verifier, challenge)
	}
	config := OAuthConfig{ClientID: "id", Scopes: []string{"scope-a", "scope-b"}}
	raw, _ := url.Parse(config.AuthorizationURL("state-1", "http://127.0.0.1:9999/", challenge))
	if raw.Query().Get("code_challenge") != challenge || raw.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing PKCE params")
	}
	if raw.Query().Get("access_type") != "offline" || raw.Query().Get("prompt") != "consent" || !strings.Contains(raw.Query().Get("scope"), "scope-a") {
		t.Fatal("unexpected auth URL params")
	}
}

func TestExchangeCodeAndRefreshAccessToken(t *testing.T) {
	var form url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		if form.Get("grant_type") == "refresh_token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "expires_in": 3600, "token_type": "Bearer"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "refresh_token": "refresh", "expires_in": 3600, "token_type": "Bearer"})
	}))
	defer server.Close()
	config := TokenConfig{ClientID: "id", ClientSecret: "secret", TokenURL: server.URL, Client: server.Client()}
	token, err := ExchangeCode(context.Background(), config, "code-1", "verifier-1", "http://127.0.0.1:9999/")
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "access" || token.RefreshToken != "refresh" || form.Get("code_verifier") != "verifier-1" {
		t.Fatalf("unexpected code exchange: %#v form=%v", token, form)
	}
	refreshed, err := RefreshAccessToken(context.Background(), config, "refresh")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "new-access" || form.Get("refresh_token") != "refresh" {
		t.Fatalf("unexpected refresh: %#v form=%v", refreshed, form)
	}
	if refreshed.Expiry.IsZero() {
		t.Fatal("expected expiry to be set")
	}
}

func TestTokenManagerRefreshesOnceThenCaches(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + fmt.Sprint(count), "expires_in": 3600, "token_type": "Bearer"})
	}))
	defer server.Close()
	manager := &TokenManager{Config: TokenConfig{ClientID: "id", TokenURL: server.URL, Client: server.Client()}, RefreshToken: "refresh-1"}
	first, err := manager.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "tok-1" {
		t.Fatalf("unexpected first token: %q", first)
	}
	second, err := manager.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second != "tok-1" || count != 1 {
		t.Fatalf("expected cached token with single refresh: second=%q count=%d", second, count)
	}
	manager.SetRefreshToken("refresh-2")
	third, err := manager.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third != "tok-2" || count != 2 {
		t.Fatalf("expected refresh after reset: third=%q count=%d", third, count)
	}
}
