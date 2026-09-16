package cli_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestCraftmakeBinaryExitCodeContract(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	smokeConfigurationPath := filepath.Join(repositoryRoot, "testdata", "configs", "smoke.yaml")
	smokeWorkflowPath := filepath.Join(repositoryRoot, "testdata", "workflows", "smoke.yaml")
	compilationFailureWorkflowPath := filepath.Join(temporaryDirectory, "compilation-failure.yaml")
	writeTextFile(t, compilationFailureWorkflowPath, `name: Invalid resource workflow
version: 1
on:
  otter:
    workflow: BeaverBS
    phase: step1
    modes: [RRBS]
jobs:
  invalid_resources:
    scope: global
    outputs:
      result: output/invalid-resource.txt
    resources:
      cores: 1
      memory: definitely-not-memory
    steps:
      - run: printf 'unreachable\n'
`)

	legacyTriggerWorkflowPath := filepath.Join(temporaryDirectory, "legacy-trigger.yaml")
	writeTextFile(t, legacyTriggerWorkflowPath, `name: Legacy trigger workflow
version: 1
on:
  xdxtools:
    workflow: BeaverBS
    phase: step1
    modes: [RRBS]
jobs:
  unreachable:
    scope: global
    resources:
      cores: 1
      memory: 32M
    steps:
      - run: printf 'unreachable\n'
`)

	taskFailureManifestPath := writeTaskFailureManifest(t, temporaryDirectory)
	missingStatePath := filepath.Join(temporaryDirectory, "missing-state-parent", "state.sqlite")

	testCases := []struct {
		name             string
		expectedExitCode int
		expectedOutput   string
		environment      []string
		arguments        []string
	}{
		{
			name:             "success",
			expectedExitCode: 0,
			expectedOutput:   "local: ok",
			arguments:        []string{"doctor", "--backend", "local"},
		},
		{
			name:             "usage error",
			expectedExitCode: 2,
			expectedOutput:   "unknown command",
			arguments:        []string{"not-a-command"},
		},
		{
			name:             "configuration error",
			expectedExitCode: 3,
			expectedOutput:   "read otter config",
			arguments: []string{
				"validate",
				"--legacy-config", "--config", filepath.Join(temporaryDirectory, "missing-config.yaml"),
				"--workflow", smokeWorkflowPath,
			},
		},
		{
			name:             "legacy workflow trigger rejected",
			expectedExitCode: 3,
			expectedOutput:   "on.otter.workflow and on.otter.phase are required",
			arguments: []string{
				"validate",
				"--legacy-config", "--config", smokeConfigurationPath,
				"--workflow", legacyTriggerWorkflowPath,
			},
		},
		{
			name:             "compilation error",
			expectedExitCode: 4,
			expectedOutput:   "definitely-not-memory",
			arguments: []string{
				"validate",
				"--legacy-config", "--config", smokeConfigurationPath,
				"--workflow", compilationFailureWorkflowPath,
			},
		},
		{
			name:             "task failure",
			expectedExitCode: 5,
			expectedOutput:   "task exit-contract/task-failure failed",
			arguments:        []string{"__task-runner", "--manifest", taskFailureManifestPath},
		},
		{
			name:             "backend failure",
			expectedExitCode: 6,
			expectedOutput:   "missing Slurm commands",
			environment:      []string{"PATH="},
			arguments:        []string{"doctor", "--backend", "slurm"},
		},
		{
			name:             "state failure",
			expectedExitCode: 7,
			expectedOutput:   "state database",
			arguments:        []string{"status", "--state", missingStatePath, "--run", "missing-run"},
		},
		{
			name:             "internal failure",
			expectedExitCode: 9,
			expectedOutput:   "read task manifest",
			arguments: []string{
				"__task-runner",
				"--manifest", filepath.Join(temporaryDirectory, "missing-manifest.json"),
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			commandEnvironment := os.Environ()
			if testCase.environment != nil {
				commandEnvironment = append(commandEnvironment, testCase.environment...)
			}
			exitCode, output := executeCraftmakeForExitCode(t, binaryPath, commandEnvironment, testCase.arguments...)
			if exitCode != testCase.expectedExitCode {
				t.Fatalf("unexpected exit code for craftmake %s: got %d, expected %d\n%s", strings.Join(testCase.arguments, " "), exitCode, testCase.expectedExitCode, output)
			}
			if !strings.Contains(output, testCase.expectedOutput) {
				t.Fatalf("output for craftmake %s does not contain %q:\n%s", strings.Join(testCase.arguments, " "), testCase.expectedOutput, output)
			}
		})
	}
}

