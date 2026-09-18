package colab

import (
	"fmt"
	"net/http"
)

type ErrorKind string

const (
	ErrorAuthRequired       ErrorKind = "auth-required"
	ErrorRuntimeUnavailable ErrorKind = "runtime-unavailable"
	ErrorKernelDisconnected ErrorKind = "kernel-disconnected"
	ErrorTransferFailed     ErrorKind = "transfer-failed"
	ErrorProtocolMismatch   ErrorKind = "protocol-mismatch"
	ErrorMountNotAuthorized ErrorKind = "mount-not-authorized"
)

// MountNotAuthorizedError reports that the Drive mount preflight failed because
// the runtime is not authorized to mount Drive. It carries an actionable hint
// referencing the named session and the one-time authorization command, so the
// user can authorize before retrying instead of blocking the kernel on input.
type MountNotAuthorizedError struct {
	SessionID      string
	AuthConfigPath string
	MountPath      string
	DriveRoot      string
	Err            error
}

func (e *MountNotAuthorizedError) Error() string {
	return fmt.Sprintf("Drive mount is not authorized for session %q: %v; run `craftmake colab drive authorize --session %s` to authorize once", e.SessionID, e.Err, e.SessionID)
}
func (e *MountNotAuthorizedError) Unwrap() error { return e.Err }

type RemoteError struct {
	Kind       ErrorKind
	Operation  string
	StatusCode int
	Err        error
}

func (e *RemoteError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("Colab %s (%s)", e.Operation, e.Kind)
	}
	return fmt.Sprintf("Colab %s (%s): %v", e.Operation, e.Kind, e.Err)
}
func (e *RemoteError) Unwrap() error { return e.Err }

func ClassifyHTTPStatus(status int) ErrorKind {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrorAuthRequired
	case http.StatusPreconditionFailed, http.StatusServiceUnavailable, http.StatusTooManyRequests:
		return ErrorRuntimeUnavailable
	default:
		return ErrorTransferFailed
	}
}
