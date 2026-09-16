package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/internal/store"
)

func TestExportEvidenceBundleIncludesPersistedIncidents(t *testing.T) {
	stateStore, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	observedAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if err := stateStore.CreateRun(t.Context(), store.Run{
		ID: "evidence-run", Workflow: "BeaverBS", Phase: "step2", Backend: "slurm", Status: "failed", StartedAt: observedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.UpsertTask(t.Context(), store.TaskInstance{
		RunID: "evidence-run", TaskID: "align", JobID: "align-job", Dimensions: map[string]string{}, Inputs: map[string][]string{}, Outputs: map[string]string{}, Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.CreateAttempt(t.Context(), store.TaskAttempt{
		ID: "evidence-attempt", RunID: "evidence-run", TaskID: "align", AttemptNumber: 1, Status: "failed", StartedAt: &observedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.SaveRuntimeIncident(t.Context(), store.RuntimeIncident{
		ID: "evidence-incident", RunID: "evidence-run", AttemptID: "evidence-attempt", SchemaVersion: "otter.runtime-incident/v1",
		Category: "tool_invocation", Scope: "step", RetryPolicy: "manual", Owner: "workflow", Escalation: "inspect logs",
		RemediationStatus: "open", Summary: "tool failed", FirstObservedAt: observedAt, Backend: "slurm",
	}); err != nil {
		t.Fatal(err)
	}

	outputDirectory := filepath.Join(t.TempDir(), "report")
	if err := ExportCSV(t.Context(), stateStore, "evidence-run", outputDirectory); err != nil {
		t.Fatal(err)
	}
	bundlePath, err := ExportEvidenceBundle(t.Context(), stateStore, "evidence-run", outputDirectory, "/logs/controller.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	bundleData, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	var bundle EvidenceBundle
	if err := json.Unmarshal(bundleData, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.SchemaVersion != "craftmake.run-evidence/v1" || bundle.TaskStatusCounts["failed"] != 1 || len(bundle.Incidents) != 1 {
		t.Fatalf("unexpected evidence bundle: %#v", bundle)
	}
}
