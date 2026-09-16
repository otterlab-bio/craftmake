package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeIncidentRoundTrip(t *testing.T) {
	stateStore, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	observedAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if err := stateStore.CreateRun(t.Context(), Run{
		ID: "incident-run", Workflow: "BeaverBS", Phase: "step2", Backend: "slurm", Status: "running", StartedAt: observedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.UpsertTask(t.Context(), TaskInstance{
		RunID: "incident-run", TaskID: "map", JobID: "map-job", Dimensions: map[string]string{}, Inputs: map[string][]string{}, Outputs: map[string]string{}, Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.CreateAttempt(t.Context(), TaskAttempt{
		ID: "incident-attempt", RunID: "incident-run", TaskID: "map", AttemptNumber: 1, Status: "failed", StartedAt: &observedAt,
	}); err != nil {
		t.Fatal(err)
	}
	exitCode := 137
	if err := stateStore.SaveRuntimeIncident(t.Context(), RuntimeIncident{
		ID: "incident-attempt-incident", RunID: "incident-run", AttemptID: "incident-attempt",
		SchemaVersion: "otter.runtime-incident/v1", Category: "resource_exhaustion", Scope: "task",
		RetryPolicy: "manual", Owner: "workflow", Escalation: "review allocation", RemediationStatus: "open",
		Summary: "out of memory", FirstObservedAt: observedAt, Backend: "slurm", BackendJobID: "12345", ExitCode: &exitCode,
		DiagnosticPaths: []string{"/logs/stderr.log"}, EvidencePaths: []string{"/runtime/result.json"},
	}); err != nil {
		t.Fatal(err)
	}

	incidents, err := stateStore.ListRuntimeIncidents(t.Context(), "incident-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("expected one incident, got %#v", incidents)
	}
	incident := incidents[0]
	if incident.Category != "resource_exhaustion" || incident.ExitCode == nil || *incident.ExitCode != 137 || incident.BackendJobID != "12345" {
		t.Fatalf("unexpected persisted incident: %#v", incident)
	}
	if len(incident.DiagnosticPaths) != 1 || len(incident.EvidencePaths) != 1 {
		t.Fatalf("incident evidence paths were not retained: %#v", incident)
	}
}
