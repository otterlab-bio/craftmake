package colab

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyHTTPStatusAndRemoteError(t *testing.T) {
	if ClassifyHTTPStatus(http.StatusUnauthorized) != ErrorAuthRequired {
		t.Fatal("401 should require auth")
	}
	if ClassifyHTTPStatus(http.StatusTooManyRequests) != ErrorRuntimeUnavailable {
		t.Fatal("429 should indicate runtime unavailability")
	}
	if ClassifyHTTPStatus(http.StatusBadGateway) != ErrorTransferFailed {
		t.Fatal("502 should indicate transfer failure")
	}
	base := errors.New("expired")
	err := &RemoteError{Kind: ErrorKernelDisconnected, Operation: "execute cell", Err: base}
	if !errors.Is(err, base) || !strings.Contains(err.Error(), "execute cell") {
		t.Fatalf("unexpected remote error: %v", err)
	}
}
