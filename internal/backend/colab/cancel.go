package colab

import (
	"context"
	"errors"
	"time"

	"github.com/otterlab-bio/craftmake/internal/backend"
)

// KernelGracePeriod is how long CancelSubmission waits for an in-flight
// notebook execution to settle after issuing an interrupt.
const KernelGracePeriod = 5 * time.Second

// NotebookInterruptor is an optional capability that lets the backend request
// an in-flight notebook execution stop. The concrete executor implements it;
// when absent, CancelSubmission falls back to the graceful-wait path only.
type NotebookInterruptor interface {
	Interrupt(context.Context, Runtime) error
}

// errRemoteGone is wrapped by cancel paths when the runtime is already gone;
// callers treat it as an idempotent success so a 404 never overwrites the
// local cancelled state.
var errRemoteGone = errors.New("remote runtime is already gone")

// isRemoteGone reports whether err indicates the runtime no longer exists
// (for example a 404 from the Colab control plane during unassign).
func isRemoteGone(err error) bool {
	var remoteError *RemoteError
	if errors.As(err, &remoteError) && remoteError.StatusCode == 404 {
		return true
	}
	return errors.Is(err, errRemoteGone)
}

// CancelSubmission interrupts the currently executing kernel and waits a
// bounded grace period for it to settle. In the ephemeral-instance model each
// manifest runs on its own short-lived runtime, so there is no persistent
// runtime to interrupt here; the executor's interrupt path is exercised during
// RunSubmission. This is an idempotent no-op.
func (b *Backend) CancelSubmission(ctx context.Context, submissionID string, metadata map[string]any) error {
	return nil
}

// Cancel stops the run: it defensively releases any residual assignments,
// treating a missing remote (404) as an idempotent success.
func (b *Backend) Cancel(ctx context.Context) error {
	if b.Control == nil {
		return nil
	}
	assignments, err := b.Control.ListAssignments(ctx)
	if err != nil {
		return err
	}
	for _, a := range assignments {
		if releaseErr := b.Control.ReleaseRuntime(ctx, Runtime{ID: a.Endpoint}); releaseErr != nil && !isRemoteGone(releaseErr) {
			return releaseErr
		}
	}
	return nil
}

var _ backend.Backend = (*Backend)(nil)