func TestCraftmakeBinaryReturnsCancelledExitCodeAfterTerminationSignal(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	workflowPath := filepath.Join(temporaryDirectory, "cancellation.yaml")
	writeTextFile(t, workflowPath, `name: Cancellation workflow
version: 1
on:
  otter:
    workflow: BeaverBS
    phase: step1
    modes: [RRBS]
defaults:
  shell: bash
jobs:
  wait:
    scope: global
    outputs:
      result: output/cancellation-finished.txt
    resources:
      cores: 1
      memory: 32M
    steps:
      - name: Wait for cancellation
        run: |
          sleep 30
          mkdir -p output
          printf 'finished\n' > output/cancellation-finished.txt
`)

	projectDirectory := filepath.Join(temporaryDirectory, "project")
	stateDirectory := filepath.Join(projectDirectory, "state")
	if err := os.MkdirAll(projectDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	runID := "exit-contract-cancelled"
	command := exec.Command(
		binaryPath,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", filepath.Join(repositoryRoot, "testdata", "configs", "smoke.yaml"),
		"--project-dir", projectDirectory,
		"--state-dir", stateDirectory,
		"--run-id", runID,
		"--backend", "local",
		"--max-parallel", "1",
		"--max-cores", "1",
	)
	command.Env = os.Environ()
	var commandOutput strings.Builder
	command.Stdout = &commandOutput
	command.Stderr = &commandOutput
	if err := command.Start(); err != nil {
		t.Fatalf("start cancellable craftmake run: %v", err)
	}
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- command.Wait()
	}()

	statePath := filepath.Join(stateDirectory, "state.sqlite")
	waitForRunningCraftmakeRun(t, binaryPath, statePath, runID, command, waitResult, &commandOutput)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		_ = command.Process.Kill()
		<-waitResult
		t.Fatalf("signal craftmake process: %v", err)
	}

	select {
	case waitErr := <-waitResult:
		exitCode := processExitCode(waitErr)
		if exitCode != 8 {
			t.Fatalf("unexpected cancellation exit code: got %d, expected 8\n%s", exitCode, commandOutput.String())
		}
		if !strings.Contains(commandOutput.String(), "context canceled") {
			t.Fatalf("cancellation output does not report context cancellation:\n%s", commandOutput.String())
		}
		controllerLogPath := filepath.Join(stateDirectory, "runs", runID, "controller.jsonl")
		controllerLogData, readErr := os.ReadFile(controllerLogPath)
		if readErr != nil {
			t.Fatalf("read cancellation controller log: %v", readErr)
		}
		if !strings.Contains(string(controllerLogData), `"event":"run.cancellation_requested"`) ||
			!strings.Contains(string(controllerLogData), `"event":"run.finished"`) ||
			!strings.Contains(string(controllerLogData), `"status":"cancelled"`) {
			t.Fatalf("cancellation controller log is incomplete:\n%s", controllerLogData)
		}
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		<-waitResult
		t.Fatalf("craftmake did not exit after termination signal:\n%s", commandOutput.String())
	}
}

func writeTaskFailureManifest(t *testing.T, parentDirectory string) string {
	t.Helper()
	runtimeDirectory := filepath.Join(parentDirectory, "task-runtime")
	manifest := protocol.TaskManifest{
		ProtocolVersion:  protocol.Version,
		RunID:            "exit-contract",
		TaskID:           "exit-contract/task-failure",
		JobID:            "task-failure",
		Attempt:          1,
		Workflow:         "Smoke",
		Phase:            "step1",
		Scope:            "global",
		WorkDirectory:    parentDirectory,
		TempDirectory:    filepath.Join(runtimeDirectory, "temp"),
		RuntimeDirectory: runtimeDirectory,
		ResultPath:       filepath.Join(runtimeDirectory, "result.json"),
		Steps: []protocol.StepManifest{
			{
				Index:   1,
				Name:    "Fail deliberately",
				Shell:   "bash",
				Command: "exit 23",
			},
		},
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(parentDirectory, "task-failure-manifest.json")
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	return manifestPath
}

func writeTextFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func executeCraftmakeForExitCode(t *testing.T, binaryPath string, environment []string, arguments ...string) (int, string) {
	t.Helper()
	command := exec.Command(binaryPath, arguments...)
	command.Env = environment
	output, err := command.CombinedOutput()
	return processExitCode(err), string(output)
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

func waitForRunningCraftmakeRun(t *testing.T, binaryPath, statePath, runID string, runningCommand *exec.Cmd, waitResult <-chan error, commandOutput *strings.Builder) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastStatusOutput string
	for time.Now().Before(deadline) {
		select {
		case waitErr := <-waitResult:
			t.Fatalf("craftmake exited before it could be cancelled with code %d:\n%s", processExitCode(waitErr), commandOutput.String())
		default:
		}
		if _, err := os.Stat(statePath); err == nil {
			statusCommand := exec.Command(binaryPath, "status", "--state", statePath, "--run", runID)
			statusCommand.Env = os.Environ()
			statusOutput, statusErr := statusCommand.CombinedOutput()
			lastStatusOutput = string(statusOutput)
			if statusErr == nil && strings.Contains(lastStatusOutput, "status: running") {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = runningCommand.Process.Kill()
	<-waitResult
	t.Fatalf("timed out waiting for craftmake run %q to start; last status output:\n%s\ncommand output:\n%s", runID, lastStatusOutput, commandOutput.String())
}
