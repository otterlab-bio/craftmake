//go:build linux

package cli_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/internal/processgroup"
	"github.com/otterlab-bio/craftmake/internal/store"
)

func TestCraftmakeResumeRecoversLocalOrphanAndReusesSuccessfulTask(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	projectDirectory := filepath.Join(temporaryDirectory, "project")
	stateDirectory := filepath.Join(projectDirectory, "state")
	if err := os.MkdirAll(projectDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	configurationData, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "configs", "smoke.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	configurationPath := filepath.Join(projectDirectory, "config.yaml")
	if err := os.WriteFile(configurationPath, configurationData, 0o644); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(projectDirectory, "resume-workflow.yaml")
	writeTextFile(t, workflowPath, `name: Resume integration workflow
version: 1
on:
  otter:
    workflow: BeaverBS
    phase: step1
    modes: [RRBS]
defaults:
  shell: bash
jobs:
  prepare:
    scope: global
    outputs:
      result: output/prepare.txt
    resources:
      cores: 1
      memory: 32M
    steps:
      - name: Prepare cacheable output
        run: |
          mkdir -p output
          printf 'prepared\n' > '${{ outputs.result }}'
  wait_for_resume:
    scope: global
    needs: [prepare]
    inputs:
      prepared: '${{ jobs.prepare.outputs.result }}'
    outputs:
      result: output/resumed.txt
    resources:
      cores: 1
      memory: 32M
    steps:
      - name: Wait for controller recovery
        run: |
          mkdir -p output
          if [ ! -e output/wait-for-resume.started ]; then
            printf 'started\n' > output/wait-for-resume.started
            sleep 30
          fi
          cat '${{ inputs.prepared }}' > '${{ outputs.result }}'
`)

	const sourceRunID = "resume-source-run"
	runCommand := exec.Command(
		binaryPath,
		"run",
		"--workflow", workflowPath,
		"--legacy-config", "--config", configurationPath,
		"--project-dir", projectDirectory,
		"--state-dir", stateDirectory,
		"--run-id", sourceRunID,
		"--backend", "local",
		"--max-parallel", "1",
		"--max-cores", "1",
	)
	runCommand.Env = os.Environ()
	var runOutput strings.Builder
	runCommand.Stdout = &runOutput
	runCommand.Stderr = &runOutput
	if err := runCommand.Start(); err != nil {
		t.Fatalf("start source craftmake run: %v", err)
	}
	runWaitResult := make(chan error, 1)
	go func() {
		runWaitResult <- runCommand.Wait()
	}()
	controllerWaited := false
	orphanProcessGroupID := 0
	t.Cleanup(func() {
		if !controllerWaited {
			_ = runCommand.Process.Kill()
			select {
			case <-runWaitResult:
			case <-time.After(3 * time.Second):
			}
		}
		if processgroup.GroupExists(orphanProcessGroupID) {
			_ = processgroup.TerminateGroup(orphanProcessGroupID)
			cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancelCleanup()
			_ = processgroup.WaitForGroupExit(cleanupContext, orphanProcessGroupID)
		}
	})

	statePath := filepath.Join(stateDirectory, "state.sqlite")
	orphanProcessGroupID = waitForResumeOrphan(t, binaryPath, statePath, sourceRunID, projectDirectory, stateDirectory)
	if err := runCommand.Process.Kill(); err != nil {
		t.Fatalf("abruptly terminate source controller: %v", err)
	}
	select {
	case <-runWaitResult:
		controllerWaited = true
	case <-time.After(5 * time.Second):
		t.Fatal("source controller did not exit after abrupt termination")
	}
	if !processgroup.GroupExists(orphanProcessGroupID) {
		t.Fatalf("expected task process group %d to survive controller termination", orphanProcessGroupID)
	}

	resumeOutput := runCraftmake(
		t,
		binaryPath,
		os.Environ(),
		"resume",
		"--legacy-config",
		"--state", statePath,
		"--run", sourceRunID,
		"--max-parallel", "1",
		"--max-cores", "1",
	)
	if !strings.Contains(resumeOutput, "recovered_run: "+sourceRunID) || !strings.Contains(resumeOutput, "recovered_status: failed") {
		t.Fatalf("resume output does not describe source recovery:\n%s", resumeOutput)
	}
	if !strings.Contains(resumeOutput, "resumed_from: "+sourceRunID) {
		t.Fatalf("resume output does not contain source lineage:\n%s", resumeOutput)
	}
	resumedRunID := outputValue(t, resumeOutput, "run_id")

	resumedStatus := runCraftmake(t, binaryPath, os.Environ(), "status", "--state", statePath, "--run", resumedRunID)
	if !strings.Contains(resumedStatus, "status: succeeded") || !strings.Contains(resumedStatus, "cached: 1") || !strings.Contains(resumedStatus, "succeeded: 1") {
		t.Fatalf("unexpected resumed run status:\n%s", resumedStatus)
	}
	sourceStatus := runCraftmake(t, binaryPath, os.Environ(), "status", "--state", statePath, "--run", sourceRunID)
	if !strings.Contains(sourceStatus, "status: failed") || !strings.Contains(sourceStatus, "succeeded: 1") {
		t.Fatalf("unexpected recovered source status:\n%s", sourceStatus)
	}
	if !strings.Contains(sourceStatus, "failed: 1") && !strings.Contains(sourceStatus, "interrupted: 1") {
		t.Fatalf("source run does not contain a recovered non-successful task:\n%s", sourceStatus)
	}

	stateStore, err := store.Open(t.Context(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	resumedRun, resumedCounts, err := stateStore.RunSummary(t.Context(), resumedRunID)
	if err != nil {
		t.Fatal(err)
	}
	if resumedRun.ResumedFromRunID != sourceRunID {
		t.Fatalf("unexpected resume lineage %q", resumedRun.ResumedFromRunID)
	}
	if resumedCounts["cached"] != 1 || resumedCounts["succeeded"] != 1 {
		t.Fatalf("unexpected persisted resumed counts: %#v", resumedCounts)
	}
	if processgroup.GroupExists(orphanProcessGroupID) {
		t.Fatalf("orphan process group %d is still running after resume", orphanProcessGroupID)
	}
	resumedOutputPath := filepath.Join(projectDirectory, "output", "resumed.txt")
	resumedOutputData, err := os.ReadFile(resumedOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(resumedOutputData) != "prepared\n" {
		t.Fatalf("unexpected resumed workflow output %q", resumedOutputData)
	}
}

func waitForResumeOrphan(
	t *testing.T,
	binaryPath string,
	statePath string,
	runID string,
	projectDirectory string,
	stateDirectory string,
) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	prepareOutputPath := filepath.Join(projectDirectory, "output", "prepare.txt")
	waitStartedPath := filepath.Join(projectDirectory, "output", "wait-for-resume.started")
	processGroupPattern := filepath.Join(stateDirectory, "runs", runID, "tasks", "*", "attempt-001", "process-group.pid")
	var lastStatusOutput string
	for time.Now().Before(deadline) {
		statusCommand := exec.Command(binaryPath, "status", "--state", statePath, "--run", runID)
		statusCommand.Env = os.Environ()
		statusOutput, statusErr := statusCommand.CombinedOutput()
		lastStatusOutput = string(statusOutput)
		processGroupPaths, globErr := filepath.Glob(processGroupPattern)
		if globErr != nil {
			t.Fatal(globErr)
		}
		prepareExists := pathExists(prepareOutputPath)
		waitStarted := pathExists(waitStartedPath)
		if statusErr == nil && prepareExists && waitStarted && strings.Contains(lastStatusOutput, "succeeded: 1") && strings.Contains(lastStatusOutput, "running: 1") && len(processGroupPaths) == 1 {
			processGroupData, readErr := os.ReadFile(processGroupPaths[0])
			if readErr != nil {
				t.Fatal(readErr)
			}
			processGroupID, parseErr := strconv.Atoi(strings.TrimSpace(string(processGroupData)))
			if parseErr != nil || processGroupID <= 0 {
				t.Fatalf("invalid orphan process group record %q", processGroupData)
			}
			return processGroupID
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for resumable local orphan; last status output:\n%s", lastStatusOutput)
	return 0
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
