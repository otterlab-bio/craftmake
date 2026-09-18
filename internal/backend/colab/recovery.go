package colab

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/otterlab-bio/craftmake/internal/backend"
)

// RemoteResultReader reads a result file from the durable remote workspace.
type RemoteResultReader interface {
	ReadResultFile(context.Context, string) ([]byte, error)
}

// DriveResultReader reads result files through a DriveFileClient.
type DriveResultReader struct{ Client *DriveFileClient }

func (r DriveResultReader) ReadResultFile(ctx context.Context, path string) ([]byte, error) {
	if r.Client == nil {
		return nil, fmt.Errorf("Drive result reader requires a Drive file client")
	}
	return r.Client.Get(ctx, path)
}

// RecoverSubmission reconstructs completed task outcomes from durable remote
// result files without restarting the runtime. It only accepts a validated
// TaskResult that matches the manifest; a missing or mismatched result is
// reported as failed rather than inferred successful.
func (b *Backend) RecoverSubmission(ctx context.Context, request backend.RecoveryRequest) (*backend.SubmissionResult, error) {
	if b.ResultReader == nil {
		return nil, fmt.Errorf("Colab result reader is required for recovery")
	}
	result := &backend.SubmissionResult{BackendID: "colab:" + request.SubmissionID, Tasks: map[string]backend.TaskOutcome{}}
	for _, manifest := range request.Manifests {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		mapping := RemoteTaskMapping{WorkDirectory: filepath.Join(b.remoteRoot(), "work"), TempDirectory: filepath.Join(b.scratchRoot(), "tmp"), RuntimeDirectory: filepath.Join(b.remoteRoot(), "runtime", manifest.TaskID), ResultPath: filepath.Join(b.remoteRoot(), "runtime", manifest.TaskID, "result.json")}
		raw, err := b.ResultReader.ReadResultFile(ctx, mapping.ResultPath)
		if err != nil {
			result.Tasks[manifest.TaskID] = backend.TaskOutcome{Err: fmt.Errorf("recover Colab task %s: %w", manifest.TaskID, err)}
			continue
		}
		taskResult, err := DecodeTaskResult(string(raw))
		if err != nil {
			result.Tasks[manifest.TaskID] = backend.TaskOutcome{Err: err}
			continue
		}
		if taskResult.RunID != manifest.RunID || taskResult.TaskID != manifest.TaskID || taskResult.Attempt != manifest.Attempt {
			result.Tasks[manifest.TaskID] = backend.TaskOutcome{Err: fmt.Errorf("remote result does not match manifest for %s", manifest.TaskID)}
			continue
		}
		if b.Materializer != nil {
			if err := b.materializeTaskLogs(ctx, taskResult, manifest, mapping, ""); err != nil {
				taskResult.ObservabilityErrors = append(taskResult.ObservabilityErrors, err.Error())
			}
		}
		result.Tasks[manifest.TaskID] = backend.TaskOutcome{Result: &backend.Result{TaskResult: taskResult, BackendID: "colab:" + manifest.TaskID}}
	}
	return result, nil
}
