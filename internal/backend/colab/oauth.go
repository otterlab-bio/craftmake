package colab

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Default Google OAuth endpoints and scopes, matching colab-cli.
const (
	GoogleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	GoogleTokenURL    = "https://oauth2.googleapis.com/token"
	GoogleUserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"
)

type TokenResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    int       `json:"expires_in"`
	TokenType    string    `json:"token_type"`
	Scope        string    `json:"scope"`
	Expiry       time.Time `json:"-"`
}

type GoogleUser struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
	AuthURL      string
	TokenURL     string
	UserInfoURL  string
	Client       HTTPDoer
}

func (c OAuthConfig) withDefaults() OAuthConfig {
	if c.AuthURL == "" {
		c.AuthURL = GoogleAuthURL
	}
	if c.TokenURL == "" {
		c.TokenURL = GoogleTokenURL
	}
	if c.UserInfoURL == "" {
		c.UserInfoURL = GoogleUserInfoURL
	}
	if c.Client == nil {
		c.Client = http.DefaultClient
	}
	return c
}

// AuthorizationURL builds the loopback authorization URL with PKCE S256.
func (c OAuthConfig) AuthorizationURL(state, redirectURI, codeChallenge string) string {
	c = c.withDefaults()
	q := url.Values{}
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	q.Set("scope", strings.Join(c.Scopes, " "))
	q.Set("code_challenge_method", "S256")
	q.Set("code_challenge", codeChallenge)
	q.Set("state", state)
	return c.AuthURL + "?" + q.Encode()
}

// GeneratePKCE creates a random code verifier and its S256 challenge.
func GeneratePKCE() (verifier, challenge string, err error) {
	buffer := make([]byte, 48)
	if _, err = rand.Read(buffer); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buffer)
	return verifier, PKCEChallenge(verifier), nil
}

func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// TokenConfig identifies a token exchange/refresh request.
type TokenConfig struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
	Client       HTTPDoer
}

// ExchangeCode trades an authorization code for tokens (loopback flow).
func ExchangeCode(ctx context.Context, config TokenConfig, code, codeVerifier, redirectURI string) (TokenResponse, error) {
	if config.TokenURL == "" {
		config.TokenURL = GoogleTokenURL
	}
	if config.Client == nil {
		config.Client = http.DefaultClient
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", config.ClientID)
	if config.ClientSecret != "" {
		form.Set("client_secret", config.ClientSecret)
	}
	return postToken(ctx, config, form)
}

// RefreshAccessToken renews an access token from a refresh token (colab-cli AuthManager).
func RefreshAccessToken(ctx context.Context, config TokenConfig, refreshToken string) (TokenResponse, error) {
	if config.TokenURL == "" {
		config.TokenURL = GoogleTokenURL
	}
	if config.Client == nil {
		config.Client = http.DefaultClient
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", config.ClientID)
	if config.ClientSecret != "" {
		form.Set("client_secret", config.ClientSecret)
	}
	return postToken(ctx, config, form)
}

func postToken(ctx context.Context, config TokenConfig, form url.Values) (TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.TokenURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := config.Client.Do(req)
	if err != nil {
		return TokenResponse{}, &RemoteError{Kind: ErrorKernelDisconnected, Operation: "OAuth token request", Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return TokenResponse{}, &RemoteError{Kind: ErrorAuthRequired, Operation: "OAuth token request", StatusCode: response.StatusCode, Err: fmt.Errorf("token endpoint HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(b)))}
	}
	var token TokenResponse
	if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
		return TokenResponse{}, &RemoteError{Kind: ErrorProtocolMismatch, Operation: "decode OAuth token", Err: err}
	}
	if token.AccessToken == "" {
		return TokenResponse{}, errors.New("token response missing access_token")
	}
	if token.ExpiresIn > 0 {
		token.Expiry = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	return token, nil
}

// TokenRefreshMargin is applied before an access token is considered expired.
const TokenRefreshMargin = 5 * time.Minute

// TokenManager caches an access token and transparently refreshes it from the
// stored refresh token when it is missing or close to expiry, mirroring the
// colab-cli AuthManager refreshIfNeeded behavior.
type TokenManager struct {
	Config       TokenConfig
	RefreshToken string
	accessToken  string
	expiry       time.Time
}

// SetRefreshToken updates the stored refresh token and clears the cached token.
func (m *TokenManager) SetRefreshToken(token string) {
	m.RefreshToken = token
	m.accessToken = ""
	m.expiry = time.Time{}
}

// AccessToken returns a valid access token, refreshing when necessary.
func (m *TokenManager) AccessToken(ctx context.Context) (string, error) {
	if m.RefreshToken == "" {
		return "", errors.New("Colab refresh token is not set")
	}
	if m.accessToken != "" && time.Until(m.expiry) > TokenRefreshMargin {
		return m.accessToken, nil
	}
	token, err := RefreshAccessToken(ctx, m.Config, m.RefreshToken)
	if err != nil {
		return "", err
	}
	m.accessToken = token.AccessToken
	m.expiry = token.Expiry
	return m.accessToken, nil
}

// FetchGoogleUser retrieves the OAuth account profile.
func FetchGoogleUser(ctx context.Context, config OAuthConfig, accessToken string) (GoogleUser, error) {
	c := config.withDefaults()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.UserInfoURL, nil)
	if err != nil {
		return GoogleUser{}, err
	}
	req.Header.Set(HeaderAuthorization, "Bearer "+accessToken)
	response, err := c.Client.Do(req)
	if err != nil {
		return GoogleUser{}, &RemoteError{Kind: ErrorKernelDisconnected, Operation: "fetch Google user", Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return GoogleUser{}, &RemoteError{Kind: ClassifyHTTPStatus(response.StatusCode), Operation: "fetch Google user", StatusCode: response.StatusCode, Err: fmt.Errorf("HTTP %d", response.StatusCode)}
	}
	var user GoogleUser
	if err := json.NewDecoder(response.Body).Decode(&user); err != nil {
		return GoogleUser{}, err
	}
	return user, nil
}
