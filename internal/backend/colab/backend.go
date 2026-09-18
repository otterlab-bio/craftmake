package colab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type RuntimeRequest struct {
	RunID       string
	Region      string
	Accelerator string
}
type Runtime struct {
	ID         string
	ProxyURL   string
	ProxyToken string
}

type ControlPlane interface {
	AcquireRuntime(context.Context, RuntimeRequest) (Runtime, error)
	ReleaseRuntime(context.Context, Runtime) error
	ListAssignments(context.Context) ([]Assignment, error)
}
type NotebookExecutor interface {
	ExecuteNotebook(context.Context, Runtime, []byte) (string, error)
}

type DriveMountRequest struct {
	RunID          string
	SessionID      string
	AuthConfigPath string
	MountPath      string
	DriveRoot      string
}
type DriveMountPreflight interface {
	CheckMount(context.Context, DriveMountRequest) error
}
type LogMaterializer interface {
	Materialize(context.Context, string, string) error
}

type Config struct {
	RemoteRoot         string
	ScratchRoot        string
	DriveRoot          string
	MountPath          string
	AuthConfigPath     string
	SessionID          string
	LocalRoot          string
	SyncExcludes       []string
	DefaultAccelerator string
}

type Backend struct {
	Config         Config
	Control        ControlPlane
	Executor       NotebookExecutor
	MountPreflight DriveMountPreflight
	Materializer   LogMaterializer
	Workspace      WorkspaceSyncer
	ResultReader   RemoteResultReader
	Redactor       *Redactor
}

func (b *Backend) Name() string { return "colab" }

func (b *Backend) BeginRun(ctx context.Context, run backend.RunContext) error {
	if b.Control == nil || b.Executor == nil {
		return fmt.Errorf("colab control plane and notebook executor are required")
	}
	config := b.Config
	if config.LocalRoot == "" {
		config.LocalRoot = run.ProjectDirectory
	}
	if config.AuthConfigPath != "" {
		auth, err := LoadSessionAuth(config.AuthConfigPath, config.SessionID)
		if err != nil {
			return fmt.Errorf("load Colab session auth: %w", err)
		}
		if config.DriveRoot == "" {
			config.DriveRoot = auth.DriveRoot
		}
		if config.MountPath == "" {
			config.MountPath = auth.MountPath
		}
	}
	if config.DefaultAccelerator == "" {
		config.DefaultAccelerator = "cpu"
	}
	b.Config = config
	// Validate Drive authorization up front (a credential check, not a runtime
	// assignment). Instances are acquired per-submission in RunSubmission and
	// released immediately after each manifest executes, because Google Drive
	// is the durable shared state between jobs.
	if config.DriveRoot != "" {
		if b.MountPreflight == nil {
			return fmt.Errorf("drive mount preflight is required when drive root is configured")
		}
		mountPath := config.MountPath
		if mountPath == "" {
			mountPath = "/content/drive"
		}
		if err := b.MountPreflight.CheckMount(ctx, DriveMountRequest{RunID: run.RunID, SessionID: config.SessionID, AuthConfigPath: config.AuthConfigPath, MountPath: mountPath, DriveRoot: config.DriveRoot}); err != nil {
			return &MountNotAuthorizedError{SessionID: config.SessionID, AuthConfigPath: config.AuthConfigPath, MountPath: mountPath, DriveRoot: config.DriveRoot, Err: err}
		}
	}
	// Sync the local project into the durable Drive workspace once at run start.
	if b.Workspace != nil && config.LocalRoot != "" {
		target := config.DriveRoot
		if target == "" {
			target = b.remoteRoot()
		}
		excludes := config.SyncExcludes
		if len(excludes) == 0 {
			excludes = []string{".craftmake/state", ".git"}
		}
		if err := b.Workspace.SyncIn(ctx, WorkspaceSyncRequest{RunID: run.RunID, LocalRoot: config.LocalRoot, RemoteRoot: target, Direction: "in", Excludes: excludes}); err != nil {
			return fmt.Errorf("sync workspace to Colab: %w", err)
		}
	}
	return nil
}

func (b *Backend) EndRun(ctx context.Context, outcome backend.RunOutcome) error {
	var syncErr error
	config := b.Config
	if config.AuthConfigPath != "" {
		if auth, err := LoadSessionAuth(config.AuthConfigPath, config.SessionID); err != nil {
			syncErr = fmt.Errorf("load Colab session auth for sync-out: %w", err)
		} else {
			if config.DriveRoot == "" {
				config.DriveRoot = auth.DriveRoot
			}
			if config.MountPath == "" {
				config.MountPath = auth.MountPath
			}
		}
	}
	if syncErr == nil && b.Workspace != nil && outcome.ProjectDirectory != "" {
		target := config.DriveRoot
		if target == "" {
			target = b.remoteRoot()
		}
		excludes := config.SyncExcludes
		if len(excludes) == 0 {
			excludes = []string{".craftmake/state", ".git"}
		}
		syncErr = b.Workspace.SyncOut(ctx, WorkspaceSyncRequest{RunID: outcome.RunID, LocalRoot: target, RemoteRoot: outcome.ProjectDirectory, Direction: "out", Excludes: excludes})
	}
	// Defensive cleanup: instances are released per-submission in RunSubmission,
	// but scan for and release any residual assignments to guarantee zero leakage.
	if b.Control != nil {
		if assignments, err := b.Control.ListAssignments(ctx); err == nil {
			for _, a := range assignments {
				_ = b.Control.ReleaseRuntime(ctx, Runtime{ID: a.Endpoint})
			}
		}
	}
	if syncErr != nil {
		return fmt.Errorf("sync workspace from Colab: %w", syncErr)
	}
	return nil
}

func (b *Backend) RunSubmission(ctx context.Context, submissionID string, request backend.SubmissionRequest) (*backend.SubmissionResult, error) {
	result := &backend.SubmissionResult{BackendID: "colab:" + submissionID, Tasks: map[string]backend.TaskOutcome{}}
	for _, manifest := range request.Manifests {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// 1. Determine the accelerator type for this manifest.
		accelerator := manifest.Resources.Accelerator
		if accelerator == "" {
			accelerator = b.Config.DefaultAccelerator
		}
		if accelerator == "" {
			accelerator = "cpu"
		}
		// 2. Acquire a fresh ephemeral instance of the requested type.
		runtime, err := b.Control.AcquireRuntime(ctx, RuntimeRequest{RunID: manifest.RunID, Accelerator: accelerator})
		if err != nil {
			result.Tasks[manifest.TaskID] = backend.TaskOutcome{Err: RedactError(b.Redactor, err)}
			continue
		}
		// 3. Execute the manifest on this instance.
		outcome := b.executeOnRuntime(ctx, runtime, manifest, request.OnStarted)
		// 4. Release the instance immediately — Drive is the durable shared state.
		if releaseErr := b.Control.ReleaseRuntime(ctx, runtime); releaseErr != nil {
			if outcome.Err == nil {
				outcome.Err = RedactError(b.Redactor, fmt.Errorf("release Colab runtime: %w", releaseErr))
			}
		}
		result.Tasks[manifest.TaskID] = outcome
	}
	return result, nil
}

// executeOnRuntime builds and runs a single manifest on the given runtime,
// materializing logs and decoding the task result. It does not manage the
// runtime lifecycle; the caller is responsible for release.
func (b *Backend) executeOnRuntime(ctx context.Context, runtime Runtime, manifest *protocol.TaskManifest, onStarted func(string, map[string]any) error) backend.TaskOutcome {
	mapping := RemoteTaskMapping{WorkDirectory: filepath.Join(b.remoteRoot(), "work"), TempDirectory: filepath.Join(b.scratchRoot(), "tmp"), RuntimeDirectory: filepath.Join(b.remoteRoot(), "runtime", manifest.TaskID), ResultPath: filepath.Join(b.remoteRoot(), "runtime", manifest.TaskID, "result.json")}
	notebook, err := BuildNotebookRedacted(manifest, mapping, b.Redactor)
	if err != nil {
		return backend.TaskOutcome{Err: RedactError(b.Redactor, err)}
	}
	payload, err := notebook.JSON()
	if err != nil {
		return backend.TaskOutcome{Err: RedactError(b.Redactor, err)}
	}
	if onStarted != nil {
		if err := onStarted("colab:"+manifest.TaskID, map[string]any{"runtime_id": runtime.ID}); err != nil {
			return backend.TaskOutcome{Err: err}
		}
	}
	output, err := b.Executor.ExecuteNotebook(ctx, runtime, payload)
	if err != nil {
		return backend.TaskOutcome{Err: RedactError(b.Redactor, err)}
	}
	taskResult, err := DecodeTaskResult(output)
	if err != nil {
		return backend.TaskOutcome{Err: RedactError(b.Redactor, fmt.Errorf("%w; raw kernel output: %q", err, output))}
	}
	if err := b.materializeTaskLogs(ctx, taskResult, manifest, mapping, output); err != nil {
		taskResult.ObservabilityErrors = append(taskResult.ObservabilityErrors, err.Error())
	}
	return backend.TaskOutcome{Result: &backend.Result{TaskResult: taskResult, BackendID: "colab:" + manifest.TaskID}}
}

func (b *Backend) materializeTaskLogs(ctx context.Context, taskResult *protocol.TaskResult, manifest *protocol.TaskManifest, mapping RemoteTaskMapping, output string) error {
	var failures []string
	for _, step := range taskResult.Steps {
		if step.Index < 0 || step.Index >= len(manifest.Steps) {
			continue
		}
		manifestStep := manifest.Steps[step.Index]
		remoteStdout := step.StdoutPath
		if remoteStdout == "" {
			remoteStdout = filepath.Join(mapping.RuntimeDirectory, fmt.Sprintf("step-%d.stdout", step.Index))
		}
		remoteStderr := step.StderrPath
		if remoteStderr == "" {
			remoteStderr = filepath.Join(mapping.RuntimeDirectory, fmt.Sprintf("step-%d.stderr", step.Index))
		}
		localStdout := manifestStep.StdoutPath
		localStderr := manifestStep.StderrPath
		if localStdout == "" {
			localStdout = filepath.Join(manifest.RuntimeDirectory, fmt.Sprintf("step-%d.stdout", step.Index))
		}
		if localStderr == "" {
			localStderr = filepath.Join(manifest.RuntimeDirectory, fmt.Sprintf("step-%d.stderr", step.Index))
		}
		if b.Materializer != nil {
			if err := b.Materializer.Materialize(ctx, remoteStdout, localStdout); err != nil {
				failures = append(failures, fmt.Sprintf("stdout step %d: %v", step.Index, err))
			}
			if err := b.Materializer.Materialize(ctx, remoteStderr, localStderr); err != nil {
				failures = append(failures, fmt.Sprintf("stderr step %d: %v", step.Index, err))
			}
		} else {
			outText := extractStepLog(output, fmt.Sprintf("[step-%d stdout]", step.Index))
			if outText != "" && localStdout != "" {
				_ = os.MkdirAll(filepath.Dir(localStdout), 0o755)
				_ = os.WriteFile(localStdout, []byte(outText+"\n"), 0o644)
			}
			errText := extractStepLog(output, fmt.Sprintf("[step-%d stderr]", step.Index))
			if errText != "" && localStderr != "" {
				_ = os.MkdirAll(filepath.Dir(localStderr), 0o755)
				_ = os.WriteFile(localStderr, []byte(errText+"\n"), 0o644)
			}
		}
		step.StdoutPath, step.StderrPath = localStdout, localStderr
	}
	if manifest.ResultPath != "" {
		_ = os.MkdirAll(filepath.Dir(manifest.ResultPath), 0o755)
		if data, err := json.MarshalIndent(taskResult, "", "  "); err == nil {
			_ = os.WriteFile(manifest.ResultPath, data, 0o644)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("materialize Colab logs: %s", strings.Join(failures, "; "))
	}
	return nil
}

func extractStepLog(output, header string) string {
	idx := strings.Index(output, header+"\n")
	if idx < 0 {
		return ""
	}
	start := idx + len(header) + 1
	rest := output[start:]
	end := strings.Index(rest, "\n[step-")
	if end < 0 {
		end = strings.Index(rest, "\nCRAFTMAKE_TASK_RESULT_BEGIN")
	}
	if end >= 0 {
		return rest[:end]
	}
	return strings.TrimSpace(rest)
}

func (b *Backend) remoteRoot() string {
	if b.Config.DriveRoot != "" {
		return b.Config.DriveRoot
	}
	if b.Config.RemoteRoot != "" {
		return b.Config.RemoteRoot
	}
	return "/content/craftmake"
}
func (b *Backend) scratchRoot() string {
	if b.Config.ScratchRoot != "" {
		return b.Config.ScratchRoot
	}
	return "/content"
}

var _ backend.Backend = (*Backend)(nil)
var _ backend.RunLifecycle = (*Backend)(nil)
var _ backend.Recoverable = (*Backend)(nil)
