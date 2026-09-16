package slurm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestLoadTaskOutcomeRejectsFutureResultProtocol(t *testing.T) {
	manifest := recoveryTestManifest(t)
	finishedAt := time.Now().UTC()
	writeRecoveryTaskResult(t, manifest.ResultPath, protocol.TaskResult{
		ProtocolVersion: protocol.Version + 1,
		RunID:           manifest.RunID,
		TaskID:          manifest.TaskID,
		Attempt:         manifest.Attempt,
		Status:          "succeeded",
		StartedAt:       finishedAt.Add(-time.Second),
		FinishedAt:      finishedAt,
		ExitCode:        0,
	})

	result, err := New().loadTaskOutcome(context.Background(), "12345", preparedWorker{
		manifest:   manifest,
		stepIDPath: filepath.Join(manifest.RuntimeDirectory, "slurm-step-id"),
	}, false)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected future protocol rejection, got result=%#v error=%v", result, err)
	}
	if result == nil || result.TaskResult != nil {
		t.Fatalf("future protocol result should not be accepted: %#v", result)
	}
}

func TestRefreshMetricsLoadsPersistedStepIDAndQueriesAccounting(t *testing.T) {
	fakeCommandDirectory := t.TempDir()
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sacct"), "#!/bin/sh\nprintf '12345.7|COMPLETED|0:0|12|4|00:00:20|00:00:15|00:00:05|1024K|512K|2048K|4M|8M|4096Mc|node-a|2026-07-23T00:00:00|2026-07-23T00:00:12\\n'\n")
	t.Setenv("PATH", fakeCommandDirectory)

	runtimeDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(runtimeDirectory, "slurm-step-id"), []byte("12345.7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	collected, err := New().RefreshMetrics(context.Background(), backend.MetricsRefreshRequest{
		AttemptID:        "attempt-a",
		RuntimeDirectory: runtimeDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if collected.Source != "slurm_sacct" || collected.Quality != "accounting" {
		t.Fatalf("unexpected refreshed metric identity: %#v", collected)
	}
	if collected.WallSeconds == nil || *collected.WallSeconds != 12 {
		t.Fatalf("unexpected refreshed wall time: %#v", collected.WallSeconds)
	}
	if collected.AllocatedCPUs == nil || *collected.AllocatedCPUs != 4 {
		t.Fatalf("unexpected refreshed allocated CPUs: %#v", collected.AllocatedCPUs)
	}
	if collected.MaxRSSBytes == nil || *collected.MaxRSSBytes != 1<<20 {
		t.Fatalf("unexpected refreshed max RSS: %#v", collected.MaxRSSBytes)
	}
}

func TestQueryStateUsesTerminalAccountingStateWhileQueueIsCompleting(t *testing.T) {
	fakeCommandDirectory := t.TempDir()
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "squeue"), "#!/bin/sh\nprintf 'COMPLETING\\n'\n")
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sacct"), "#!/bin/sh\nprintf '12345|COMPLETED\\n'\n")
	t.Setenv("PATH", fakeCommandDirectory)

	state, complete, err := queryState(context.Background(), "12345")
	if err != nil {
		t.Fatal(err)
	}
	if state != "COMPLETED" || !complete {
		t.Fatalf("unexpected completing state resolution: state=%q complete=%t", state, complete)
	}
}

func TestQueryStateDoesNotTreatNonterminalAccountingStateAsComplete(t *testing.T) {
	fakeCommandDirectory := t.TempDir()
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "squeue"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sacct"), "#!/bin/sh\nprintf '12345|RUNNING\\n'\n")
	t.Setenv("PATH", fakeCommandDirectory)

	state, complete, err := queryState(context.Background(), "12345")
	if err != nil {
		t.Fatal(err)
	}
	if state != "RUNNING" || complete {
		t.Fatalf("unexpected nonterminal accounting state: state=%q complete=%t", state, complete)
	}
}

