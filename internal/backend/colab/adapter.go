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

// ServerControlPlane adapts the reference-informed ColabServerClient to the
// Backend ControlPlane seam by mapping acquire/release onto assign/unassign.
type ServerControlPlane struct {
	Client *ColabServerClient
	Spec   RuntimeSpec
}

func (c *ServerControlPlane) AcquireRuntime(ctx context.Context, request RuntimeRequest) (Runtime, error) {
	if c.Client == nil {
		return Runtime{}, fmt.Errorf("Colab server client is required")
	}
	spec := c.Spec
	if spec.NotebookHash == "" {
		spec.NotebookHash = notebookHash(request.RunID)
	}
	// Map the abstract accelerator type ("cpu"/"gpu"/"tpu") to the Colab
	// variant and accelerator parameters. Colab API requires variant=GPU
	// combined with accelerator=T4 for standard GPU machines; variant=GPU alone
	// is rejected with HTTP 400. "cpu" maps to empty variant/accelerator (DEFAULT).
	switch request.Accelerator {
	case "gpu":
		spec.Variant = "GPU"
		spec.Accelerator = "T4"
	case "tpu":
		spec.Variant = "TPU"
	case "cpu", "":
		spec.Variant = ""
		spec.Accelerator = ""
	default:
		spec.Variant = "GPU"
		spec.Accelerator = request.Accelerator
	}
	assignment, err := c.Client.Assign(ctx, spec)
	if err != nil {
		if remote, ok := err.(*RemoteError); ok && remote.StatusCode == http.StatusPreconditionFailed {
			// Auto-clean dangling assignments on Colab 412 (TooManyAssignmentsError)
			if assignments, listErr := c.Client.ListAssignments(ctx); listErr == nil && len(assignments) > 0 {
				for _, a := range assignments {
					_ = c.Client.Unassign(ctx, a.Endpoint)
				}
				assignment, err = c.Client.Assign(ctx, spec)
			}
		}
		if err != nil {
			return Runtime{}, err
		}
	}
	return Runtime{ID: assignment.Endpoint, ProxyURL: assignment.RuntimeProxyInfo.URL, ProxyToken: assignment.RuntimeProxyInfo.Token}, nil
}

func (c *ServerControlPlane) ReleaseRuntime(ctx context.Context, runtime Runtime) error {
	if c.Client == nil {
		return fmt.Errorf("Colab server client is required")
	}
	return c.Client.Unassign(ctx, runtime.ID)
}

func (c *ServerControlPlane) ListAssignments(ctx context.Context) ([]Assignment, error) {
	if c.Client == nil {
		return nil, fmt.Errorf("Colab server client is required")
	}
	return c.Client.ListAssignments(ctx)
}

// ProxyNotebookExecutor executes a notebook through the Colab runtime proxy,
// sending the runtime proxy token in the X-Colab-Runtime-Proxy-Token header.
type ProxyNotebookExecutor struct {
	Client      HTTPDoer
	BearerToken string
}

func (e *ProxyNotebookExecutor) ExecuteNotebook(ctx context.Context, runtime Runtime, notebook []byte) (string, error) {
	proxyURL := runtime.ProxyURL
	if proxyURL == "" {
		proxyURL = runtime.ID
	}
	if proxyURL == "" {
		return "", fmt.Errorf("runtime proxy endpoint is required")
	}
	target, err := url.Parse(proxyURL)
	if err != nil {
		return "", &RemoteError{Kind: ErrorProtocolMismatch, Operation: "parse proxy URL", Err: err}
	}
	target.Path = "/kernel/execute"
	if target.Scheme == "" {
		return "", fmt.Errorf("runtime proxy endpoint is not a URL")
	}
	data, err := json.Marshal(struct {
		Notebook []byte `json:"notebook"`
	}{Notebook: notebook})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderProxyToken, e.BearerToken)
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return "", &RemoteError{Kind: ErrorKernelDisconnected, Operation: "execute proxy notebook", Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", &RemoteError{Kind: ClassifyHTTPStatus(response.StatusCode), Operation: "execute proxy notebook", StatusCode: response.StatusCode, Err: fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(b)))}
	}
	output, err := io.ReadAll(response.Body)
	if err != nil {
		return "", &RemoteError{Kind: ErrorTransferFailed, Operation: "read proxy notebook output", Err: err}
	}
	return string(output), nil
}

var _ ControlPlane = (*ServerControlPlane)(nil)
var _ NotebookExecutor = (*ProxyNotebookExecutor)(nil)
