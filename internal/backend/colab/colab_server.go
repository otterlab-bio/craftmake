package colab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// notebookHash derives a valid Colab assignment notebook hash (nbh) from an
// identifier. Colab's NBH-Regex is ^[a-zA-Z0-9\-_.]{44}$; colab-vscode builds it
// from a UUID by replacing '-' with '_' and padding with '.' to 44 chars. We
// derive a deterministic UUID from the identifier to keep the nbh stable per run.
func NotebookHash(identifier string) string {
	sum := sha256.Sum256([]byte(identifier))
	uuid := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	return strings.ReplaceAll(uuid, "-", "_") + strings.Repeat(".", 44-len(uuid))
}

func notebookHash(identifier string) string { return NotebookHash(identifier) }

// Const default Colab backend domains used by the reference implementation.
const (
	DefaultColabDomain     = "https://colab.research.google.com"
	DefaultColabGapiDomain = "https://colab.pa.googleapis.com"
	TunEndpoint            = "/tun/m"
)

// Default HTTP headers used by the Colab backend.
const (
	HeaderAuthorization = "Authorization"
	HeaderClientAgent   = "X-Colab-Client-Agent"
	HeaderTunnel        = "X-Colab-Tunnel"
	HeaderXSRF          = "X-Goog-Colab-Token"
	HeaderProxyToken    = "X-Colab-Runtime-Proxy-Token"
	HeaderVSAppName     = "X-Colab-VS-Code-App-Name"
	HeaderVSExtVersion  = "X-Colab-VS-Code-Extension-Version"
)

type RuntimeSpec struct {
	Variant      string
	Accelerator  string
	Shape        int
	Version      string
	NotebookHash string
}

type RuntimeProxyInfo struct {
	Token                 string `json:"token"`
	TokenExpiresInSeconds int    `json:"tokenExpiresInSeconds"`
	URL                   string `json:"url"`
}

// VariantValue accepts the Colab variant as either a string enum (GET) or a
// number (POST), matching colab-vscode's normalization.
type VariantValue string

func (v *VariantValue) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*v = VariantValue(s)
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	switch n {
	case 0:
		*v = "DEFAULT"
	case 1:
		*v = "GPU"
	case 2:
		*v = "TPU"
	default:
		*v = VariantValue(fmt.Sprintf("%d", n))
	}
	return nil
}

func (v VariantValue) String() string { return string(v) }

// ShapeValue accepts Colab machineShape as either an int (0, 1) or a string enum.
type ShapeValue int

func (s *ShapeValue) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*s = ShapeValue(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		if strings.EqualFold(str, "HM") || strings.Contains(strings.ToUpper(str), "HIGH") {
			*s = 1
		} else {
			*s = 0
		}
		return nil
	}
	return nil
}

func (s ShapeValue) Int() int { return int(s) }

type Assignment struct {
	Endpoint         string           `json:"endpoint"`
	Accelerator      string           `json:"accelerator"`
	Variant          VariantValue     `json:"variant"`
	MachineShape     ShapeValue       `json:"machineShape"`
	RuntimeProxyInfo RuntimeProxyInfo `json:"runtimeProxyInfo"`
	RuntimeVersion   string           `json:"runtimeVersion"`
}

type UserInfo struct {
	SubscriptionTier       string            `json:"subscriptionTier"`
	EligibleAccelerators   []AcceleratorInfo `json:"eligibleAccelerators"`
	IneligibleAccelerators []AcceleratorInfo `json:"ineligibleAccelerators"`
}

type AcceleratorInfo struct {
	Variant string   `json:"variant"`
	Models  []string `json:"models"`
}

// ColabServerClient talks to the Colab v1 backend, mirroring the reference
// implementation in googlecolab/colab-vscode. It keeps the endpoint paths,
// header names and the GET-then-POST XSRF choreography behind one seam.
type ColabServerClient struct {
	ColabDomain      string
	ColabGapiDomain  string
	Client           HTTPDoer
	ClientAgent      string
	AppName          string
	ExtensionVersion string
	GetAccessToken   func() (string, error)
	OnAuthError      func() error
}

