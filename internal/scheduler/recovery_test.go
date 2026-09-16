package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/controllerlog"
	"github.com/otterlab-bio/craftmake/internal/store"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestValidateRecoveredResultAcceptsCompatibleProtocolAndRejectsFutureProtocol(t *testing.T) {
	attempt := store.TaskAttempt{
		ID:            "attempt-a",
		RunID:         "run-a",
		TaskID:        "task-a",
		AttemptNumber: 1,
	}
	finishedAt := time.Now().UTC()
	compatibleResult := &protocol.TaskResult{
		ProtocolVersion: protocol.MinimumCompatibleVersion,
		RunID:           attempt.RunID,
		TaskID:          attempt.TaskID,
		Attempt:         attempt.AttemptNumber,
		Status:          "succeeded",
		StartedAt:       finishedAt.Add(-time.Second),
		FinishedAt:      finishedAt,
	}
	if err := validateRecoveredResult(attempt, compatibleResult); err != nil {
		t.Fatalf("compatible recovered result was rejected: %v", err)
	}

	futureResult := *compatibleResult
	futureResult.ProtocolVersion = protocol.Version + 1
	if err := validateRecoveredResult(attempt, &futureResult); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected future recovered result rejection, got %v", err)
	}
}

type recoveringBackend struct {
	name          string
	result        *protocol.TaskResult
	taskOutcomes  map[string]backend.TaskOutcome
	recoveryCalls int
}

func (testBackend *recoveringBackend) Name() string { return testBackend.name }

func (testBackend *recoveringBackend) RunSubmission(context.Context, string, backend.SubmissionRequest) (*backend.SubmissionResult, error) {
	return nil, nil
}

func (testBackend *recoveringBackend) CancelSubmission(context.Context, string, map[string]any) error {
	return nil
}

func (testBackend *recoveringBackend) Cancel(context.Context) error { return nil }

func (testBackend *recoveringBackend) RecoverSubmission(_ context.Context, request backend.RecoveryRequest) (*backend.SubmissionResult, error) {
	testBackend.recoveryCalls++
	outcomes := make(map[string]backend.TaskOutcome, len(request.Manifests))
	for _, manifest := range request.Manifests {
		if taskOutcome, exists := testBackend.taskOutcomes[manifest.TaskID]; exists {
			outcomes[manifest.TaskID] = taskOutcome
			continue
		}
		outcomes[manifest.TaskID] = backend.TaskOutcome{Result: &backend.Result{
			BackendID:  request.BackendJobID,
			TaskResult: testBackend.result,
		}}
	}
	return &backend.SubmissionResult{BackendID: request.BackendJobID, Tasks: outcomes}, nil
}