func TestRecoverSubmissionUsesTerminalResultWithoutSlurmCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	manifest := recoveryTestManifest(t)
	finishedAt := time.Now().UTC()
	writeRecoveryTaskResult(t, manifest.ResultPath, protocol.TaskResult{
		ProtocolVersion: protocol.MinimumCompatibleVersion,
		RunID:           manifest.RunID,
		TaskID:          manifest.TaskID,
		Attempt:         manifest.Attempt,
		Status:          "succeeded",
		StartedAt:       finishedAt.Add(-time.Second),
		FinishedAt:      finishedAt,
		ExitCode:        0,
	})
	if err := os.WriteFile(filepath.Join(manifest.RuntimeDirectory, "slurm-step-id"), []byte("12345.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := New().RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "12345",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, exists := result.Tasks[manifest.TaskID]
	if !exists || outcome.Err != nil || outcome.Result == nil || outcome.Result.TaskResult.Status != "succeeded" {
		t.Fatalf("unexpected recovered outcome: %#v", outcome)
	}
	if outcome.Result.Metrics == nil || outcome.Result.Metrics.Quality != "unavailable" {
		t.Fatalf("expected non-blocking unavailable metrics fallback, got %#v", outcome.Result.Metrics)
	}
}

func TestRecoverSubmissionWaitsForRunningResultAndReloadsTerminalResult(t *testing.T) {
	manifest := recoveryTestManifest(t)
	startedAt := time.Now().UTC().Add(-time.Minute)
	writeRecoveryTaskResult(t, manifest.ResultPath, protocol.TaskResult{
		ProtocolVersion: protocol.Version,
		RunID:           manifest.RunID,
		TaskID:          manifest.TaskID,
		Attempt:         manifest.Attempt,
		Status:          "running",
		StartedAt:       startedAt,
	})
	terminalResultData, err := json.Marshal(protocol.TaskResult{
		ProtocolVersion: protocol.Version,
		RunID:           manifest.RunID,
		TaskID:          manifest.TaskID,
		Attempt:         manifest.Attempt,
		Status:          "succeeded",
		StartedAt:       startedAt,
		FinishedAt:      time.Now().UTC(),
		ExitCode:        0,
	})
	if err != nil {
		t.Fatal(err)
	}

	fakeCommandDirectory := t.TempDir()
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "squeue"), "#!/bin/sh\nexit 0\n")
	sacctScript := "#!/bin/sh\nprintf '%s' " + shellValue(string(terminalResultData)) + " > " + shellValue(manifest.ResultPath) + "\nprintf '12345|COMPLETED\\n'\n"
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sacct"), sacctScript)
	t.Setenv("PATH", fakeCommandDirectory)

	result, err := New().RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "12345",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, exists := result.Tasks[manifest.TaskID]
	if !exists || outcome.Err != nil || outcome.Result == nil || outcome.Result.TaskResult.Status != "succeeded" {
		t.Fatalf("unexpected reattached outcome: %#v", outcome)
	}
}

func TestRecoverSubmissionRejectsMismatchedTerminalResultWithoutSlurmCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	manifest := recoveryTestManifest(t)
	finishedAt := time.Now().UTC()
	writeRecoveryTaskResult(t, manifest.ResultPath, protocol.TaskResult{
		ProtocolVersion: protocol.Version,
		RunID:           manifest.RunID,
		TaskID:          "different-task",
		Attempt:         manifest.Attempt,
		Status:          "succeeded",
		StartedAt:       finishedAt.Add(-time.Second),
		FinishedAt:      finishedAt,
		ExitCode:        0,
	})

	result, err := New().RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "12345",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if err == nil || !strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("expected identity mismatch error, got %v", err)
	}
	outcome := result.Tasks[manifest.TaskID]
	if outcome.Result != nil || outcome.Err == nil {
		t.Fatalf("unexpected mismatched result outcome: %#v", outcome)
	}
}

func TestRecoverSubmissionRejectsFutureResultProtocolWithoutSlurmCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	manifest := recoveryTestManifest(t)
	finishedAt := time.Now().UTC()
	writeRecoveryTaskResult(t, manifest.ResultPath, protocol.TaskResult{
		ProtocolVersion: protocol.Version + 1,
		RunID:           manifest.RunID,
		TaskID:          manifest.TaskID,
		Attempt:         manifest.Attempt,
		Status:          "succeeded",
		StartedAt:       finishedAt.Add(-time.Second),
		FinishedAt:      finishedAt,
		ExitCode:        0,
	})

	result, err := New().RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "12345",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected future protocol rejection, got %v", err)
	}
	outcome := result.Tasks[manifest.TaskID]
	if outcome.Result != nil || outcome.Err == nil {
		t.Fatalf("unexpected future protocol outcome: %#v", outcome)
	}
}