func NewColabServerClient(colabDomain, colabGapiDomain string, client HTTPDoer) *ColabServerClient {
	if strings.TrimSpace(colabDomain) == "" {
		colabDomain = DefaultColabDomain
	}
	if strings.TrimSpace(colabGapiDomain) == "" {
		colabGapiDomain = DefaultColabGapiDomain
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &ColabServerClient{ColabDomain: strings.TrimRight(colabDomain, "/"), ColabGapiDomain: strings.TrimRight(colabGapiDomain, "/"), Client: client, ClientAgent: "vscode"}
}

func (c *ColabServerClient) token() (string, error) {
	if c.GetAccessToken == nil {
		return "", nil
	}
	return c.GetAccessToken()
}

// Assign obtains a runtime. It GETs the assignment target; if the response
// carries an XSRF token the client must POST to claim the machine.
func (c *ColabServerClient) Assign(ctx context.Context, spec RuntimeSpec) (Assignment, error) {
	path := c.tunPath("assign")
	if spec.NotebookHash == "" {
		spec.NotebookHash = NotebookHash("craftmake-default-session")
	}
	path += "?nbh=" + url.QueryEscape(spec.NotebookHash)
	// The Colab API requires the authuser parameter to be set (colab-vscode).
	path += "&authuser=0"
	// colab-vscode only sets variant when it is not DEFAULT.
	if spec.Variant != "" && spec.Variant != "DEFAULT" {
		path += "&variant=" + url.QueryEscape(spec.Variant)
	}
	// colab-vscode uses the `accelerator` param name (not `acc`).
	if spec.Accelerator != "" {
		path += "&accelerator=" + url.QueryEscape(spec.Accelerator)
	}
	// colab-vscode only sets shape for high-mem (`hm`); STANDARD is omitted.
	if spec.Shape == 1 {
		path += "&shape=hm"
	}
	// colab-vscode uses the `runtime_version_label` param name (not `version`).
	if spec.Version != "" {
		path += "&runtime_version_label=" + url.QueryEscape(spec.Version)
	}
	var tokenResponse struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodGet, c.ColabDomain, path, nil, &tokenResponse); err != nil {
		return Assignment{}, err
	}
	if tokenResponse.Token == "" {
		var assigned Assignment
		if err := c.do(ctx, http.MethodGet, c.ColabDomain, path, nil, &assigned); err != nil {
			return Assignment{}, err
		}
		if assigned.Endpoint == "" {
			return Assignment{}, &RemoteError{Kind: ErrorProtocolMismatch, Operation: "assign runtime", Err: fmt.Errorf("assignment response missing endpoint")}
		}
		return assigned, nil
	}
	var posted Assignment
	if err := c.do(ctx, http.MethodPost, c.ColabDomain, path, nil, &posted, tokenResponse.Token); err != nil {
		return Assignment{}, err
	}
	if posted.Endpoint == "" {
		return Assignment{}, &RemoteError{Kind: ErrorProtocolMismatch, Operation: "post assignment", Err: fmt.Errorf("assignment response missing endpoint")}
	}
	return posted, nil
}

// Unassign releases a runtime, following the GET-then-POST XSRF choreography.
func (c *ColabServerClient) Unassign(ctx context.Context, endpoint string) error {
	path := c.tunPath("unassign/" + endpoint)
	var tokenResponse struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodGet, c.ColabDomain, path, nil, &tokenResponse); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, c.ColabDomain, path, nil, nil, tokenResponse.Token)
}

