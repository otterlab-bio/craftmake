package colab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	// SyncIn/SyncOut enable the workspace archive sync performed through the
	// runtime's kernel (see NotebookOptions).
	SyncIn  bool
	SyncOut bool
	// DrivePreflight selects how a missing Drive authorization is handled
	// before a run starts: notify (default), strict, or off.
	DrivePreflight string
	// DriveMount selects how the runtime makes /content/drive usable: auto
	// (default), drivefs, or off.
	DriveMount string
	// DriveCredential backs the DriveFS mount. Nil disables mounting.
	DriveCredential *DriveMountCredential
	// DriveMountPath is the runtime mount point (default /content/drive).
	DriveMountPath string
	// DriveTransport selects how the workspace sync reaches Google Drive:
	// kernel (default, through the Jupyter kernel only) or rest (the Drive REST
	// API, durable in the account's Drive).
	DriveTransport string
	// DriveFolder is the Drive folder the workspace mirrors into, relative to
	// My Drive.
	DriveFolder string
	// DriveStore backs DriveTransport=rest. Nil disables it.
	DriveStore DriveStore
	// PathMap maps host path prefixes to remote path prefixes for workspace
	// sync, mirroring the action file's `colab.path_map`.
	PathMap map[string]string
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

	// syncMu guards syncInDone, which memoizes the once-per-run workspace
	// upload performed by syncWorkspaceIn.
	syncMu     sync.Mutex
	syncInDone bool
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
	config.DrivePreflight = NormalizeDrivePreflightMode(config.DrivePreflight)
	config.DriveMount = NormalizeDriveMountMode(config.DriveMount)
	config.DriveTransport = NormalizeDriveTransport(config.DriveTransport)
	b.Config = config
	// Validate Drive authorization up front. Instances are acquired per
	// submission in RunSubmission and released immediately after each manifest
	// executes, because Drive is the durable shared state between jobs.
	if config.DriveRoot != "" && config.DrivePreflight != DrivePreflightOff {
		if b.MountPreflight == nil {
			return fmt.Errorf("drive mount preflight is required when drive root is configured")
		}
		mountPath := config.MountPath
		if mountPath == "" {
			mountPath = "/content/drive"
		}
		if err := b.MountPreflight.CheckMount(ctx, DriveMountRequest{RunID: run.RunID, SessionID: config.SessionID, AuthConfigPath: config.AuthConfigPath, MountPath: mountPath, DriveRoot: config.DriveRoot}); err != nil {
			// Only a missing Drive consent is a mount-authorization problem; a
			// credential/control-plane failure must surface as itself instead of
			// being reported as "Drive is not authorized".
			var required *DriveAuthorizationRequiredError
			if !errors.As(err, &required) {
				return fmt.Errorf("Colab drive mount preflight failed: %w", err)
			}
			switch config.DrivePreflight {
			case DrivePreflightStrict:
				// Colab's Drive consent is bound to the runtime that requested
				// it, so a run that needs Drive has to authorize while it is
				// holding a runtime. Strict mode fails now instead of waiting.
				return &MountNotAuthorizedError{SessionID: config.SessionID, AuthConfigPath: config.AuthConfigPath, MountPath: mountPath, DriveRoot: config.DriveRoot, Err: err}
			default:
				// Advisory: the runtime-side consent prompt completes the
				// authorization while the run is waiting for it.
				fmt.Fprintf(os.Stderr, "notice: %v; continuing, the runtime will ask for Drive authorization when it is needed\n", err)
			}
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
		// 2b. Refresh the remote workspace from the local project once per run.
		if b.Config.SyncIn {
			if err := b.syncWorkspaceIn(ctx, runtime); err != nil {
				result.Tasks[manifest.TaskID] = backend.TaskOutcome{Err: RedactError(b.Redactor, err)}
				// Release the instance before reporting the failure; EndRun also
				// sweeps residual assignments.
				_ = b.Control.ReleaseRuntime(ctx, runtime)
				continue
			}
		}
		// 3. Execute the manifest on this instance.
		outcome := b.executeOnRuntime(ctx, runtime, manifest, request.OnStarted)
		// 3b. Copy the remote workspace back to the local project.
		if b.Config.SyncOut && outcome.Err == nil {
			if err := b.syncWorkspaceOut(ctx, runtime); err != nil {
				outcome.Err = RedactError(b.Redactor, fmt.Errorf("sync workspace from Colab: %w", err))
			}
		}
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

// workspaceUploadAttempts bounds the retry of the idempotent workspace upload.
// A freshly assigned runtime can accept a WebSocket connection before its kernel
// answers, which shows up as a read timeout that succeeds on a later attempt.
const workspaceUploadAttempts = 3

// workspaceRetryBackoff is a variable so tests can avoid sleeping.
var workspaceRetryBackoff = 5 * time.Second

// syncWorkspaceIn uploads the local workspace to the runtime kernel exactly once
// per run; later submissions reuse the already-synced remote workspace.
func (b *Backend) syncWorkspaceIn(ctx context.Context, runtime Runtime) error {
	transport, ok := b.Executor.(WorkspaceTransport)
	if !ok {
		return fmt.Errorf("Colab workspace sync_in requires an executor with workspace transport support (%T)", b.Executor)
	}
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	if b.syncInDone {
		return nil
	}
	// With the REST transport the durable workspace lives in Google Drive: pull
	// it into the local mirror and restore it into the runtime before the local
	// project is uploaded, so local files win for the paths they cover.
	if b.Config.DriveTransport == DriveTransportREST && b.Config.DriveStore != nil {
		if err := b.restoreDriveWorkspace(ctx, transport, runtime); err != nil {
			return err
		}
	}
	mapper := PathMapper{HostRoot: b.Config.LocalRoot, RemoteRoot: filepath.Join(b.remoteRoot(), "work"), PathMap: b.Config.PathMap}
	archive, err := BuildWorkspaceArchive(b.Config.LocalRoot, mapper, b.workspaceExcludes())
	if err != nil {
		return err
	}
	if err := uploadWorkspaceWithRetry(ctx, transport, runtime, archive); err != nil {
		return err
	}
	b.syncInDone = true
	return nil
}

// restoreDriveWorkspace mirrors the Drive folder into the local mirror and
// uploads it into the runtime at the remote workspace root.
func (b *Backend) restoreDriveWorkspace(ctx context.Context, transport WorkspaceTransport, runtime Runtime) error {
	mirrorRoot := b.syncOutRoot()
	if err := os.MkdirAll(mirrorRoot, 0o755); err != nil {
		return err
	}
	restored := 0
	if err := b.Config.DriveStore.Walk(ctx, b.driveFolder(), func(relativePath string, size int64, content io.Reader) error {
		target := filepath.Join(mirrorRoot, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// Stream to disk: a restored checkpoint must not have to fit in memory.
		file, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(file, content); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		restored++
		return nil
	}); err != nil {
		return fmt.Errorf("restore workspace from Drive: %w", err)
	}
	if restored == 0 {
		return nil
	}
	archive, err := BuildWorkspaceArchive(mirrorRoot, PathMapper{HostRoot: mirrorRoot, RemoteRoot: b.remoteRoot()}, nil)
	if err != nil {
		return err
	}
	return uploadWorkspaceWithRetry(ctx, transport, runtime, archive)
}

// drivePersistReport summarises what a Drive workspace upload did, so a run can
// say how much of the workspace was actually transferred.
type drivePersistReport struct {
	Uploaded int
	// Skipped counts the files Drive already had, which the checksum comparison
	// avoids transferring.
	Skipped int
	// Resumed counts the files that continued a previous partial upload.
	Resumed int
}

// persistDriveWorkspace uploads the local mirror into the Drive folder.
//
// Each file is streamed from disk by the store, which also compares checksums, so
// an unchanged workspace costs one listing instead of a full re-upload.
func (b *Backend) persistDriveWorkspace(ctx context.Context) (drivePersistReport, error) {
	report := drivePersistReport{}
	mirrorRoot := b.syncOutRoot()
	if _, err := os.Stat(mirrorRoot); err != nil {
		if os.IsNotExist(err) {
			return report, nil
		}
		return report, err
	}
	err := filepath.WalkDir(mirrorRoot, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(mirrorRoot, current)
		if err != nil {
			return err
		}
		folder := b.driveFolder()
		if directory := path.Dir(filepath.ToSlash(relative)); directory != "." {
			if folder == "" {
				folder = directory
			} else {
				folder = path.Join(folder, directory)
			}
		}
		outcome, err := b.Config.DriveStore.Upload(ctx, folder, filepath.Base(relative), current)
		if err != nil {
			return fmt.Errorf("upload %s to Drive: %w", relative, err)
		}
		switch {
		case outcome.Skipped:
			report.Skipped++
		default:
			if outcome.Resumed {
				report.Resumed++
			}
			report.Uploaded++
		}
		return nil
	})
	return report, err
}

// driveFolder is the Drive folder the workspace mirrors into.
func (b *Backend) driveFolder() string {
	if folder := strings.Trim(strings.TrimSpace(b.Config.DriveFolder), "/"); folder != "" {
		return folder
	}
	// Derive from a Drive-shaped remote root, e.g.
	// /content/drive/MyDrive/craftmake -> craftmake.
	root := b.remoteRoot()
	for _, prefix := range []string{"/content/drive/My Drive/", "/content/drive/MyDrive/"} {
		if strings.HasPrefix(root, prefix) {
			return strings.Trim(strings.TrimPrefix(root, prefix), "/")
		}
	}
	return "craftmake"
}

// uploadWorkspaceWithRetry retries the upload while the failure looks like a
// kernel that is not answering yet. The operation overwrites files, so repeating
// it is safe.
//
// Two failures are worth repeating, and they are the two ways a freshly assigned
// runtime shows up: the kernel drops the connection (the channels WebSocket
// answers before the kernel does), or it accepts the request but does not run the
// cell, which is reported as a missing confirmation marker. Anything else - a
// rejected archive, a bad credential - is returned as it is, so a real problem is
// not retried three times before it is reported.
func uploadWorkspaceWithRetry(ctx context.Context, transport WorkspaceTransport, runtime Runtime, archive []byte) error {
	var lastErr error
	for attempt := 1; attempt <= workspaceUploadAttempts; attempt++ {
		lastErr = transport.UploadWorkspace(ctx, runtime, archive)
		if lastErr == nil {
			return nil
		}
		var remote *RemoteError
		if !errors.As(lastErr, &remote) || !isRetryableWorkspaceUpload(remote.Kind) {
			return lastErr
		}
		if attempt == workspaceUploadAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * workspaceRetryBackoff):
		}
	}
	return lastErr
}

