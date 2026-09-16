package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCommandEnvelopeRoundTrip(t *testing.T) {
	payloadBytes, err := json.Marshal(ValidatePayload{
		Workflow:        "BeaverBS",
		Phase:           "step1",
		TaskCount:       7,
		SubmissionCount: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := NewCommandEnvelope(
		"validate",
		true,
		"run-20260726T013245Z-kxqjrm",
		"/run/state/state.sqlite",
		"/run/logs/controller.jsonl",
		payloadBytes,
	)
	var output bytes.Buffer
	if err := WriteCommandEnvelope(&output, envelope, false); err != nil {
		t.Fatal(err)
	}

	var decoded CommandEnvelope
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ProtocolVersion != CommandProtocolVersion || decoded.Command != "validate" || !decoded.OK {
		t.Fatalf("unexpected command envelope: %#v", decoded)
	}
	if decoded.RunID != envelope.RunID || decoded.StatePath != envelope.StatePath || decoded.ControllerLog != envelope.ControllerLog {
		t.Fatalf("envelope metadata did not round trip: %#v", decoded)
	}
	var payload ValidatePayload
	if err := json.Unmarshal(decoded.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Workflow != "BeaverBS" || payload.TaskCount != 7 || payload.SubmissionCount != 7 {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}
