package colab

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
)

// fakeMountPreflightResult lets a test inject a pass/fail into CheckMount.
type fakeMountPreflightResult struct{ err error }

func (f fakeMountPreflightResult) CheckMount(context.Context, DriveMountRequest) error { return f.err }

func TestBeginRunMountNotAuthorizedReturnsHint(t *testing.T) {
	b := &Backend{
		Config:         Config{DriveRoot: "/content/drive/MyDrive/project", MountPath: "/content/drive", SessionID: "gpu"},
		Control:        &fakeControlPlane{},
		Executor:       fakeNotebookExecutor{},
		MountPreflight: fakeMountPreflightResult{err: &DriveAuthorizationRequiredError{SessionID: "gpu", RedirectURI: "https://colab.research.google.com/consent"}},
	}
	err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1", ProjectDirectory: "/local/project"})
	if err == nil {
		t.Fatal("expected mount error")
	}
	var mountErr *MountNotAuthorizedError
	if !errors.As(err, &mountErr) {
		t.Fatalf("expected MountNotAuthorizedError, got %T", err)
	}
	if mountErr.SessionID != "gpu" {
		t.Fatalf("unexpected session: %q", mountErr.SessionID)
	}
	if !strings.Contains(err.Error(), "craftmake colab drive mount --session gpu --authorize") {
		t.Fatalf("error missing the real authorization command: %v", err)
	}
}

// TestBeginRunPreflightFailureIsNotReportedAsDriveAuthorization pins the fix
// found by a live run: a credential or control-plane failure during the
// preflight must not be reported as "Drive is not authorized", which sends the
// user chasing the wrong problem.
func TestBeginRunPreflightFailureIsNotReportedAsDriveAuthorization(t *testing.T) {
	credentialErr := errors.New(`Colab OAuth token request (auth-required): token endpoint HTTP 401`)
	b := &Backend{
		Config:         Config{DriveRoot: "/content/drive/MyDrive/project", MountPath: "/content/drive", SessionID: "gpu"},
		Control:        &fakeControlPlane{},
		Executor:       fakeNotebookExecutor{},
		MountPreflight: fakeMountPreflightResult{err: credentialErr},
	}
	err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1", ProjectDirectory: "/local/project"})
	if err == nil {
		t.Fatal("expected the preflight error to surface")
	}
	var mountErr *MountNotAuthorizedError
	if errors.As(err, &mountErr) {
		t.Fatalf("a credential failure must not be classified as a Drive authorization problem: %v", err)
	}
	if !errors.Is(err, credentialErr) {
		t.Fatalf("expected the underlying error to be preserved, got %v", err)
	}
	if strings.Contains(err.Error(), "authorize") {
		t.Fatalf("credential failure should not suggest Drive authorization: %v", err)
	}
}

// TestBeginRunMountNotAuthorizedSurfacesURLAndConfigPath checks that the hint
// is directly actionable: it names the configured auth file and the
// authorization URL returned by the probe.
func TestBeginRunMountNotAuthorizedSurfacesURLAndConfigPath(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "colab-auth.json")
	if err := UpsertSessionAuth(authPath, SessionAuth{SessionID: "gpu", DriveRoot: "/content/drive/MyDrive/project", MountPath: "/content/drive", ColabCredentialFile: "/tmp/colab.json", DriveCredentialFile: "/tmp/drive.json"}); err != nil {
		t.Fatal(err)
	}
	b := &Backend{
		Config:         Config{DriveRoot: "/content/drive/MyDrive/project", MountPath: "/content/drive", SessionID: "gpu", AuthConfigPath: authPath},
		Control:        &fakeControlPlane{},
		Executor:       fakeNotebookExecutor{},
		MountPreflight: fakeMountPreflightResult{err: &DriveAuthorizationRequiredError{SessionID: "gpu", RedirectURI: "https://colab.research.google.com/consent?x=1"}},
	}
	err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1", ProjectDirectory: "/local/project"})
	if err == nil {
		t.Fatal("expected mount error")
	}
	message := err.Error()
	for _, want := range []string{
		"https://colab.research.google.com/consent?x=1",
		"craftmake colab drive mount --config " + authPath + " --session gpu --authorize",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q missing %q", message, want)
		}
	}
}

func TestBeginRunMountAuthorizedProceeds(t *testing.T) {
	control := &fakeControlPlane{}
	b := &Backend{
		Config:         Config{DriveRoot: "/content/drive/MyDrive/project", MountPath: "/content/drive", SessionID: "gpu"},
		Control:        control,
		Executor:       fakeNotebookExecutor{},
		MountPreflight: fakeMountPreflightResult{err: nil},
	}
	if err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1", ProjectDirectory: "/local/project"}); err != nil {
		t.Fatal(err)
	}
	if control.acquired != 0 {
		t.Fatalf("BeginRun must not acquire a runtime, got %d", control.acquired)
	}
}