// isRetryableWorkspaceUpload reports whether a failed workspace upload is worth
// repeating.
func isRetryableWorkspaceUpload(kind ErrorKind) bool {
	return kind == ErrorKernelDisconnected || kind == ErrorKernelNotReady
}

// syncWorkspaceOut downloads the remote workspace and merges it into the local
// project directory.
func (b *Backend) syncWorkspaceOut(ctx context.Context, runtime Runtime) error {
	transport, ok := b.Executor.(WorkspaceTransport)
	if !ok {
		return fmt.Errorf("Colab workspace sync_out requires an executor with workspace transport support (%T)", b.Executor)
	}
	archive, err := transport.DownloadWorkspace(ctx, runtime, b.remoteRoot(), b.workspaceSyncOutExcludes())
	if err != nil {
		return err
	}
	if len(archive) == 0 {
		return nil
	}
	mirrorRoot := b.syncOutRoot()
	if err := os.MkdirAll(mirrorRoot, 0o755); err != nil {
		return err
	}
	if err := ExtractWorkspaceArchive(archive, mirrorRoot, b.remoteRoot(), b.Config.PathMap, b.workspaceSyncOutExcludes()); err != nil {
		return err
	}
	if b.Config.DriveTransport == DriveTransportREST && b.Config.DriveStore != nil {
		report, err := b.persistDriveWorkspace(ctx)
		if err != nil {
			return err
		}
		// Say what actually moved: with the checksum comparison an unchanged
		// workspace transfers nothing, which is worth reporting rather than
		// leaving the operator to guess.
		if report.Skipped > 0 || report.Resumed > 0 {
			fmt.Fprintf(os.Stderr, "notice: Drive workspace persisted: %d uploaded, %d unchanged (skipped), %d resumed\n", report.Uploaded, report.Skipped, report.Resumed)
		}
	}
	return nil
}