func TestReconcileRunPersistsTerminalResultAndFinalizesSourceRun(t *testing.T) {
	ctx := context.Background()
	projectDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(projectDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	runStartedAt := time.Now().UTC().Add(-time.Minute)
	if err := stateStore.CreateRun(ctx, store.Run{
		ID:               "recovery-source-run",
		Workflow:         "workflow",
		Phase:            "phase",
		ConfigPath:       filepath.Join(projectDirectory, "config.yaml"),
		WorkflowPath:     filepath.Join(projectDirectory, "workflow.yaml"),
		Backend:          "recovering",
		CraftmakeVersion: "test",
		Status:           "running",
		StartedAt:        runStartedAt,
	}); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(projectDirectory, "results", "result.txt")
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("recovered output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(projectDirectory, "state", "runs", "recovery-source-run", "tasks", "task-a", "attempt-001", "result.json")
	manifestPath := filepath.Join(filepath.Dir(resultPath), "manifest.json")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &protocol.TaskManifest{
		ProtocolVersion:  protocol.Version,
		RunID:            "recovery-source-run",
		TaskID:           "workflow/phase/task-a",
		JobID:            "task-a",
		Attempt:          1,
		Workflow:         "workflow",
		Phase:            "phase",
		Scope:            "global",
		WorkDirectory:    projectDirectory,
		RuntimeDirectory: filepath.Dir(resultPath),
		ResultPath:       resultPath,
		Outputs:          map[string]string{"result": outputPath},
		Resources:        protocol.ResourceRequest{Cores: 2, MemoryByte: 64 << 20},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := stateStore.UpsertTask(ctx, store.TaskInstance{
		RunID:       "recovery-source-run",
		TaskID:      manifest.TaskID,
		JobID:       manifest.JobID,
		Dimensions:  map[string]string{},
		Inputs:      map[string][]string{},
		Outputs:     manifest.Outputs,
		Fingerprint: "fingerprint",
		Status:      "running",
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.CreateSubmission(ctx, store.Submission{
		ID:           "recovery-submission",
		RunID:        "recovery-source-run",
		Backend:      "recovering",
		Scope:        "global",
		BackendJobID: "backend-job-1",
		Status:       "running",
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.CreateAttempt(ctx, store.TaskAttempt{
		ID:            "recovery-attempt",
		RunID:         "recovery-source-run",
		TaskID:        manifest.TaskID,
		AttemptNumber: 1,
		SubmissionID:  "recovery-submission",
		Status:        "running",
		StartedAt:     &runStartedAt,
		ResultPath:    resultPath,
	}); err != nil {
		t.Fatal(err)
	}

	finishedAt := time.Now().UTC()
	testBackend := &recoveringBackend{
		name: "recovering",
		result: &protocol.TaskResult{
			ProtocolVersion: protocol.Version,
			RunID:           manifest.RunID,
			TaskID:          manifest.TaskID,
			Attempt:         manifest.Attempt,
			Status:          "succeeded",
			StartedAt:       runStartedAt,
			FinishedAt:      finishedAt,
			ExitCode:        0,
		},
	}
	var recoveryLog bytes.Buffer
	structuredLogger := controllerlog.New(&recoveryLog, stateStore)
	recoverySummary, err := ReconcileRun(ctx, stateStore, testBackend, "recovery-source-run", projectDirectory, structuredLogger)
	if err != nil {
		t.Fatal(err)
	}
	for _, expectedEvent := range []string{
		`"event":"recovery.started"`,
		`"event":"submission.recovery_started"`,
		`"event":"attempt.recovery_finished"`,
		`"event":"submission.recovery_finished"`,
		`"event":"recovery.finished"`,
	} {
		if !bytes.Contains(recoveryLog.Bytes(), []byte(expectedEvent)) {
			t.Errorf("recovery controller log did not contain %s", expectedEvent)
		}
	}
	if testBackend.recoveryCalls != 1 {
		t.Fatalf("expected one backend recovery call, got %d", testBackend.recoveryCalls)
	}
	if recoverySummary.FinalStatus != "succeeded" || recoverySummary.Succeeded != 1 || recoverySummary.Interrupted != 0 {
		t.Fatalf("unexpected recovery summary: %#v", recoverySummary)
	}

	run, counts, err := stateStore.RunSummary(ctx, "recovery-source-run")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "succeeded" || counts["succeeded"] != 1 {
		t.Fatalf("unexpected recovered run state: status=%s counts=%#v", run.Status, counts)
	}

	attemptRows, err := stateStore.QueryRows(ctx, `SELECT status FROM task_attempts WHERE attempt_id = ?`, "recovery-attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer attemptRows.Close()
	if !attemptRows.Next() {
		t.Fatal("recovered attempt query returned no row")
	}
	var attemptStatus string
	if err := attemptRows.Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if err := attemptRows.Close(); err != nil {
		t.Fatal(err)
	}
	if attemptStatus != "succeeded" {
		t.Fatalf("unexpected recovered attempt status %q", attemptStatus)
	}
	artifactRows, err := stateStore.QueryRows(ctx, `SELECT validation_status FROM artifacts WHERE attempt_id = ? AND role = 'output'`, "recovery-attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer artifactRows.Close()
	if !artifactRows.Next() {
		t.Fatal("recovered artifact query returned no row")
	}
	var artifactStatus string
	if err := artifactRows.Scan(&artifactStatus); err != nil {
		t.Fatal(err)
	}
	if artifactStatus != "valid" {
		t.Fatalf("unexpected recovered artifact status %q", artifactStatus)
	}
}

func TestReconcileRunPersistsMixedTerminalOutcomes(t *testing.T) {
	ctx := context.Background()
	projectDirectory := t.TempDir()
	stateStore, err := store.Open(ctx, filepath.Join(projectDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	const runID = "mixed-recovery-run"
	const submissionID = "mixed-recovery-submission"
	startedAt := time.Now().UTC().Add(-time.Minute)
	finishedAt := time.Now().UTC()
	if err := stateStore.CreateRun(ctx, store.Run{
		ID:               runID,
		Workflow:         "workflow",
		Phase:            "phase",
		ConfigPath:       filepath.Join(projectDirectory, "config.yaml"),
		WorkflowPath:     filepath.Join(projectDirectory, "workflow.yaml"),
		Backend:          "recovering",
		CraftmakeVersion: "test",
		Status:           "running",
		StartedAt:        startedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.CreateSubmission(ctx, store.Submission{
		ID:           submissionID,
		RunID:        runID,
		Backend:      "recovering",
		Scope:        "batch",
		BackendJobID: "backend-job-mixed",
		Status:       "running",
	}); err != nil {
		t.Fatal(err)
	}

	taskOutcomes := make(map[string]backend.TaskOutcome)
	for _, testCase := range []struct {
		jobID          string
		terminalStatus string
		terminalError  string
		outcomeError   error
		writeOutput    bool
	}{
		{jobID: "task-succeeded", terminalStatus: "succeeded", writeOutput: true},
		{jobID: "task-failed", terminalStatus: "failed", terminalError: "deliberate recovered failure", outcomeError: errors.New("deliberate recovered failure")},
		{jobID: "task-interrupted", outcomeError: errors.New("recovery result is unavailable")},
	} {
		taskID := "workflow/phase/" + testCase.jobID
		runtimeDirectory := filepath.Join(projectDirectory, "state", "runs", runID, "tasks", testCase.jobID, "attempt-001")
		if err := os.MkdirAll(runtimeDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		resultPath := filepath.Join(runtimeDirectory, "result.json")
		outputPath := filepath.Join(projectDirectory, "results", testCase.jobID+".txt")
		if testCase.writeOutput {
			if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outputPath, []byte("recovered output\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		manifest := &protocol.TaskManifest{
			ProtocolVersion:  protocol.Version,
			RunID:            runID,
			TaskID:           taskID,
			JobID:            testCase.jobID,
			Attempt:          1,
			Workflow:         "workflow",
			Phase:            "phase",
			Scope:            "batch",
			WorkDirectory:    projectDirectory,
			RuntimeDirectory: runtimeDirectory,
			ResultPath:       resultPath,
			Outputs:          map[string]string{"result": outputPath},
			Resources:        protocol.ResourceRequest{Cores: 1, MemoryByte: 32 << 20},
		}
		manifestData, marshalErr := json.Marshal(manifest)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err := os.WriteFile(filepath.Join(runtimeDirectory, "manifest.json"), manifestData, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := stateStore.UpsertTask(ctx, store.TaskInstance{
			RunID:       runID,
			TaskID:      taskID,
			JobID:       testCase.jobID,
			Dimensions:  map[string]string{},
			Inputs:      map[string][]string{},
			Outputs:     manifest.Outputs,
			Fingerprint: "fingerprint-" + testCase.jobID,
			Status:      "running",
		}); err != nil {
			t.Fatal(err)
		}
		if err := stateStore.CreateAttempt(ctx, store.TaskAttempt{
			ID:            "attempt-" + testCase.jobID,
			RunID:         runID,
			TaskID:        taskID,
			AttemptNumber: 1,
			SubmissionID:  submissionID,
			Status:        "running",
			StartedAt:     &startedAt,
			ResultPath:    resultPath,
		}); err != nil {
			t.Fatal(err)
		}
		if testCase.terminalStatus == "" {
			taskOutcomes[taskID] = backend.TaskOutcome{Err: testCase.outcomeError}
			continue
		}
		exitCode := 0
		if testCase.terminalStatus == "failed" {
			exitCode = 1
		}
		taskOutcomes[taskID] = backend.TaskOutcome{
			Result: &backend.Result{
				BackendID: "backend-job-mixed",
				TaskResult: &protocol.TaskResult{
					ProtocolVersion: protocol.Version,
					RunID:           runID,
					TaskID:          taskID,
					Attempt:         1,
					Status:          testCase.terminalStatus,
					StartedAt:       startedAt,
					FinishedAt:      finishedAt,
					ExitCode:        exitCode,
					Error:           testCase.terminalError,
				},
			},
			Err: testCase.outcomeError,
		}
	}

	testBackend := &recoveringBackend{name: "recovering", taskOutcomes: taskOutcomes}
	summary, err := ReconcileRun(ctx, stateStore, testBackend, runID, projectDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FinalStatus != "failed" || summary.Succeeded != 1 || summary.Failed != 1 || summary.Interrupted != 1 || summary.Cancelled != 0 {
		t.Fatalf("unexpected mixed recovery summary: %#v", summary)
	}

	run, counts, err := stateStore.RunSummary(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "failed" || counts["succeeded"] != 1 || counts["failed"] != 1 || counts["interrupted"] != 1 {
		t.Fatalf("unexpected mixed recovered run: status=%s counts=%#v", run.Status, counts)
	}

	attemptRows, err := stateStore.QueryRows(ctx, `
		SELECT task_id, status, COALESCE(failure_reason, '')
		FROM task_attempts
		WHERE run_id = ?
		ORDER BY task_id
	`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer attemptRows.Close()
	attemptStatuses := make(map[string]string)
	attemptReasons := make(map[string]string)
	for attemptRows.Next() {
		var taskID string
		var status string
		var reason string
		if err := attemptRows.Scan(&taskID, &status, &reason); err != nil {
			t.Fatal(err)
		}
		attemptStatuses[taskID] = status
		attemptReasons[taskID] = reason
	}
	if err := attemptRows.Err(); err != nil {
		t.Fatal(err)
	}
	if attemptStatuses["workflow/phase/task-succeeded"] != "succeeded" {
		t.Fatalf("unexpected succeeded attempt status map: %#v", attemptStatuses)
	}
	if attemptStatuses["workflow/phase/task-failed"] != "failed" || !strings.Contains(attemptReasons["workflow/phase/task-failed"], "deliberate recovered failure") {
		t.Fatalf("unexpected failed attempt state: statuses=%#v reasons=%#v", attemptStatuses, attemptReasons)
	}
	if attemptStatuses["workflow/phase/task-interrupted"] != "interrupted" || !strings.Contains(attemptReasons["workflow/phase/task-interrupted"], "result is unavailable") {
		t.Fatalf("unexpected interrupted attempt state: statuses=%#v reasons=%#v", attemptStatuses, attemptReasons)
	}
}

func TestReconcileRunMarksMissingRecoveryDataInterrupted(t *testing.T) {
	ctx := context.Background()
	stateStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	startedAt := time.Now().UTC().Add(-time.Minute)
	if err := stateStore.CreateRun(ctx, store.Run{ID: "missing-recovery-run", Workflow: "workflow", Phase: "phase", Backend: "recovering", Status: "running", StartedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.UpsertTask(ctx, store.TaskInstance{RunID: "missing-recovery-run", TaskID: "task-a", JobID: "task-a", Dimensions: map[string]string{}, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.CreateAttempt(ctx, store.TaskAttempt{ID: "missing-attempt", RunID: "missing-recovery-run", TaskID: "task-a", AttemptNumber: 1, SubmissionID: "missing-submission", Status: "running", StartedAt: &startedAt, ResultPath: filepath.Join(t.TempDir(), "missing-result.json")}); err != nil {
		t.Fatal(err)
	}

	testBackend := &recoveringBackend{name: "recovering"}
	summary, err := ReconcileRun(ctx, stateStore, testBackend, "missing-recovery-run", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if summary.FinalStatus != "failed" || summary.Interrupted != 1 {
		t.Fatalf("unexpected missing recovery summary: %#v", summary)
	}
	statusRows, err := stateStore.QueryRows(ctx, `SELECT status FROM task_attempts WHERE attempt_id = ?`, "missing-attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer statusRows.Close()
	if !statusRows.Next() {
		t.Fatal("missing recovery attempt query returned no row")
	}
	var status string
	if err := statusRows.Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "interrupted" {
		t.Fatalf("expected interrupted attempt, got %q", status)
	}
}
