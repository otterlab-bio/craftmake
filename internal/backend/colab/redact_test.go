package colab

import (
	"errors"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestRedactorScrubsKnownSecrets(t *testing.T) {
	r := NewRedactor("bearer-token-abc", "proxy-secret-xyz", "")
	text := "Authorization: Bearer bearer-token-abc\nX-Colab-Runtime-Proxy-Token: proxy-secret-xyz\nkeep me"
	got := r.Redact(text)
	if strings.Contains(got, "bearer-token-abc") || strings.Contains(got, "proxy-secret-xyz") {
		t.Fatalf("secrets not redacted: %q", got)
	}
	if !strings.Contains(got, RedactionMarker) || !strings.Contains(got, "keep me") {
		t.Fatalf("unexpected redaction result: %q", got)
	}
}

func TestRedactorNilAndEmptyAreNoop(t *testing.T) {
	var nilRedactor *Redactor
	if nilRedactor.Redact("abc") != "abc" {
		t.Fatal("nil redactor should be no-op")
	}
	if NewRedactor().Redact("abc") != "abc" {
		t.Fatal("empty redactor should be no-op")
	}
}

func TestBuildNotebookRedactedScrubsCellSources(t *testing.T) {
	manifest := &protocol.TaskManifest{RunID: "run-1", TaskID: "task-1", Attempt: 1, Steps: []protocol.StepManifest{{Index: 0, Command: "echo $TOKEN", Env: map[string]string{"TOKEN": "super-secret-value"}}}}
	mapping := RemoteTaskMapping{WorkDirectory: "/w", TempDirectory: "/t", RuntimeDirectory: "/r", ResultPath: "/r/result.json"}
	redactor := NewRedactor("super-secret-value")
	nb, err := BuildNotebookRedacted(manifest, mapping, redactor)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, cell := range nb.Cells {
		joined += cell.Source
	}
	if strings.Contains(joined, "super-secret-value") {
		t.Fatalf("secret leaked into notebook: %q", joined)
	}
	if !strings.Contains(joined, RedactionMarker) {
		t.Fatalf("expected redaction marker in notebook: %q", joined)
	}
}

func TestRedactErrorScrubsMessageAndPreservesCause(t *testing.T) {
	redactor := NewRedactor("secret-token")
	base := errors.New("failed with secret-token inside")
	redacted := RedactError(redactor, base)
	if strings.Contains(redacted.Error(), "secret-token") {
		t.Fatalf("secret leaked in error: %q", redacted.Error())
	}
	if !strings.Contains(redacted.Error(), RedactionMarker) {
		t.Fatalf("expected marker in error: %q", redacted.Error())
	}
	if !errors.Is(redacted, base) {
		t.Fatal("expected cause preserved for errors.Is")
	}
}

func TestRedactErrorNilOrNoSecretsIsNoop(t *testing.T) {
	base := errors.New("plain")
	if RedactError(nil, base) != base {
		t.Fatal("nil redactor should return original error")
	}
	if RedactError(NewRedactor(), base) != base {
		t.Fatal("empty redactor should return original error")
	}
}