// syncOutRoot is the local mirror of the remote workspace. It lives inside the
// excluded .craftmake directory so a downloaded workspace is never re-uploaded
// (which would nest work/work/... on every run).
func (b *Backend) syncOutRoot() string {
	return filepath.Join(b.Config.LocalRoot, ".craftmake", "colab-workspace")
}

// workspaceSyncOutExcludes additionally skips craftmake's own remote runtime
// directory, whose contents are materialized locally as task artifacts.
func (b *Backend) workspaceSyncOutExcludes() []string {
	return append(append([]string{}, b.workspaceExcludes()...), "runtime")
}

// workspaceExcludes returns the configured sync excludes, defaulting to the
// local state directory and the git database. The local workspace mirror is
// always excluded so it is never uploaded back into the runtime.
func (b *Backend) workspaceExcludes() []string {
	excludes := b.Config.SyncExcludes
	if len(excludes) == 0 {
		excludes = []string{".craftmake/state", ".git"}
	}
	excludes = append(append([]string{}, excludes...), ".craftmake/colab-workspace")
	return excludes
}

// executeOnRuntime builds and runs a single manifest on the given runtime,
// materializing logs and decoding the task result. It does not manage the
// runtime lifecycle; the caller is responsible for release.
func (b *Backend) executeOnRuntime(ctx context.Context, runtime Runtime, manifest *protocol.TaskManifest, onStarted func(string, map[string]any) error) backend.TaskOutcome {
	mapping := RemoteTaskMapping{WorkDirectory: filepath.Join(b.remoteRoot(), "work"), TempDirectory: filepath.Join(b.scratchRoot(), "tmp"), RuntimeDirectory: filepath.Join(b.remoteRoot(), "runtime", manifest.TaskID), ResultPath: filepath.Join(b.remoteRoot(), "runtime", manifest.TaskID, "result.json")}
	notebook, err := BuildNotebookRedactedWithOptions(manifest, mapping, b.Redactor, b.notebookOptions())
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
	if notice := driveMountNotice(output); notice != "" {
		// Surface a missing Drive mount: otherwise a step writing under
		// /content/drive would keep succeeding against the ephemeral disk.
		taskResult.ObservabilityErrors = append(taskResult.ObservabilityErrors, notice)
		fmt.Fprintf(os.Stderr, "notice: %s\n", notice)
	}
	if err := b.materializeTaskLogs(ctx, taskResult, manifest, mapping, output); err != nil {
		taskResult.ObservabilityErrors = append(taskResult.ObservabilityErrors, err.Error())
	}
	return backend.TaskOutcome{Result: &backend.Result{TaskResult: taskResult, BackendID: "colab:" + manifest.TaskID}}
}

