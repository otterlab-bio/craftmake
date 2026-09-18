package colab

import (
	"strings"
)

// RedactionMarker is the replacement for any detected secret.
const RedactionMarker = "[REDACTED]"

// Redactor scrubs known secret values from arbitrary text so they never leak
// into notebooks, controller logs, TaskResult payloads, or error messages.
type Redactor struct {
	secrets []string
}

// NewRedactor builds a Redactor from the given secret values. Empty values are
// ignored.
func NewRedactor(secrets ...string) *Redactor {
	seen := make(map[string]bool)
	for _, s := range secrets {
		if strings.TrimSpace(s) != "" {
			seen[s] = true
		}
	}
	result := &Redactor{secrets: make([]string, 0, len(seen))}
	for s := range seen {
		result.secrets = append(result.secrets, s)
	}
	return result
}

// Redact replaces every occurrence of a known secret with the redaction marker.
func (r *Redactor) Redact(text string) string {
	if r == nil || len(r.secrets) == 0 {
		return text
	}
	for _, secret := range r.secrets {
		text = strings.ReplaceAll(text, secret, RedactionMarker)
	}
	return text
}

// HasSecrets reports whether the redactor holds any secrets.
func (r *Redactor) HasSecrets() bool { return r != nil && len(r.secrets) > 0 }

// RedactError returns an error whose message has known secrets scrubbed. It
// preserves the original error identity for errors.As/Is when possible by
// wrapping the original error.
func RedactError(redactor *Redactor, err error) error {
	if err == nil || redactor == nil || !redactor.HasSecrets() {
		return err
	}
	return &redactedError{message: redactor.Redact(err.Error()), cause: err}
}

// redactedError wraps an error with a redacted message while preserving the
// original cause for unwrapping.
type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }
