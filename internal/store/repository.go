package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/otterlab-bio/craftmake/internal/metrics"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func (stateStore *Store) CreateRun(ctx context.Context, run Run) error {
	_, err := stateStore.database.ExecContext(ctx, `
		INSERT INTO runs(run_id, workflow, phase, config_path, config_digest, workflow_path, workflow_digest, backend, craftmake_version, resumed_from_run_id, loader_kind, status, started_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, run.ID, run.Workflow, run.Phase, run.ConfigPath, run.ConfigDigest, run.WorkflowPath, run.WorkflowDigest, run.Backend, run.CraftmakeVersion, nullableString(run.ResumedFromRunID), run.LoaderKind, run.Status, formatTime(run.StartedAt))
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	return nil
}

func (stateStore *Store) FinishRun(ctx context.Context, runID, status string, finishedAt time.Time) error {
	_, err := stateStore.database.ExecContext(ctx, `UPDATE runs SET status = ?, finished_at = ? WHERE run_id = ?`, status, formatTime(finishedAt), runID)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

func (stateStore *Store) RecordEvent(
	ctx context.Context,
	runID string,
	taskID string,
	occurredAt time.Time,
	eventType string,
	payload json.RawMessage,
) error {
	_, err := stateStore.database.ExecContext(ctx, `
		INSERT INTO events(run_id, task_id, occurred_at, event_type, payload_json)
		VALUES(?, ?, ?, ?, ?)
	`, runID, nullableString(taskID), formatTime(occurredAt), eventType, string(payload))
	if err != nil {
		return fmt.Errorf("record event %q: %w", eventType, err)
	}
	return nil
}

func (stateStore *Store) UpsertTask(ctx context.Context, task TaskInstance) error {
	dimensions, _ := json.Marshal(task.Dimensions)
	inputs, _ := json.Marshal(task.Inputs)
	outputs, _ := json.Marshal(task.Outputs)
	_, err := stateStore.database.ExecContext(ctx, `
		INSERT INTO task_instances(
			task_id, run_id, job_id, dimensions_json, inputs_json, outputs_json, fingerprint,
			definition_fingerprint, dependency_fingerprint, cache_decision, cache_reason_code, cache_reason_detail, status
		)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, task_id) DO UPDATE SET
			job_id=excluded.job_id,
			dimensions_json=excluded.dimensions_json,
			inputs_json=excluded.inputs_json,
			outputs_json=excluded.outputs_json,
			fingerprint=excluded.fingerprint,
			definition_fingerprint=excluded.definition_fingerprint,
			dependency_fingerprint=excluded.dependency_fingerprint,
			cache_decision=excluded.cache_decision,
			cache_reason_code=excluded.cache_reason_code,
			cache_reason_detail=excluded.cache_reason_detail,
			status=excluded.status
	`, task.TaskID, task.RunID, task.JobID, string(dimensions), string(inputs), string(outputs), task.Fingerprint,
		nullableString(task.DefinitionFingerprint), nullableString(task.DependencyFingerprint), nullableString(task.CacheDecision),
		nullableString(task.CacheReasonCode), nullableString(task.CacheReasonDetail), task.Status)
	if err != nil {
		return fmt.Errorf("upsert task %q: %w", task.TaskID, err)
	}
	return nil
}

func (stateStore *Store) AddDependency(ctx context.Context, dependency Dependency) error {
	_, err := stateStore.database.ExecContext(ctx, `
		INSERT OR IGNORE INTO dependencies(run_id, upstream_task_id, downstream_task_id, dependency_type) VALUES(?, ?, ?, ?)
	`, dependency.RunID, dependency.UpstreamTaskID, dependency.DownstreamTaskID, dependency.Type)
	if err != nil {
		return fmt.Errorf("add dependency: %w", err)
	}
	return nil
}

func (stateStore *Store) UpdateTaskStatus(ctx context.Context, runID, taskID, status string) error {
	_, err := stateStore.database.ExecContext(ctx, `UPDATE task_instances SET status = ? WHERE run_id = ? AND task_id = ?`, status, runID, taskID)
	if err != nil {
		return fmt.Errorf("update task %q status: %w", taskID, err)
	}
	return nil
}

func (stateStore *Store) EvaluateCache(
	ctx context.Context,
	taskID string,
	fingerprint string,
	definitionFingerprint string,
	dependencyFingerprint string,
	projectDirectory string,
	inputs map[string][]string,
	outputs map[string]string,
) (CacheDecision, error) {
	var attemptID string
	err := stateStore.database.QueryRowContext(ctx, `
		SELECT task_attempts.attempt_id
		FROM task_instances
		JOIN task_attempts
		  ON task_attempts.run_id = task_instances.run_id
		 AND task_attempts.task_id = task_instances.task_id
		WHERE task_instances.task_id = ?
		  AND task_instances.fingerprint = ?
		  AND task_attempts.status = 'succeeded'
		ORDER BY task_attempts.finished_at DESC
		LIMIT 1
	`, taskID, fingerprint).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return stateStore.classifyFingerprintCacheMiss(ctx, taskID, definitionFingerprint, dependencyFingerprint)
	}
	if err != nil {
		return CacheDecision{}, fmt.Errorf("query fingerprint cache: %w", err)
	}

	cachedArtifacts, err := stateStore.cachedArtifacts(ctx, attemptID)
	if err != nil {
		return CacheDecision{}, err
	}
	expectedArtifacts := expectedTaskArtifacts(projectDirectory, inputs, outputs)
	if len(cachedArtifacts) != len(expectedArtifacts) {
		return cacheMiss("artifact_set_changed", fmt.Sprintf("cached artifact count %d does not match expected count %d", len(cachedArtifacts), len(expectedArtifacts))), nil
	}

	artifactKeys := make([]string, 0, len(expectedArtifacts))
	for artifactKey := range expectedArtifacts {
		artifactKeys = append(artifactKeys, artifactKey)
	}
	sort.Strings(artifactKeys)
	for _, artifactKey := range artifactKeys {
		expectedArtifact := expectedArtifacts[artifactKey]
		cachedArtifact, exists := cachedArtifacts[artifactKey]
		if !exists {
			return cacheMiss("artifact_set_changed", fmt.Sprintf("expected artifact %s was not recorded", artifactKey)), nil
		}
		if decision := compareCachedArtifact(cachedArtifact, expectedArtifact.Path); !decision.Hit {
			return decision, nil
		}
	}
	return CacheDecision{Hit: true, Decision: "hit", Detail: fmt.Sprintf("reused successful attempt %s", attemptID)}, nil
}

func (stateStore *Store) classifyFingerprintCacheMiss(
	ctx context.Context,
	taskID string,
	definitionFingerprint string,
	dependencyFingerprint string,
) (CacheDecision, error) {
	var cachedFingerprint string
	var cachedDefinitionFingerprint sql.NullString
	var cachedDependencyFingerprint sql.NullString
	err := stateStore.database.QueryRowContext(ctx, `
		SELECT task_instances.fingerprint,
		       task_instances.definition_fingerprint,
		       task_instances.dependency_fingerprint
		FROM task_instances
		JOIN task_attempts
		  ON task_attempts.run_id = task_instances.run_id
		 AND task_attempts.task_id = task_instances.task_id
		WHERE task_instances.task_id = ?
		  AND task_attempts.status = 'succeeded'
		ORDER BY task_attempts.finished_at DESC
		LIMIT 1
	`, taskID).Scan(&cachedFingerprint, &cachedDefinitionFingerprint, &cachedDependencyFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return cacheMiss("no_prior_success", "no prior successful attempt exists for this task"), nil
	}
	if err != nil {
		return CacheDecision{}, fmt.Errorf("classify fingerprint cache miss: %w", err)
	}

	definitionChanged := cachedDefinitionFingerprint.Valid && cachedDefinitionFingerprint.String != definitionFingerprint
	dependencyChanged := cachedDependencyFingerprint.Valid && cachedDependencyFingerprint.String != dependencyFingerprint
	switch {
	case definitionChanged && dependencyChanged:
		return cacheMiss("definition_changed", "task definition and dependency fingerprints changed"), nil
	case definitionChanged:
		return cacheMiss("definition_changed", "task definition fingerprint changed"), nil
	case dependencyChanged:
		return cacheMiss("dependency_changed", "dependency fingerprint changed"), nil
	default:
		return cacheMiss("fingerprint_changed", fmt.Sprintf("runtime fingerprint changed from %s", abbreviatedFingerprint(cachedFingerprint))), nil
	}
}

func (stateStore *Store) cachedArtifacts(ctx context.Context, attemptID string) (map[string]Artifact, error) {
	rows, err := stateStore.database.QueryContext(ctx, `
		SELECT role, name, path, size_bytes, mtime_ns, validation_status
		FROM artifacts
		WHERE attempt_id = ? AND role IN ('input', 'output')
	`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("query cached artifacts: %w", err)
	}
	defer rows.Close()

	cachedArtifacts := make(map[string]Artifact)
	for rows.Next() {
		var artifact Artifact
		var sizeBytes sql.NullInt64
		var modificationTimeNanoseconds sql.NullInt64
		if err := rows.Scan(&artifact.Role, &artifact.Name, &artifact.Path, &sizeBytes, &modificationTimeNanoseconds, &artifact.ValidationStatus); err != nil {
			return nil, fmt.Errorf("scan cached artifact: %w", err)
		}
		if sizeBytes.Valid {
			artifact.SizeBytes = &sizeBytes.Int64
		}
		if modificationTimeNanoseconds.Valid {
			modificationTime := time.Unix(0, modificationTimeNanoseconds.Int64).UTC()
			artifact.ModificationTime = &modificationTime
		}
		cachedArtifacts[artifact.Role+":"+artifact.Name] = artifact
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read cached artifacts: %w", err)
	}
	return cachedArtifacts, nil
}

func compareCachedArtifact(cachedArtifact Artifact, expectedPath string) CacheDecision {
	artifactIdentity := cachedArtifact.Role + ":" + cachedArtifact.Name
	if expectedPath == "" {
		return cacheMiss("artifact_path_changed", fmt.Sprintf("%s has an empty expected path", artifactIdentity))
	}
	if cachedArtifact.ValidationStatus != "valid" {
		return cacheMiss("artifact_invalid", fmt.Sprintf("%s was recorded with validation status %q", artifactIdentity, cachedArtifact.ValidationStatus))
	}
	if filepath.Clean(cachedArtifact.Path) != expectedPath {
		return cacheMiss("artifact_path_changed", fmt.Sprintf("%s path changed from %s to %s", artifactIdentity, cachedArtifact.Path, expectedPath))
	}
	if cachedArtifact.SizeBytes == nil || cachedArtifact.ModificationTime == nil {
		return cacheMiss("artifact_metadata_missing", fmt.Sprintf("%s has no complete size and modification-time snapshot", artifactIdentity))
	}
	fileInfo, err := osStat(expectedPath)
	if err != nil {
		return cacheMiss(cachedArtifact.Role+"_missing", fmt.Sprintf("%s is unavailable at %s: %v", artifactIdentity, expectedPath, err))
	}
	if fileInfo.Size() != *cachedArtifact.SizeBytes {
		return cacheMiss(cachedArtifact.Role+"_size_changed", fmt.Sprintf("%s size changed from %d to %d bytes", artifactIdentity, *cachedArtifact.SizeBytes, fileInfo.Size()))
	}
	if fileInfo.ModTime().UnixNano() != cachedArtifact.ModificationTime.UnixNano() {
		return cacheMiss(cachedArtifact.Role+"_mtime_changed", fmt.Sprintf("%s modification time changed", artifactIdentity))
	}
	return CacheDecision{Hit: true, Decision: "hit"}
}

func cacheMiss(reasonCode string, detail string) CacheDecision {
	return CacheDecision{Decision: "miss", ReasonCode: reasonCode, Detail: detail}
}

func abbreviatedFingerprint(fingerprint string) string {
	if len(fingerprint) <= 12 {
		return fingerprint
	}
	return fingerprint[:12]
}

func expectedTaskArtifacts(projectDirectory string, inputs map[string][]string, outputs map[string]string) map[string]Artifact {
	expectedArtifacts := make(map[string]Artifact)
	inputNames := make([]string, 0, len(inputs))
	for inputName := range inputs {
		inputNames = append(inputNames, inputName)
	}
	sort.Strings(inputNames)
	for _, inputName := range inputNames {
		inputPaths := inputs[inputName]
		for inputIndex, inputPath := range inputPaths {
			artifactName := inputName
			if len(inputPaths) > 1 {
				artifactName = fmt.Sprintf("%s[%d]", inputName, inputIndex)
			}
			resolvedPath := resolveArtifactPath(projectDirectory, inputPath)
			expectedArtifacts["input:"+artifactName] = Artifact{Role: "input", Name: artifactName, Path: resolvedPath}
		}
	}
	for outputName, outputPath := range outputs {
		resolvedPath := resolveArtifactPath(projectDirectory, outputPath)
		expectedArtifacts["output:"+outputName] = Artifact{Role: "output", Name: outputName, Path: resolvedPath}
	}
	return expectedArtifacts
}

func resolveArtifactPath(projectDirectory, artifactPath string) string {
	if artifactPath == "" {
		return ""
	}
	resolvedPath := artifactPath
	if !filepath.IsAbs(resolvedPath) {
		resolvedPath = filepath.Join(projectDirectory, resolvedPath)
	}
	return filepath.Clean(resolvedPath)
}

func (stateStore *Store) CreateSubmission(ctx context.Context, submission Submission) error {
	_, err := stateStore.database.ExecContext(ctx, `
		INSERT INTO physical_submissions(submission_id, run_id, backend, scope, group_key, backend_job_id, resources_json, status, started_at, raw_metadata_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, submission.ID, submission.RunID, submission.Backend, submission.Scope, submission.GroupKey, submission.BackendJobID, string(submission.Resources), submission.Status, nullableTime(submission.StartedAt), string(submission.RawMetadata))
	if err != nil {
		return fmt.Errorf("create submission: %w", err)
	}
	return nil
}

func (stateStore *Store) UpdateSubmissionStarted(ctx context.Context, submissionID, backendJobID string, metadata json.RawMessage) error {
	_, err := stateStore.database.ExecContext(ctx, `
		UPDATE physical_submissions SET backend_job_id=?, raw_metadata_json=? WHERE submission_id=?
	`, backendJobID, string(metadata), submissionID)
	if err != nil {
		return fmt.Errorf("update started submission: %w", err)
	}
	return nil
}

func (stateStore *Store) CancelRun(ctx context.Context, runID string, finishedAt time.Time) error {
	return stateStore.WithTransaction(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `UPDATE task_attempts SET status='cancelled', finished_at=? WHERE run_id=? AND status='running'`, formatTime(finishedAt), runID); err != nil {
			return fmt.Errorf("cancel running task attempts: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE task_instances SET status='cancelled' WHERE run_id=? AND status IN ('pending', 'running')`, runID); err != nil {
			return fmt.Errorf("cancel task instances: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE physical_submissions SET status='cancelled', finished_at=? WHERE run_id=? AND status='running'`, formatTime(finishedAt), runID); err != nil {
			return fmt.Errorf("cancel physical submissions: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE runs SET status='cancelled', finished_at=? WHERE run_id=?`, formatTime(finishedAt), runID); err != nil {
			return fmt.Errorf("cancel run: %w", err)
		}
		return nil
	})
}

func (stateStore *Store) RunStatus(ctx context.Context, runID string) (string, error) {
	var status string
	if err := stateStore.database.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=?`, runID).Scan(&status); err != nil {
		return "", fmt.Errorf("load run status: %w", err)
	}
	return status, nil
}

func (stateStore *Store) RunningSubmissions(ctx context.Context, runID string) ([]Submission, error) {
	rows, err := stateStore.database.QueryContext(ctx, `
		SELECT submission_id, run_id, backend, scope, group_key, backend_job_id, resources_json, status, started_at, finished_at, raw_metadata_json
		FROM physical_submissions WHERE run_id=? AND status='running' ORDER BY submission_id
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("query running submissions: %w", err)
	}
	defer rows.Close()
	var submissions []Submission
	for rows.Next() {
		var submission Submission
		var resources string
		var metadata sql.NullString
		var startedAt sql.NullString
		var finishedAt sql.NullString
		if err := rows.Scan(&submission.ID, &submission.RunID, &submission.Backend, &submission.Scope, &submission.GroupKey, &submission.BackendJobID, &resources, &submission.Status, &startedAt, &finishedAt, &metadata); err != nil {
			return nil, err
		}
		submission.Resources = json.RawMessage(resources)
		if metadata.Valid && metadata.String != "" {
			submission.RawMetadata = json.RawMessage(metadata.String)
		}
		if startedAt.Valid {
			parsed, parseErr := time.Parse(time.RFC3339Nano, startedAt.String)
			if parseErr != nil {
				return nil, fmt.Errorf("parse submission start time: %w", parseErr)
			}
			submission.StartedAt = &parsed
		}
		if finishedAt.Valid {
			parsed, parseErr := time.Parse(time.RFC3339Nano, finishedAt.String)
			if parseErr != nil {
				return nil, fmt.Errorf("parse submission finish time: %w", parseErr)
			}
			submission.FinishedAt = &parsed
		}
		submissions = append(submissions, submission)
	}
	return submissions, rows.Err()
}

func (stateStore *Store) RunningAttempts(ctx context.Context, runID string) ([]TaskAttempt, error) {
	rows, err := stateStore.database.QueryContext(ctx, `
		SELECT attempt_id, run_id, task_id, attempt_number, submission_id, status, started_at, finished_at,
		       exit_code, signal, failure_reason, stdout_path, stderr_path, result_path
		FROM task_attempts
		WHERE run_id = ? AND status = 'running'
		ORDER BY submission_id, task_id, attempt_number
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("query running task attempts: %w", err)
	}
	defer rows.Close()

	var attempts []TaskAttempt
	for rows.Next() {
		var attempt TaskAttempt
		var submissionID, startedAt, finishedAt, signal, failureReason, stdoutPath, stderrPath, resultPath sql.NullString
		var exitCode sql.NullInt64
		if err := rows.Scan(
			&attempt.ID,
			&attempt.RunID,
			&attempt.TaskID,
			&attempt.AttemptNumber,
			&submissionID,
			&attempt.Status,
			&startedAt,
			&finishedAt,
			&exitCode,
			&signal,
			&failureReason,
			&stdoutPath,
			&stderrPath,
			&resultPath,
		); err != nil {
			return nil, fmt.Errorf("scan running task attempt: %w", err)
		}
		attempt.SubmissionID = submissionID.String
		attempt.Signal = signal.String
		attempt.FailureReason = failureReason.String
		attempt.StdoutPath = stdoutPath.String
		attempt.StderrPath = stderrPath.String
		attempt.ResultPath = resultPath.String
		if exitCode.Valid {
			exitCodeValue := int(exitCode.Int64)
			attempt.ExitCode = &exitCodeValue
		}
		if startedAt.Valid {
			parsedStartedAt, parseErr := time.Parse(time.RFC3339Nano, startedAt.String)
			if parseErr != nil {
				return nil, fmt.Errorf("parse task attempt start time: %w", parseErr)
			}
			attempt.StartedAt = &parsedStartedAt
		}
		if finishedAt.Valid {
			parsedFinishedAt, parseErr := time.Parse(time.RFC3339Nano, finishedAt.String)
			if parseErr != nil {
				return nil, fmt.Errorf("parse task attempt finish time: %w", parseErr)
			}
			attempt.FinishedAt = &parsedFinishedAt
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read running task attempts: %w", err)
	}
	return attempts, nil
}

func (stateStore *Store) InterruptAttempt(ctx context.Context, attempt TaskAttempt, reason string, finishedAt time.Time) error {
	return stateStore.WithTransaction(ctx, func(transaction *sql.Tx) error {
		result, err := transaction.ExecContext(ctx, `
			UPDATE task_attempts
			SET status = 'interrupted', finished_at = ?, failure_reason = ?
			WHERE attempt_id = ? AND status = 'running'
		`, formatTime(finishedAt), reason, attempt.ID)
		if err != nil {
			return fmt.Errorf("interrupt task attempt %q: %w", attempt.ID, err)
		}
		updatedRows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("inspect interrupted task attempt %q: %w", attempt.ID, err)
		}
		if updatedRows == 0 {
			return nil
		}
		if _, err := transaction.ExecContext(ctx, `
			UPDATE task_instances
			SET status = 'interrupted'
			WHERE run_id = ? AND task_id = ? AND status = 'running'
		`, attempt.RunID, attempt.TaskID); err != nil {
			return fmt.Errorf("interrupt task instance %q: %w", attempt.TaskID, err)
		}
		return nil
	})
}

func (stateStore *Store) FinalizeRecoveredRun(ctx context.Context, runID, interruptionReason string, finishedAt time.Time) (string, error) {
	finalStatus := "succeeded"
	err := stateStore.WithTransaction(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `
			UPDATE task_attempts
			SET status = 'interrupted', finished_at = ?, failure_reason = ?
			WHERE run_id = ? AND status = 'running'
		`, formatTime(finishedAt), interruptionReason, runID); err != nil {
			return fmt.Errorf("interrupt remaining task attempts: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, `
			UPDATE task_instances
			SET status = 'interrupted'
			WHERE run_id = ? AND status IN ('pending', 'running')
		`, runID); err != nil {
			return fmt.Errorf("interrupt remaining task instances: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, `
			UPDATE physical_submissions
			SET status = 'interrupted', finished_at = ?
			WHERE run_id = ? AND status = 'running'
		`, formatTime(finishedAt), runID); err != nil {
			return fmt.Errorf("interrupt remaining physical submissions: %w", err)
		}

		var nonSuccessfulTasks int
		if err := transaction.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM task_instances
			WHERE run_id = ? AND status NOT IN ('succeeded', 'cached')
		`, runID).Scan(&nonSuccessfulTasks); err != nil {
			return fmt.Errorf("count non-successful recovered tasks: %w", err)
		}
		if nonSuccessfulTasks > 0 {
			finalStatus = "failed"
		}
		if _, err := transaction.ExecContext(ctx, `
			UPDATE runs SET status = ?, finished_at = ? WHERE run_id = ?
		`, finalStatus, formatTime(finishedAt), runID); err != nil {
			return fmt.Errorf("finalize recovered run: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return finalStatus, nil
}

func (stateStore *Store) FinishSubmission(ctx context.Context, submissionID, status, backendJobID string, finishedAt time.Time, metadata json.RawMessage) error {
	_, err := stateStore.database.ExecContext(ctx, `
		UPDATE physical_submissions SET status=?, backend_job_id=?, finished_at=?, raw_metadata_json=? WHERE submission_id=?
	`, status, backendJobID, formatTime(finishedAt), string(metadata), submissionID)
	if err != nil {
		return fmt.Errorf("finish submission: %w", err)
	}
	return nil
}

func (stateStore *Store) CreateAttempt(ctx context.Context, attempt TaskAttempt) error {
	_, err := stateStore.database.ExecContext(ctx, `
		INSERT INTO task_attempts(attempt_id, run_id, task_id, attempt_number, submission_id, status, started_at, stdout_path, stderr_path, result_path)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, attempt.ID, attempt.RunID, attempt.TaskID, attempt.AttemptNumber, nullableString(attempt.SubmissionID), attempt.Status, nullableTime(attempt.StartedAt), attempt.StdoutPath, attempt.StderrPath, attempt.ResultPath)
	if err != nil {
		return fmt.Errorf("create task attempt: %w", err)
	}
	return nil
}

func (stateStore *Store) FinishAttempt(ctx context.Context, attemptID, status string, result *protocol.TaskResult, artifacts []Artifact) error {
	return stateStore.WithTransaction(ctx, func(transaction *sql.Tx) error {
		_, err := transaction.ExecContext(ctx, `
			UPDATE task_attempts SET status=?, finished_at=?, exit_code=?, failure_reason=? WHERE attempt_id=?
		`, status, formatTime(result.FinishedAt), result.ExitCode, result.Error, attemptID)
		if err != nil {
			return fmt.Errorf("finish task attempt: %w", err)
		}
		for _, step := range result.Steps {
			wallSeconds := step.FinishedAt.Sub(step.StartedAt).Seconds()
			_, err := transaction.ExecContext(ctx, `
				INSERT OR REPLACE INTO step_attempts(step_attempt_id, attempt_id, step_index, step_name, environment, started_at, finished_at, wall_seconds, exit_code, stdout_path, stderr_path)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, fmt.Sprintf("%s-step-%03d", attemptID, step.Index), attemptID, step.Index, step.Name, step.Environment, formatTime(step.StartedAt), formatTime(step.FinishedAt), wallSeconds, step.ExitCode, step.StdoutPath, step.StderrPath)
			if err != nil {
				return fmt.Errorf("record step attempt: %w", err)
			}
		}
		for artifactIndex, artifact := range artifacts {
			artifactID := artifact.ID
			if artifactID == "" {
				artifactID = fmt.Sprintf("%s-artifact-%03d", attemptID, artifactIndex+1)
			}
			var sizeBytes any
			if artifact.SizeBytes != nil {
				sizeBytes = *artifact.SizeBytes
			}
			var modificationTimeNanoseconds any
			if artifact.ModificationTime != nil {
				modificationTimeNanoseconds = artifact.ModificationTime.UnixNano()
			}
			_, err := transaction.ExecContext(ctx, `
				INSERT OR REPLACE INTO artifacts(artifact_id, attempt_id, role, name, path, size_bytes, mtime_ns, digest, validation_status)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, artifactID, attemptID, artifact.Role, artifact.Name, artifact.Path, sizeBytes, modificationTimeNanoseconds, nullableString(artifact.Digest), artifact.ValidationStatus)
			if err != nil {
				return fmt.Errorf("record task artifact %q: %w", artifact.Name, err)
			}
		}
		return nil
	})
}

func (stateStore *Store) SaveMetrics(ctx context.Context, attemptID string, collected *metrics.TaskMetrics, allocatedCPUs int64, requestedMemory int64) error {
	raw, _ := json.Marshal(collected.Raw)
	_, err := stateStore.database.ExecContext(ctx, `
		INSERT OR REPLACE INTO task_metrics(attempt_id, source, quality, wall_seconds, user_cpu_seconds, system_cpu_seconds,
		allocated_cpus, requested_memory_bytes, max_rss_bytes, disk_read_bytes, disk_write_bytes,
		filesystem_input_operations, filesystem_output_operations, input_artifact_bytes, output_artifact_bytes,
		major_page_faults, minor_page_faults, voluntary_context_switches, involuntary_context_switches, raw_metrics_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, attemptID, collected.Source, collected.Quality, collected.WallSeconds, collected.UserCPUSeconds, collected.SystemCPUSeconds,
		allocatedCPUs, requestedMemory, collected.MaxRSSBytes, collected.DiskReadBytes, collected.DiskWriteBytes,
		collected.FilesystemInputOperations, collected.FilesystemOutputOperations, collected.InputArtifactBytes, collected.OutputArtifactBytes,
		collected.MajorPageFaults, collected.MinorPageFaults, collected.VoluntaryContextSwitches, collected.InvoluntaryContextSwitches, string(raw))
	if err != nil {
		return fmt.Errorf("save task metrics: %w", err)
	}
	return nil
}

func (stateStore *Store) SaveRuntimeIncident(ctx context.Context, incident RuntimeIncident) error {
	if incident.ID == "" || incident.RunID == "" || incident.AttemptID == "" {
		return fmt.Errorf("runtime incident id, run id, and attempt id are required")
	}
	diagnosticPaths, err := json.Marshal(incident.DiagnosticPaths)
	if err != nil {
		return fmt.Errorf("encode runtime incident diagnostic paths: %w", err)
	}
	evidencePaths, err := json.Marshal(incident.EvidencePaths)
	if err != nil {
		return fmt.Errorf("encode runtime incident evidence paths: %w", err)
	}
	_, err = stateStore.database.ExecContext(ctx, `
		INSERT OR REPLACE INTO runtime_incidents(
			incident_id, run_id, attempt_id, schema_version, category, scope, retry_safe,
			retry_policy, owner, escalation, remediation_status, summary, first_observed_at,
			executor, backend, backend_job_id, exit_code, signal, diagnostic_paths_json, evidence_paths_json
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, incident.ID, incident.RunID, incident.AttemptID, incident.SchemaVersion, incident.Category, incident.Scope,
		incident.RetrySafe, incident.RetryPolicy, incident.Owner, incident.Escalation, incident.RemediationStatus,
		incident.Summary, formatTime(incident.FirstObservedAt), nullableString(incident.Executor), nullableString(incident.Backend),
		nullableString(incident.BackendJobID), incident.ExitCode, nullableString(incident.Signal), string(diagnosticPaths), string(evidencePaths))
	if err != nil {
		return fmt.Errorf("save runtime incident %q: %w", incident.ID, err)
	}
	return nil
}

func (stateStore *Store) ListRuntimeIncidents(ctx context.Context, runID string) ([]RuntimeIncident, error) {
	rows, err := stateStore.database.QueryContext(ctx, `
		SELECT incident_id, run_id, attempt_id, schema_version, category, scope, retry_safe,
		       retry_policy, owner, escalation, remediation_status, summary, first_observed_at,
		       COALESCE(executor, ''), COALESCE(backend, ''), COALESCE(backend_job_id, ''),
		       exit_code, COALESCE(signal, ''), COALESCE(diagnostic_paths_json, '[]'), COALESCE(evidence_paths_json, '[]')
		FROM runtime_incidents WHERE run_id = ? ORDER BY first_observed_at, incident_id
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list runtime incidents: %w", err)
	}
	defer rows.Close()

	incidents := []RuntimeIncident{}
	for rows.Next() {
		var incident RuntimeIncident
		var firstObservedAt string
		var retrySafe int
		var diagnosticPaths, evidencePaths string
		if err := rows.Scan(&incident.ID, &incident.RunID, &incident.AttemptID, &incident.SchemaVersion, &incident.Category,
			&incident.Scope, &retrySafe, &incident.RetryPolicy, &incident.Owner, &incident.Escalation,
			&incident.RemediationStatus, &incident.Summary, &firstObservedAt, &incident.Executor, &incident.Backend,
			&incident.BackendJobID, &incident.ExitCode, &incident.Signal, &diagnosticPaths, &evidencePaths); err != nil {
			return nil, fmt.Errorf("scan runtime incident: %w", err)
		}
		incident.RetrySafe = retrySafe != 0
		if incident.FirstObservedAt, err = time.Parse(time.RFC3339Nano, firstObservedAt); err != nil {
			return nil, fmt.Errorf("parse runtime incident timestamp: %w", err)
		}
		if err := json.Unmarshal([]byte(diagnosticPaths), &incident.DiagnosticPaths); err != nil {
			return nil, fmt.Errorf("decode runtime incident diagnostic paths: %w", err)
		}
		if err := json.Unmarshal([]byte(evidencePaths), &incident.EvidencePaths); err != nil {
			return nil, fmt.Errorf("decode runtime incident evidence paths: %w", err)
		}
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read runtime incidents: %w", err)
	}
	return incidents, nil
}

func (stateStore *Store) RefreshableMetrics(ctx context.Context, runID string) ([]MetricRefreshCandidate, error) {
	rows, err := stateStore.database.QueryContext(ctx, `
		SELECT task_attempts.attempt_id, COALESCE(task_attempts.result_path, '')
		FROM runs
		JOIN task_attempts ON task_attempts.run_id = runs.run_id
		JOIN task_metrics ON task_metrics.attempt_id = task_attempts.attempt_id
		WHERE runs.run_id = ?
		  AND runs.backend = 'slurm'
		  AND task_attempts.status <> 'running'
		  AND task_metrics.source = 'slurm_sacct'
		  AND COALESCE(task_metrics.quality, '') <> 'accounting'
		ORDER BY task_attempts.task_id, task_attempts.attempt_number
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("query refreshable task metrics: %w", err)
	}
	defer rows.Close()

	var candidates []MetricRefreshCandidate
	for rows.Next() {
		var candidate MetricRefreshCandidate
		if err := rows.Scan(&candidate.AttemptID, &candidate.ResultPath); err != nil {
			return nil, fmt.Errorf("scan refreshable task metrics: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read refreshable task metrics: %w", err)
	}
	return candidates, nil
}

func (stateStore *Store) SaveRefreshedMetrics(ctx context.Context, attemptID string, collected *metrics.TaskMetrics) error {
	if collected == nil {
		return fmt.Errorf("save refreshed task metrics for attempt %q: metrics are required", attemptID)
	}
	raw, _ := json.Marshal(collected.Raw)
	result, err := stateStore.database.ExecContext(ctx, `
		UPDATE task_metrics
		SET source = ?, quality = ?, wall_seconds = ?, user_cpu_seconds = ?, system_cpu_seconds = ?,
		    allocated_cpus = COALESCE(?, allocated_cpus), max_rss_bytes = ?, disk_read_bytes = ?, disk_write_bytes = ?,
		    filesystem_input_operations = ?, filesystem_output_operations = ?, major_page_faults = ?, minor_page_faults = ?,
		    voluntary_context_switches = ?, involuntary_context_switches = ?, raw_metrics_json = ?
		WHERE attempt_id = ?
	`, collected.Source, collected.Quality, collected.WallSeconds, collected.UserCPUSeconds, collected.SystemCPUSeconds,
		collected.AllocatedCPUs, collected.MaxRSSBytes, collected.DiskReadBytes, collected.DiskWriteBytes,
		collected.FilesystemInputOperations, collected.FilesystemOutputOperations, collected.MajorPageFaults, collected.MinorPageFaults,
		collected.VoluntaryContextSwitches, collected.InvoluntaryContextSwitches, string(raw), attemptID)
	if err != nil {
		return fmt.Errorf("save refreshed task metrics for attempt %q: %w", attemptID, err)
	}
	updatedRows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect refreshed task metrics for attempt %q: %w", attemptID, err)
	}
	if updatedRows != 1 {
		return fmt.Errorf("save refreshed task metrics for attempt %q: expected one metrics row, updated %d", attemptID, updatedRows)
	}
	return nil
}

func (stateStore *Store) LatestRunID(ctx context.Context) (string, error) {
	var runID string
	err := stateStore.database.QueryRowContext(ctx, `SELECT run_id FROM runs ORDER BY started_at DESC LIMIT 1`).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("find latest run: %w", err)
	}
	return runID, nil
}

func (stateStore *Store) RunSummary(ctx context.Context, runID string) (Run, map[string]int, error) {
	var run Run
	var finished sql.NullString
	var resumedFromRunID sql.NullString
	var startedAt string
	err := stateStore.database.QueryRowContext(ctx, `
		SELECT run_id, workflow, phase, config_path, config_digest, workflow_path, workflow_digest, backend, craftmake_version, resumed_from_run_id, loader_kind, status, started_at, finished_at
		FROM runs WHERE run_id = ?
	`, runID).Scan(&run.ID, &run.Workflow, &run.Phase, &run.ConfigPath, &run.ConfigDigest, &run.WorkflowPath, &run.WorkflowDigest, &run.Backend, &run.CraftmakeVersion, &resumedFromRunID, &run.LoaderKind, &run.Status, &startedAt, &finished)
	if err != nil {
		return run, nil, fmt.Errorf("load run summary: %w", err)
	}
	parsedStartedAt, parseStartedAtErr := time.Parse(time.RFC3339Nano, startedAt)
	if parseStartedAtErr != nil {
		return run, nil, fmt.Errorf("parse run start time: %w", parseStartedAtErr)
	}
	run.StartedAt = parsedStartedAt
	if resumedFromRunID.Valid {
		run.ResumedFromRunID = resumedFromRunID.String
	}
	if finished.Valid {
		parsed, _ := time.Parse(time.RFC3339Nano, finished.String)
		run.FinishedAt = &parsed
	}
	rows, err := stateStore.database.QueryContext(ctx, `SELECT status, COUNT(*) FROM task_instances WHERE run_id=? GROUP BY status`, runID)
	if err != nil {
		return run, nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return run, nil, err
		}
		counts[status] = count
	}
	return run, counts, rows.Err()
}

func (stateStore *Store) QueryRows(ctx context.Context, query string, arguments ...any) (*sql.Rows, error) {
	return stateStore.database.QueryContext(ctx, query, arguments...)
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