func TestRecoverSubmissionReportsJobMissingWhenResultIsUnavailable(t *testing.T) {
	fakeCommandDirectory := t.TempDir()
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "squeue"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sacct"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", fakeCommandDirectory)
	manifest := recoveryTestManifest(t)
	slurmBackend := New()
	slurmBackend.PollInterval = time.Millisecond
	slurmBackend.RecoveryMissingLimit = 1

	result, err := slurmBackend.RecoverSubmission(context.Background(), backend.RecoveryRequest{
		SubmissionID: "submission-a",
		BackendJobID: "12345",
		Manifests:    []*protocol.TaskManifest{manifest},
	})
	if err == nil || !strings.Contains(err.Error(), "not present in squeue or sacct") {
		t.Fatalf("expected missing Slurm job error, got %v", err)
	}
	outcome, exists := result.Tasks[manifest.TaskID]
	if !exists || outcome.Result != nil || outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "read task result") {
		t.Fatalf("unexpected unavailable result outcome: %#v", outcome)
	}
}

func recoveryTestManifest(t *testing.T) *protocol.TaskManifest {
	t.Helper()
	runtimeDirectory := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(runtimeDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	return &protocol.TaskManifest{
		ProtocolVersion:  protocol.Version,
		RunID:            "run-a",
		TaskID:           "workflow/phase/task-a",
		JobID:            "task-a",
		Attempt:          1,
		Workflow:         "workflow",
		Phase:            "phase",
		Scope:            "global",
		WorkDirectory:    filepath.Dir(runtimeDirectory),
		RuntimeDirectory: runtimeDirectory,
		ResultPath:       filepath.Join(runtimeDirectory, "result.json"),
	}
}

func writeRecoveryTaskResult(t *testing.T, resultPath string, result protocol.TaskResult) {
	t.Helper()
	resultData, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, resultData, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, executablePath string, contents string) {
	t.Helper()
	if err := os.WriteFile(executablePath, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestQueryJobStateIncludesPendingReason(t *testing.T) {
	fakeCommandDirectory := t.TempDir()
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "squeue"), "#!/bin/sh\nprintf 'PENDING|Resources\\n'\n")
	t.Setenv("PATH", fakeCommandDirectory)

	currentState, err := queryJobState(context.Background(), "12345")
	if err != nil {
		t.Fatal(err)
	}
	if currentState.State != "PENDING" || currentState.Reason != "Resources" || currentState.Complete {
		t.Fatalf("unexpected pending state: %#v", currentState)
	}
}

func TestSubmitWithRetryReleasesAndReacquiresAdmissionBetweenAttempts(t *testing.T) {
	fakeCommandDirectory := t.TempDir()
	attemptMarkerPath := filepath.Join(fakeCommandDirectory, "attempted")
	sbatchScript := "#!/bin/sh\nif [ ! -f " + shellValue(attemptMarkerPath) + " ]; then\n  printf 'seen\\n' > " + shellValue(attemptMarkerPath) + "\n  printf 'QOSMaxSubmitJobPerUserLimit\\n' >&2\n  exit 1\nfi\nprintf '12345\\n'\n"
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sbatch"), sbatchScript)
	t.Setenv("PATH", fakeCommandDirectory)

	slurmBackend := New()
	slurmBackend.SubmitMaxAttempts = 2
	slurmBackend.SubmitInitialBackoff = time.Millisecond
	slurmBackend.SubmitMaximumBackoff = time.Millisecond
	states := make([]string, 0, 2)
	jobID, err := slurmBackend.submitWithRetry(context.Background(), "/tmp/submission.sh", "retry-test-job", func(state backend.SubmissionState) error {
		states = append(states, state.State)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if jobID != "12345" {
		t.Fatalf("unexpected job ID %q", jobID)
	}
	expectedStates := []string{"SUBMIT_RETRY_WAIT", "SUBMIT_RETRY_READY"}
	if len(states) != len(expectedStates) {
		t.Fatalf("unexpected retry states: %#v", states)
	}
	for stateIndex, expectedState := range expectedStates {
		if states[stateIndex] != expectedState {
			t.Fatalf("unexpected retry states: %#v", states)
		}
	}
}

func TestSubmitWithRetryReconcilesJobCreatedAfterAmbiguousSbatchError(t *testing.T) {
	fakeCommandDirectory := t.TempDir()
	submitCountPath := filepath.Join(fakeCommandDirectory, "submit-count")
	jobName := "run-a:ambiguous-submission"
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sbatch"), "#!/bin/sh\nprintf 'submitted\\n' >> "+shellValue(submitCountPath)+"\nprintf 'sbatch: error: Batch job submission failed: Unexpected message received\\n' >&2\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "squeue"), "#!/bin/sh\nprintf '12345|"+jobName+"\\n'\n")
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sacct"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", fakeCommandDirectory)

	slurmBackend := New()
	states := make([]backend.SubmissionState, 0, 1)
	jobID, err := slurmBackend.submitWithRetry(context.Background(), "/tmp/submission.sh", jobName, func(state backend.SubmissionState) error {
		states = append(states, state)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if jobID != "12345" {
		t.Fatalf("unexpected reconciled job ID %q", jobID)
	}
	submitCountData, err := os.ReadFile(submitCountPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(submitCountData), "submitted") != 1 {
		t.Fatalf("ambiguous submission should not be retried: %q", submitCountData)
	}
	if len(states) != 1 || states[0].State != "SUBMIT_RECONCILED" {
		t.Fatalf("unexpected reconciliation states: %#v", states)
	}
	if states[0].Details["job_id"] != "12345" {
		t.Fatalf("reconciliation state does not identify the Slurm job: %#v", states[0])
	}
}

func TestQueryJobIDsByNameUsesAccountingAfterQueueExit(t *testing.T) {
	fakeCommandDirectory := t.TempDir()
	jobName := "run-a:completed-submission"
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "squeue"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sacct"), "#!/bin/sh\nprintf '12345|"+jobName+"\\n12345.batch|batch\\n'\n")
	t.Setenv("PATH", fakeCommandDirectory)

	jobIDs, err := queryJobIDsByName(context.Background(), jobName)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobIDs) != 1 || jobIDs[0] != "12345" {
		t.Fatalf("unexpected reconciled accounting jobs: %#v", jobIDs)
	}
}

func TestSubmitWithRetryRejectsMultipleJobsForAmbiguousSubmission(t *testing.T) {
	fakeCommandDirectory := t.TempDir()
	jobName := "run-a:duplicate-submission"
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sbatch"), "#!/bin/sh\nprintf 'slurm_receive_msg failed\\n' >&2\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "squeue"), "#!/bin/sh\nprintf '12345|"+jobName+"\\n12346|"+jobName+"\\n'\n")
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sacct"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", fakeCommandDirectory)

	_, err := New().submitWithRetry(context.Background(), "/tmp/submission.sh", jobName, nil)
	if err == nil || !strings.Contains(err.Error(), "multiple Slurm jobs") {
		t.Fatalf("expected duplicate submission reconciliation failure, got %v", err)
	}
}

func TestSubmitErrorClassificationRetriesOnlyTransientCapacityFailures(t *testing.T) {
	transientMessages := []string{
		"sbatch failed: exit status 1: QOSMaxSubmitJobPerUserLimit",
		"sbatch failed: exit status 1: MaxSubmitJobsPerUser",
		"sbatch failed: exit status 1: temporarily unable to contact Slurm controller",
	}
	for _, message := range transientMessages {
		if !isRetryableSubmitError(errors.New(message)) {
			t.Errorf("expected transient submit error to be retryable: %q", message)
		}
	}
	permanentMessages := []string{
		"sbatch failed: Invalid partition name",
		"sbatch failed: Invalid account or QOS",
		"sbatch failed: Requested node configuration is not available",
	}
	for _, message := range permanentMessages {
		if isRetryableSubmitError(errors.New(message)) {
			t.Errorf("expected permanent submit error not to be retryable: %q", message)
		}
	}
}

func TestBuildScriptOverridesWorkflowPartition(t *testing.T) {
	request := backend.SubmissionRequest{
		SubmissionID: "run:partition-override",
		Resources: protocol.ResourceRequest{
			Cores:      2,
			MemoryByte: 4 << 30,
			Partition:  "cpu",
		},
	}
	workers := []preparedWorker{{
		manifest: &protocol.TaskManifest{
			TaskID:           "workflow/step/job/sample=A",
			Resources:        protocol.ResourceRequest{Cores: 2, MemoryByte: 4 << 30},
			RuntimeDirectory: "/state/task-a",
		},
		scriptPath: "/state/task-a/worker.sh",
	}}

	script, err := buildScript(request, workers, "/state/submission", " amd_512 ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "#SBATCH --partition='amd_512'") {
		t.Fatalf("script does not contain partition override\n%s", script)
	}
	if strings.Contains(script, "#SBATCH --partition='cpu'") {
		t.Fatalf("script retained workflow partition after override\n%s", script)
	}
}

func TestBuildScriptIncludesConfiguredSlurmAllocation(t *testing.T) {
	request := backend.SubmissionRequest{
		SubmissionID: "run:resource-options",
		Resources: protocol.ResourceRequest{
			Cores:      2,
			MemoryByte: 4 << 30,
		},
	}
	workers := []preparedWorker{{
		manifest: &protocol.TaskManifest{
			TaskID:           "workflow/step/job/sample=A",
			Resources:        protocol.ResourceRequest{Cores: 2, MemoryByte: 4 << 30},
			RuntimeDirectory: "/state/task-a",
		},
		scriptPath: "/state/task-a/worker.sh",
	}}

	script, err := buildScriptWithOptions(request, workers, "/state/submission", scriptOptions{
		Partition:   "compute",
		Account:     "genomics",
		QOS:         "normal",
		DefaultTime: "2-01:02:03",
		ScratchRoot: "/scratch/otter",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"#SBATCH --partition='compute'",
		"#SBATCH --account='genomics'",
		"#SBATCH --qos='normal'",
		"#SBATCH --time='2-01:02:03'",
		"export CRAFTMAKE_SCRATCH_ROOT='/scratch/otter'",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("script does not contain %q\\n%s", expected, script)
		}
	}
}

func TestBuildScriptPrefersPhaseAllocationTimeOverSlurmDefault(t *testing.T) {
	request := backend.SubmissionRequest{
		SubmissionID: "run:phase-time",
		Resources: protocol.ResourceRequest{
			Cores:      2,
			MemoryByte: 4 << 30,
			Time:       "02:00:00",
		},
	}
	workers := []preparedWorker{{
		manifest: &protocol.TaskManifest{
			TaskID:           "workflow/step/job/sample=A",
			Resources:        protocol.ResourceRequest{Cores: 2, MemoryByte: 4 << 30, Time: "02:00:00"},
			RuntimeDirectory: "/state/task-a",
		},
		scriptPath: "/state/task-a/worker.sh",
	}}

	script, err := buildScriptWithOptions(request, workers, "/state/submission", scriptOptions{DefaultTime: "08:00:00"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "#SBATCH --time='02:00:00'") {
		t.Fatalf("script did not preserve the phase allocation time\\n%s", script)
	}
	if strings.Contains(script, "#SBATCH --time='08:00:00'") {
		t.Fatalf("script incorrectly used the Slurm default time\\n%s", script)
	}
}

func TestBuildScriptCreatesExclusiveWorkerSteps(t *testing.T) {
	request := backend.SubmissionRequest{
		SubmissionID:      "run:batch-species-human",
		Scope:             "batch",
		Resources:         protocol.ResourceRequest{Cores: 8, MemoryByte: 16 << 30, Partition: "compute", Time: "02:00:00"},
		WorkerMaxParallel: 2,
	}
	workers := []preparedWorker{
		{
			manifest: &protocol.TaskManifest{
				TaskID:           "workflow/step/job/sample=A",
				Resources:        protocol.ResourceRequest{Cores: 2, MemoryByte: 4 << 30},
				RuntimeDirectory: "/state/task-a",
			},
			scriptPath: "/state/task-a/worker.sh",
		},
		{
			manifest: &protocol.TaskManifest{
				TaskID:           "workflow/step/job/sample=B",
				Resources:        protocol.ResourceRequest{Cores: 2, MemoryByte: 4 << 30},
				RuntimeDirectory: "/state/task-b",
			},
			scriptPath: "/state/task-b/worker.sh",
		},
	}

	script, err := buildScript(request, workers, "/state/submission", "")
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(script, "srun --exclusive"); count != 2 {
		t.Fatalf("expected two exclusive workers, found %d\n%s", count, script)
	}
	for _, expected := range []string{
		"#SBATCH --cpus-per-task=8",
		"#SBATCH --mem=16384M",
		"#SBATCH --partition='compute'",
		"maximum_parallel_workers=2",
		"run_worker_with_retry",
		"unable to create step",
		"srun-launch.err",
		"--cpus-per-task=2 --mem=4096M",
		"/state/task-a/worker.sh",
		"/state/task-b/worker.sh",
		"if (( ${#active_worker_pids[@]} > 0 )); then",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("script does not contain %q\n%s", expected, script)
		}
	}
}

func TestBuildScriptRetriesTransientSrunStepCreationFailure(t *testing.T) {
	temporaryDirectory := t.TempDir()
	fakeCommandDirectory := filepath.Join(temporaryDirectory, "bin")
	if err := os.MkdirAll(fakeCommandDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	srunAttemptPath := filepath.Join(temporaryDirectory, "srun-attempts")
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "srun"), "#!/bin/sh\nprintf 'attempt\\n' >> "+shellValue(srunAttemptPath)+"\nif [ \"$(wc -l < "+shellValue(srunAttemptPath)+")\" -eq 1 ]; then\n  printf 'srun: error: Unable to create step: Unexpected message received\\n' >&2\n  exit 1\nfi\nfor argument in \"$@\"; do worker_script=\"$argument\"; done\nSLURM_JOB_ID=12345 SLURM_STEP_ID=0 \"$worker_script\"\n")

	runtimeDirectory := filepath.Join(temporaryDirectory, "runtime")
	if err := os.MkdirAll(runtimeDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	stepIDPath := filepath.Join(runtimeDirectory, "slurm-step-id")
	resultPath := filepath.Join(runtimeDirectory, "result.json")
	workerScriptPath := filepath.Join(runtimeDirectory, "worker.sh")
	writeExecutable(t, workerScriptPath, "#!/bin/sh\nprintf '%s.%s\\n' \"$SLURM_JOB_ID\" \"$SLURM_STEP_ID\" > "+shellValue(stepIDPath)+"\nprintf 'succeeded\\n' > "+shellValue(resultPath)+"\n")

	request := backend.SubmissionRequest{
		SubmissionID: "run-a:worker-retry",
		Resources:    protocol.ResourceRequest{Cores: 2, MemoryByte: 4 << 30},
	}
	workers := []preparedWorker{{
		manifest: &protocol.TaskManifest{
			TaskID:           "workflow/phase/task-a",
			Resources:        protocol.ResourceRequest{Cores: 1, MemoryByte: 1 << 30},
			RuntimeDirectory: runtimeDirectory,
			ResultPath:       resultPath,
		},
		scriptPath: workerScriptPath,
		stepIDPath: stepIDPath,
	}}
	script, err := buildScript(request, workers, temporaryDirectory, "")
	if err != nil {
		t.Fatal(err)
	}
	submissionScriptPath := filepath.Join(temporaryDirectory, "submission.sh")
	writeExecutable(t, submissionScriptPath, script)

	command := exec.Command("bash", submissionScriptPath)
	command.Env = append(os.Environ(), "PATH="+fakeCommandDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated submission script failed: %v\n%s", err, output)
	}
	attemptData, err := os.ReadFile(srunAttemptPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(attemptData), "attempt") != 2 {
		t.Fatalf("expected one transient retry, got %q", attemptData)
	}
	if _, err := os.Stat(stepIDPath); err != nil {
		t.Fatalf("worker did not record a Slurm step ID: %v", err)
	}
	if _, err := os.Stat(resultPath); err != nil {
		t.Fatalf("worker did not produce its result: %v", err)
	}
	launchErrorData, err := os.ReadFile(filepath.Join(runtimeDirectory, "srun-launch.err"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(launchErrorData), "Unable to create step") || !strings.Contains(string(launchErrorData), "attempt=2") {
		t.Fatalf("unexpected worker retry evidence: %s", launchErrorData)
	}
}
