package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/controllerlog"
	"github.com/otterlab-bio/craftmake/internal/metrics"
	runtimeexecutor "github.com/otterlab-bio/craftmake/internal/runtime"
	"github.com/otterlab-bio/craftmake/internal/store"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type RecoverySummary struct {
	RunID       string
	FinalStatus string
	Succeeded   int
	Failed      int
	Cancelled   int
	Interrupted int
	Submissions int
}

func ReconcileRun(
	ctx context.Context,
	stateStore *store.Store,
	selectedBackend backend.Backend,
	runID string,
	projectDirectory string,
	providedLoggers ...*controllerlog.Logger,
) (RecoverySummary, error) {
	structuredLogger := controllerlog.Discard()
	if len(providedLoggers) > 0 && providedLoggers[0] != nil {
		structuredLogger = providedLoggers[0]
	}
	summary := RecoverySummary{RunID: runID}
	run, _, err := stateStore.RunSummary(ctx, runID)
	if err != nil {
		return summary, err
	}
	if run.Status != "running" {
		summary.FinalStatus = run.Status
		return summary, nil
	}
	if selectedBackend.Name() != run.Backend {
		return summary, fmt.Errorf("recovery backend %q does not match persisted backend %q", selectedBackend.Name(), run.Backend)
	}
	recoverableBackend, supportsRecovery := selectedBackend.(backend.Recoverable)
	if !supportsRecovery {
		return summary, fmt.Errorf("backend %q does not support submission recovery", selectedBackend.Name())
	}
	recoveryStartedAt := time.Now().UTC()
	structuredLogger.Log(ctx, controllerlog.Event{
		Timestamp: recoveryStartedAt,
		Name:      "recovery.started",
		RunID:     runID,
		Backend:   selectedBackend.Name(),
		Status:    "running",
	})
	logRecoveryFailure := func(recoveryErr error) {
		finishedAt := time.Now().UTC()
		structuredLogger.Log(ctx, controllerlog.Event{
			Timestamp:            finishedAt,
			Level:                "error",
			Name:                 "recovery.finished",
			RunID:                runID,
			Backend:              selectedBackend.Name(),
			Status:               "failed",
			DurationMilliseconds: durationMilliseconds(recoveryStartedAt, finishedAt),
			Error:                recoveryErr.Error(),
			Details: map[string]any{
				"succeeded":   summary.Succeeded,
				"failed":      summary.Failed,
				"cancelled":   summary.Cancelled,
				"interrupted": summary.Interrupted,
				"submissions": summary.Submissions,
			},
		})
	}

	runningAttempts, err := stateStore.RunningAttempts(ctx, runID)
	if err != nil {
		logRecoveryFailure(err)
		return summary, err
	}
	attemptsBySubmission := make(map[string][]store.TaskAttempt)
	attemptsByID := make(map[string]store.TaskAttempt, len(runningAttempts))
	for _, attempt := range runningAttempts {
		attemptsBySubmission[attempt.SubmissionID] = append(attemptsBySubmission[attempt.SubmissionID], attempt)
		attemptsByID[attempt.ID] = attempt
	}

	runningSubmissions, err := stateStore.RunningSubmissions(ctx, runID)
	if err != nil {
		logRecoveryFailure(err)
		return summary, err
	}
	processedAttempts := make(map[string]bool, len(runningAttempts))
	for _, submission := range runningSubmissions {
		summary.Submissions++
		attempts := attemptsBySubmission[submission.ID]
		manifests, manifestAttempts := loadRecoveryManifests(ctx, stateStore, structuredLogger, selectedBackend.Name(), attempts, processedAttempts, &summary)
		metadata := decodeRecoveryMetadata(submission.RawMetadata)
		runtimeDirectory, _ := metadata["runtime_directory"].(string)
		structuredLogger.Log(ctx, controllerlog.Event{
			Name:         "submission.recovery_started",
			RunID:        runID,
			SubmissionID: submission.ID,
			Backend:      selectedBackend.Name(),
			BackendJobID: submission.BackendJobID,
			Status:       "running",
			Details: map[string]any{
				"attempt_count":     len(attempts),
				"runtime_directory": runtimeDirectory,
			},
		})
		if len(manifests) == 0 {
			finishedAt := time.Now().UTC()
			finishRecoverySubmission(ctx, stateStore, submission, "interrupted", metadata, "no recoverable task manifests", finishedAt)
			structuredLogger.Log(ctx, controllerlog.Event{
				Timestamp:    finishedAt,
				Level:        "warn",
				Name:         "submission.recovery_finished",
				RunID:        runID,
				SubmissionID: submission.ID,
				Backend:      selectedBackend.Name(),
				BackendJobID: submission.BackendJobID,
				Status:       "interrupted",
				Error:        "no recoverable task manifests",
			})
			continue
		}

		submissionResult, recoveryErr := recoverableBackend.RecoverSubmission(ctx, backend.RecoveryRequest{
			SubmissionID:     submission.ID,
			BackendJobID:     submission.BackendJobID,
			RuntimeDirectory: runtimeDirectory,
			Metadata:         metadata,
			Manifests:        manifests,
		})
		submissionStatus := "succeeded"
		for _, manifest := range manifests {
			attempt := manifestAttempts[manifest.TaskID]
			processedAttempts[attempt.ID] = true
			outcome, exists := submissionOutcome(submissionResult, manifest.TaskID)
			if !exists {
				outcome.Err = fmt.Errorf("recovery backend returned no outcome for task %s", manifest.TaskID)
			}
			status, reconcileErr := reconcileRecoveredAttempt(ctx, stateStore, projectDirectory, attempt, manifest, outcome)
			if reconcileErr != nil && recoveryErr == nil {
				recoveryErr = reconcileErr
			}
			attemptFinishedAt := time.Now().UTC()
			backendJobID := submission.BackendJobID
			if outcome.Result != nil {
				if outcome.Result.BackendID != "" {
					backendJobID = outcome.Result.BackendID
				}
				if outcome.Result.TaskResult != nil && !outcome.Result.TaskResult.FinishedAt.IsZero() {
					attemptFinishedAt = outcome.Result.TaskResult.FinishedAt
				}
			}
			attemptErr := reconcileErr
			if attemptErr == nil {
				attemptErr = outcome.Err
			}
			structuredLogger.Log(ctx, controllerlog.Event{
				Timestamp:            attemptFinishedAt,
				Level:                eventLevelForStatus(status),
				Name:                 "attempt.recovery_finished",
				RunID:                runID,
				SubmissionID:         submission.ID,
				TaskID:               attempt.TaskID,
				AttemptID:            attempt.ID,
				AttemptNumber:        attempt.AttemptNumber,
				Backend:              selectedBackend.Name(),
				BackendJobID:         backendJobID,
				Status:               status,
				DurationMilliseconds: attemptDurationMilliseconds(attempt, attemptFinishedAt),
				Error:                errorMessage(attemptErr),
			})
			switch status {
			case "succeeded":
				summary.Succeeded++
			case "failed":
				summary.Failed++
				submissionStatus = "failed"
			case "cancelled":
				summary.Cancelled++
				submissionStatus = "failed"
			default:
				summary.Interrupted++
				if submissionStatus == "succeeded" {
					submissionStatus = "interrupted"
				}
			}
		}
		if recoveryErr != nil {
			metadata["recovery_error"] = recoveryErr.Error()
		}
		finishedAt := time.Now().UTC()
		finishRecoverySubmission(ctx, stateStore, submission, submissionStatus, metadata, "", finishedAt)
		structuredLogger.Log(ctx, controllerlog.Event{
			Timestamp:    finishedAt,
			Level:        eventLevelForStatus(submissionStatus),
			Name:         "submission.recovery_finished",
			RunID:        runID,
			SubmissionID: submission.ID,
			Backend:      selectedBackend.Name(),
			BackendJobID: submission.BackendJobID,
			Status:       submissionStatus,
			Error:        errorMessage(recoveryErr),
			Details:      cloneEventDetails(metadata),
		})
	}

	for attemptID, attempt := range attemptsByID {
		if processedAttempts[attemptID] {
			continue
		}
		finishedAt := time.Now().UTC()
		reason := "controller recovery found no running submission for attempt"
		if err := stateStore.InterruptAttempt(ctx, attempt, reason, finishedAt); err != nil {
			logRecoveryFailure(err)
			return summary, err
		}
		structuredLogger.Log(ctx, controllerlog.Event{
			Timestamp:            finishedAt,
			Level:                "warn",
			Name:                 "attempt.recovery_finished",
			RunID:                runID,
			SubmissionID:         attempt.SubmissionID,
			TaskID:               attempt.TaskID,
			AttemptID:            attempt.ID,
			AttemptNumber:        attempt.AttemptNumber,
			Backend:              selectedBackend.Name(),
			Status:               "interrupted",
			DurationMilliseconds: attemptDurationMilliseconds(attempt, finishedAt),
			Error:                reason,
		})
		summary.Interrupted++
	}

	finishedAt := time.Now().UTC()
	finalStatus, err := stateStore.FinalizeRecoveredRun(ctx, runID, "controller recovery could not reconstruct a terminal task result", finishedAt)
	if err != nil {
		logRecoveryFailure(err)
		return summary, err
	}
	summary.FinalStatus = finalStatus
	structuredLogger.Log(ctx, controllerlog.Event{
		Timestamp:            finishedAt,
		Level:                eventLevelForStatus(finalStatus),
		Name:                 "recovery.finished",
		RunID:                runID,
		Backend:              selectedBackend.Name(),
		Status:               finalStatus,
		DurationMilliseconds: durationMilliseconds(recoveryStartedAt, finishedAt),
		Details: map[string]any{
			"succeeded":   summary.Succeeded,
			"failed":      summary.Failed,
			"cancelled":   summary.Cancelled,
			"interrupted": summary.Interrupted,
			"submissions": summary.Submissions,
		},
	})
	return summary, nil
}

func loadRecoveryManifests(
	ctx context.Context,
	stateStore *store.Store,
	structuredLogger *controllerlog.Logger,
	backendName string,
	attempts []store.TaskAttempt,
	processedAttempts map[string]bool,
	summary *RecoverySummary,
) ([]*protocol.TaskManifest, map[string]store.TaskAttempt) {
	manifests := make([]*protocol.TaskManifest, 0, len(attempts))
	manifestAttempts := make(map[string]store.TaskAttempt, len(attempts))
	for _, attempt := range attempts {
		manifestPath := filepath.Join(filepath.Dir(attempt.ResultPath), "manifest.json")
		manifest, err := runtimeexecutor.LoadManifest(manifestPath)
		if err == nil {
			err = validateRecoveryManifest(attempt, manifest)
		}
		if err != nil {
			finishedAt := time.Now().UTC()
			reason := fmt.Sprintf("load recovery manifest: %v", err)
			_ = stateStore.InterruptAttempt(ctx, attempt, reason, finishedAt)
			structuredLogger.Log(ctx, controllerlog.Event{
				Timestamp:            finishedAt,
				Level:                "warn",
				Name:                 "attempt.recovery_finished",
				RunID:                attempt.RunID,
				SubmissionID:         attempt.SubmissionID,
				TaskID:               attempt.TaskID,
				AttemptID:            attempt.ID,
				AttemptNumber:        attempt.AttemptNumber,
				Backend:              backendName,
				Status:               "interrupted",
				DurationMilliseconds: attemptDurationMilliseconds(attempt, finishedAt),
				Error:                reason,
			})
			processedAttempts[attempt.ID] = true
			summary.Interrupted++
			continue
		}
		manifests = append(manifests, manifest)
		manifestAttempts[manifest.TaskID] = attempt
	}
	sort.Slice(manifests, func(leftIndex, rightIndex int) bool {
		return manifests[leftIndex].TaskID < manifests[rightIndex].TaskID
	})
	return manifests, manifestAttempts
}

func validateRecoveryManifest(attempt store.TaskAttempt, manifest *protocol.TaskManifest) error {
	if manifest.RunID != attempt.RunID || manifest.TaskID != attempt.TaskID || manifest.Attempt != attempt.AttemptNumber {
		return fmt.Errorf("manifest identity does not match persisted attempt %q", attempt.ID)
	}
	if filepath.Clean(manifest.ResultPath) != filepath.Clean(attempt.ResultPath) {
		return fmt.Errorf("manifest result path does not match persisted attempt %q", attempt.ID)
	}
	return nil
}

func reconcileRecoveredAttempt(
	ctx context.Context,
	stateStore *store.Store,
	projectDirectory string,
	attempt store.TaskAttempt,
	manifest *protocol.TaskManifest,
	outcome backend.TaskOutcome,
) (string, error) {
	if outcome.Result == nil || outcome.Result.TaskResult == nil {
		reason := "recovery backend did not return a terminal task result"
		if outcome.Err != nil {
			reason = outcome.Err.Error()
		}
		return "interrupted", stateStore.InterruptAttempt(ctx, attempt, reason, time.Now().UTC())
	}
	result := outcome.Result.TaskResult
	if err := validateRecoveredResult(attempt, result); err != nil {
		interruptErr := stateStore.InterruptAttempt(ctx, attempt, err.Error(), time.Now().UTC())
		if interruptErr != nil {
			return "interrupted", interruptErr
		}
		return "interrupted", err
	}

	artifacts := collectManifestArtifacts(projectDirectory, manifest)
	if err := stateStore.FinishAttempt(ctx, attempt.ID, result.Status, result, artifacts); err != nil {
		return result.Status, err
	}
	collectedMetrics := outcome.Result.Metrics
	if collectedMetrics == nil {
		collectedMetrics = &metrics.TaskMetrics{Source: "artifact_snapshot", Quality: "metadata", Raw: map[string]string{}}
	}
	inputArtifactBytes, outputArtifactBytes := artifactByteTotals(artifacts)
	collectedMetrics.InputArtifactBytes = &inputArtifactBytes
	collectedMetrics.OutputArtifactBytes = &outputArtifactBytes
	if err := stateStore.SaveMetrics(ctx, attempt.ID, collectedMetrics, int64(manifest.Resources.Cores), manifest.Resources.MemoryByte); err != nil {
		return result.Status, err
	}
	if err := stateStore.UpdateTaskStatus(ctx, attempt.RunID, attempt.TaskID, result.Status); err != nil {
		return result.Status, err
	}
	return result.Status, outcome.Err
}

func validateRecoveredResult(attempt store.TaskAttempt, result *protocol.TaskResult) error {
	expectedManifest := &protocol.TaskManifest{
		RunID:   attempt.RunID,
		TaskID:  attempt.TaskID,
		Attempt: attempt.AttemptNumber,
	}
	if err := protocol.ValidateTaskResultForManifest(result, expectedManifest, true); err != nil {
		return fmt.Errorf("validate recovered result for persisted attempt %q: %w", attempt.ID, err)
	}
	if result.FinishedAt.IsZero() {
		return fmt.Errorf("recovered result has no finish time")
	}
	return nil
}

func collectManifestArtifacts(projectDirectory string, manifest *protocol.TaskManifest) []store.Artifact {
	artifacts := make([]store.Artifact, 0, len(manifest.Inputs)+len(manifest.Outputs))
	inputNames := make([]string, 0, len(manifest.Inputs))
	for inputName := range manifest.Inputs {
		inputNames = append(inputNames, inputName)
	}
	sort.Strings(inputNames)
	for _, inputName := range inputNames {
		inputPaths := manifest.Inputs[inputName]
		for pathIndex, inputPath := range inputPaths {
			artifactName := inputName
			if len(inputPaths) > 1 {
				artifactName = fmt.Sprintf("%s[%d]", inputName, pathIndex)
			}
			artifacts = append(artifacts, snapshotArtifact(projectDirectory, "input", artifactName, inputPath))
		}
	}
	outputNames := make([]string, 0, len(manifest.Outputs))
	for outputName := range manifest.Outputs {
		outputNames = append(outputNames, outputName)
	}
	sort.Strings(outputNames)
	for _, outputName := range outputNames {
		artifacts = append(artifacts, snapshotArtifact(projectDirectory, "output", outputName, manifest.Outputs[outputName]))
	}
	return artifacts
}

func decodeRecoveryMetadata(rawMetadata json.RawMessage) map[string]any {
	metadata := map[string]any{}
	if len(rawMetadata) > 0 {
		_ = json.Unmarshal(rawMetadata, &metadata)
	}
	return metadata
}

func finishRecoverySubmission(
	ctx context.Context,
	stateStore *store.Store,
	submission store.Submission,
	status string,
	metadata map[string]any,
	recoveryError string,
	finishedAt time.Time,
) {
	if recoveryError != "" {
		metadata["recovery_error"] = recoveryError
	}
	_ = stateStore.FinishSubmission(ctx, submission.ID, status, submission.BackendJobID, finishedAt, marshalMetadata(metadata))
}

func attemptDurationMilliseconds(attempt store.TaskAttempt, finishedAt time.Time) int64 {
	if attempt.StartedAt == nil {
		return 0
	}
	return durationMilliseconds(*attempt.StartedAt, finishedAt)
}
