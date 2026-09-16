package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/store"
)

func TestValidatePersistedRunDigestsRejectsDrift(t *testing.T) {
	temporaryDirectory := t.TempDir()
	configPath := filepath.Join(temporaryDirectory, "run.yaml")
	workflowPath := filepath.Join(temporaryDirectory, "workflow.yaml")
	if err := os.WriteFile(configPath, []byte("run: original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("workflow: original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digests, err := calculatePlanDigests(configPath, workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{ID: "run-20260726T013245Z-kxqjrm", ConfigPath: configPath, ConfigDigest: digests.Config, WorkflowPath: workflowPath, WorkflowDigest: digests.Workflow}
	if _, err := validatePersistedRunDigests(run); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("run: modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := validatePersistedRunDigests(run); err == nil {
		t.Fatal("expected config digest drift to be rejected")
	}
}