// KeepAlive sends a keep-alive ping for the assigned endpoint.
func (c *ColabServerClient) KeepAlive(ctx context.Context, endpoint string) error {
	path := c.tunPath(endpoint + "/keep-alive/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ColabDomain+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set(HeaderTunnel, "Google")
	response, err := c.roundTrip(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return response.Body.Close()
}

// RefreshProxy obtains a fresh runtime proxy token for the endpoint.
func (c *ColabServerClient) RefreshProxy(ctx context.Context, endpoint string) (RuntimeProxyInfo, error) {
	path := "/v1/runtime-proxy-token?endpoint=" + url.QueryEscape(endpoint) + "&port=8080"
	var token RuntimeProxyInfo
	if err := c.do(ctx, http.MethodGet, c.ColabGapiDomain, path, nil, &token); err != nil {
		return RuntimeProxyInfo{}, err
	}
	if token.Token == "" || token.URL == "" {
		return RuntimeProxyInfo{}, &RemoteError{Kind: ErrorProtocolMismatch, Operation: "refresh proxy", Err: fmt.Errorf("proxy token response missing token or url")}
	}
	return token, nil
}

type CredentialsPropagationResult struct {
	Success                 bool   `json:"success"`
	UnauthorizedRedirectURI string `json:"unauthorized_redirect_uri,omitempty"`
}

// PropagateCredentials delegates credentials (such as dfs_ephemeral for Drive mount)
// to an assigned Colab runtime endpoint.
func (c *ColabServerClient) PropagateCredentials(ctx context.Context, endpoint, authType string, dryRun bool) (CredentialsPropagationResult, error) {
	if authType == "" {
		authType = "dfs_ephemeral"
	}
	dryRunStr := "false"
	if dryRun {
		dryRunStr = "true"
	}
	path := c.tunPath("credentials-propagation/"+endpoint) + fmt.Sprintf("?authtype=%s&version=2&dryrun=%s&propagate=true&record=false", url.QueryEscape(authType), dryRunStr)
	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodGet, c.ColabDomain, path, nil, &tokenResp); err != nil {
		return CredentialsPropagationResult{}, err
	}
	var result CredentialsPropagationResult
	if err := c.do(ctx, http.MethodPost, c.ColabDomain, path, nil, &result, tokenResp.Token); err != nil {
		return CredentialsPropagationResult{}, err
	}
	return result, nil
}

// ListAssignments lists all active runtime assignments on the user's Colab account.
func (c *ColabServerClient) ListAssignments(ctx context.Context) ([]Assignment, error) {
	var resp struct {
		Assignments []Assignment `json:"assignments"`
	}
	if err := c.do(ctx, http.MethodGet, c.ColabGapiDomain, "/v1/assignments", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Assignments, nil
}

// GetUserInfo returns the current user's tier and accelerator eligibility.
func (c *ColabServerClient) GetUserInfo(ctx context.Context) (UserInfo, error) {
	var info UserInfo
	if err := c.do(ctx, http.MethodGet, c.ColabGapiDomain, "/v1/user-info", nil, &info); err != nil {
		return UserInfo{}, err
	}
	return info, nil
}

func (c *ColabServerClient) tunPath(suffix string) string {
	return TunEndpoint + "/" + strings.TrimLeft(suffix, "/")
}

func (c *ColabServerClient) do(ctx context.Context, method, base, path string, payload any, result any, xsrf ...string) error {
	fullPath := path
	if base == c.ColabDomain {
		if strings.Contains(fullPath, "?") {
			if !strings.Contains(fullPath, "authuser=") {
				fullPath += "&authuser=0"
			}
		} else {
			fullPath += "?authuser=0"
		}
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+fullPath, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(HeaderClientAgent, c.ClientAgent)
	if c.AppName != "" {
		req.Header.Set(HeaderVSAppName, c.AppName)
	}
	if c.ExtensionVersion != "" {
		req.Header.Set(HeaderVSExtVersion, c.ExtensionVersion)
	}
	if token, err := c.token(); err != nil {
		return &RemoteError{Kind: ErrorAuthRequired, Operation: "refresh access token", Err: err}
	} else if token != "" {
		req.Header.Set(HeaderAuthorization, "Bearer "+token)
	}
	if len(xsrf) > 0 && xsrf[0] != "" {
		req.Header.Set(HeaderXSRF, xsrf[0])
	}
	response, err := c.roundTrip(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if result == nil {
		return nil
	}
	respBody, err := io.ReadAll(response.Body)
	if err != nil {
		return &RemoteError{Kind: ErrorTransferFailed, Operation: "read Colab response", Err: err}
	}
	// Colab responses carry an XSSI prefix (colab-vscode fetch-utils strips it).
	respBody = stripXSSI(respBody)
	if err := json.Unmarshal(respBody, result); err != nil {
		return &RemoteError{Kind: ErrorProtocolMismatch, Operation: "decode Colab response", Err: err}
	}
	return nil
}

// stripXSSI removes the Colab XSSI prefix ")]}'\n" from a response body.
func stripXSSI(body []byte) []byte {
	const prefix = ")]}'\n"
	if len(body) >= len(prefix) && string(body[:len(prefix)]) == prefix {
		return body[len(prefix):]
	}
	return body
}

func (c *ColabServerClient) roundTrip(req *http.Request) (*http.Response, error) {
	response, err := c.Client.Do(req)
	if err != nil {
		return nil, &RemoteError{Kind: ErrorKernelDisconnected, Operation: "Colab request", Err: err}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		if c.OnAuthError != nil {
			_ = c.OnAuthError()
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		return nil, &RemoteError{Kind: ClassifyHTTPStatus(response.StatusCode), Operation: req.Method + " " + req.URL.Path, StatusCode: response.StatusCode, Err: fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(b)))}
	}
	// Body stays open on success; the caller decodes and closes it.
	return response, nil
}