func (b *Backend) materializeTaskLogs(ctx context.Context, taskResult *protocol.TaskResult, manifest *protocol.TaskManifest, mapping RemoteTaskMapping, output string) error {
	// Manifest step indexes are 1-based (compiler.go assigns stepIndex+1), so
	// resolve each reported step by its index instead of using it as a slice
	// offset.
	byIndex := make(map[int]protocol.StepManifest, len(manifest.Steps))
	for _, step := range manifest.Steps {
		byIndex[step.Index] = step
	}
	var failures []string
	for _, step := range taskResult.Steps {
		manifestStep, ok := byIndex[step.Index]
		if !ok {
			continue
		}
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

// notebookOptions decides whether the generated notebook should mount Drive.
// Mounting is attempted only when a Drive credential is configured and the mode
// is not off. The rclone mount is the mode that works with a third-party
// credential and is marked required, so a failure fails the task instead of
// letting the workspace land on the ephemeral disk; the DriveFS mount degrades
// gracefully, as documented.
func (b *Backend) notebookOptions() NotebookOptions {
	mode := NormalizeDriveMountMode(b.Config.DriveMount)
	if mode == DriveMountOff || b.Config.DriveCredential == nil {
		return NotebookOptions{}
	}
	credential := *b.Config.DriveCredential
	if strings.TrimSpace(credential.RefreshToken) == "" {
		return NotebookOptions{}
	}
	mountPath := b.Config.DriveMountPath
	if mountPath == "" {
		mountPath = b.Config.MountPath
	}
	if mountPath == "" {
		mountPath = "/content/drive"
	}
	spec := DriveMountSpec{Credential: credential, MountPath: mountPath, TimeoutSecs: DriveMountTimeoutFromEnv()}
	if mode == DriveMountRclone {
		spec.Kind = DriveMountKindRclone
		spec.Rclone = RcloneMountOptionsFromEnv()
		// The operator asked for Drive explicitly: writing to the ephemeral disk
		// instead must not be reported as success.
		spec.Required = true
	} else {
		spec.Kind = DriveMountKindDriveFS
	}
	return NotebookOptions{DriveMount: &spec}
}

// driveMountNotice extracts the reason the runtime could not mount Drive, or
// could not flush it, if the bootstrap or finalizer reported one. Without this
// the condition would be invisible: the mount happens in the bootstrap cell and
// the flush in the finalizer, whose output is not part of the task result unless
// it is surfaced here.
func driveMountNotice(output string) string {
	index := -1
	for _, marker := range []string{driveMountUnavailableMarker, driveFlushTimeoutMarker} {
		if found := strings.Index(output, marker); found >= 0 && (index < 0 || found < index) {
			index = found
		}
	}
	if index < 0 {
		return ""
	}
	line := output[index:]
	if end := strings.IndexAny(line, "\r\n"); end >= 0 {
		line = line[:end]
	}
	return strings.TrimSpace(line)
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
