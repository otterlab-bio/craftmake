package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/store"
)

func TestRunAutomaticallyRoutesCatalogWorkflowAndUsesCache(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	toolDirectory := filepath.Join(temporaryDirectory, "bin")
	projectDirectory := filepath.Join(temporaryDirectory, "project")
	writeFakeBeaverBSTools(t, toolDirectory)
	writeBeaverBSProjectFixture(t, projectDirectory)

	statePath := filepath.Join(projectDirectory, "workflow", ".craftmake", "state.sqlite")
	commandEnvironment := append(os.Environ(), "PATH="+toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	firstRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--phase", "step1",
		"--catalog", filepath.Join(repositoryRoot, "workflows"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "12",
		"--max-memory", "16G",
	)
	firstRunID := outputValue(t, firstRunOutput, "run_id")
	firstStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", firstRunID)
	if !strings.Contains(firstStatus, "workflow: BeaverBS") || !strings.Contains(firstStatus, "phase: step1") || !strings.Contains(firstStatus, "succeeded: 6") {
		t.Fatalf("unexpected automatically routed run status:\n%s", firstStatus)
	}

	stateStore, err := store.Open(t.Context(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := stateStore.RunSummary(t.Context(), firstRunID)
	stateStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	expectedWorkflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step1.yaml")
	if run.WorkflowPath != expectedWorkflowPath || !filepath.IsAbs(run.ConfigPath) {
		t.Fatalf("unexpected persisted automatic routing paths: %#v", run)
	}

	secondRunOutput := runCraftmake(t, binaryPath, commandEnvironment,
		"run",
		"--legacy-config", "--config", filepath.Join(projectDirectory, "config.yaml"),
		"--phase", "step1",
		"--catalog", filepath.Join(repositoryRoot, "workflows"),
		"--project-dir", projectDirectory,
		"--backend", "local",
		"--max-parallel", "4",
		"--max-cores", "12",
		"--max-memory", "16G",
	)
	secondRunID := outputValue(t, secondRunOutput, "run_id")
	secondStatus := runCraftmake(t, binaryPath, commandEnvironment, "status", "--state", statePath, "--run", secondRunID)
	if !strings.Contains(secondStatus, "status: succeeded") || !strings.Contains(secondStatus, "cached: 6") {
		t.Fatalf("unexpected automatic routing cached status:\n%s", secondStatus)
	}
}
