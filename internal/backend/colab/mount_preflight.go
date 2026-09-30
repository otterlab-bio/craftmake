package colab

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Drive authorization preflight modes. Colab binds a Drive credential grant to
// the runtime that requested it, so a fresh ephemeral runtime reports the
// credential as unauthorized again; pre-authorizing for a later run is not
// possible. The default is therefore advisory.
const (
	// DrivePreflightNotify probes the grant, warns when it is missing and lets
	// the run continue so the runtime-side consent prompt can complete it.
	DrivePreflightNotify = "notify"
	// DrivePreflightStrict fails the run before any task when Drive is not
	// authorized, which suits unattended pipelines that must not block.
	DrivePreflightStrict = "strict"
	// DrivePreflightOff skips the probe entirely.
	DrivePreflightOff = "off"
)

// ProbeReleaseTimeout bounds the best-effort release of the preflight's probe
// runtime so a stalled control plane cannot hang a run that already failed.
const probeReleaseTimeout = 15 * time.Second

// DriveAuthorizationRequiredError reports that the Colab account behind a
// session has not granted Google Drive access yet. It carries the authorization
// URL returned by Colab so callers can surface it to the user instead of
// blocking the kernel on input later.
type DriveAuthorizationRequiredError struct {
	SessionID   string
	RedirectURI string
}

func (e *DriveAuthorizationRequiredError) Error() string {
	if e.RedirectURI == "" {
		return fmt.Sprintf("Google Drive authorization is required for session %q", e.SessionID)
	}
	return fmt.Sprintf("Google Drive authorization is required for session %q; open %s to grant access", e.SessionID, e.RedirectURI)
}

// NormalizeDrivePreflightMode maps operator input onto a known mode. Unknown or
// empty values become the advisory default.
func NormalizeDrivePreflightMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case DrivePreflightOff, "skip", "disabled", "false", "0":
		return DrivePreflightOff
	case DrivePreflightStrict, "fail", "true", "1":
		return DrivePreflightStrict
	default:
		return DrivePreflightNotify
	}
}

// ServerMountPreflight is the production DriveMountPreflight. It performs the
// real authorization check against the Colab control plane: acquire a
// short-lived probe runtime, ask Colab whether the account may propagate Drive
// credentials (`dfs_ephemeral` dry run), release the runtime, and report the
// authorization as missing when the probe does not succeed.
//
// Colab stores the Drive grant per Google account, so after the first
// authorization the probe succeeds silently and no user interaction is needed;
// that is exactly the "authorize once" contract this preflight enforces.
type ServerMountPreflight struct {
	Client *ColabServerClient
	Spec   RuntimeSpec
}

func (p *ServerMountPreflight) CheckMount(ctx context.Context, request DriveMountRequest) error {
	if p.Client == nil {
		return fmt.Errorf("Colab server client is required for the drive mount preflight")
	}
	if request.SessionID == "" {
		return fmt.Errorf("drive mount preflight requires a session id")
	}
	spec := p.Spec
	if spec.NotebookHash == "" {
		// A stable hash keeps the probe target identical across runs.
		spec.NotebookHash = NotebookHash("drive-preflight-" + request.SessionID)
	}
	control := &ServerControlPlane{Client: p.Client, Spec: spec}
	runtime, err := control.AcquireRuntime(ctx, RuntimeRequest{RunID: "drive-preflight-" + request.SessionID, Accelerator: "cpu"})
	if err != nil {
		return fmt.Errorf("acquire probe runtime: %w", err)
	}
	// Release the probe runtime on every path, including unauthorized, so a
	// failed preflight never leaks an assignment.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), probeReleaseTimeout)
		defer cancel()
		_ = control.ReleaseRuntime(releaseCtx, runtime)
	}()

	probe, err := p.Client.PropagateCredentials(ctx, runtime.ID, "dfs_ephemeral", true)
	if err != nil {
		return fmt.Errorf("check Drive credentials: %w", err)
	}
	if probe.Success {
		return nil
	}
	return &DriveAuthorizationRequiredError{SessionID: request.SessionID, RedirectURI: probe.UnauthorizedRedirectURI}
}

var _ DriveMountPreflight = (*ServerMountPreflight)(nil)
