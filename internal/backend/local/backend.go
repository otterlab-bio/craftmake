package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/metrics"
	"github.com/otterlab-bio/craftmake/internal/processgroup"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type Backend struct {
	mutex    sync.Mutex
	commands map[string]*exec.Cmd
}

func New() *Backend {
	return &Backend{commands: make(map[string]*exec.Cmd)}
}

func (localBackend *Backend) Name() string { return "local" }

func (localBackend *Backend) RunSubmission(ctx context.Context, executable string, request backend.SubmissionRequest) (*backend.SubmissionResult, error) {
	if len(request.Manifests) == 0 {
		return nil, fmt.Errorf("submission %q contains no task manifests", request.SubmissionID)
	}
	if err := os.MkdirAll(request.RuntimeDirectory, 0o755); err != nil {
		return nil, err
	}
	if request.OnStarted != nil {
		taskRuntimeDirectories := make([]string, 0, len(request.Manifests))
		for _, manifest := range request.Manifests {
			taskRuntimeDirectories = append(taskRuntimeDirectories, manifest.RuntimeDirectory)
		}
		if err := request.OnStarted("local:"+request.SubmissionID, map[string]any{"runtime_directory": request.RuntimeDirectory, "task_runtime_directories": taskRuntimeDirectories}); err != nil {
			return nil, err
		}
	}
	maximumParallelWorkers := request.WorkerMaxParallel
	if maximumParallelWorkers <= 0 || maximumParallelWorkers > len(request.Manifests) {
		maximumParallelWorkers = len(request.Manifests)
	}

	outcomes := make(map[string]backend.TaskOutcome, len(request.Manifests))
	var outcomesMutex sync.Mutex
	workerSlots := make(chan struct{}, maximumParallelWorkers)
	var workers sync.WaitGroup

	for _, manifest := range request.Manifests {
		manifest := manifest
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case workerSlots <- struct{}{}:
				defer func() { <-workerSlots }()
			case <-ctx.Done():
				outcomesMutex.Lock()
				outcomes[manifest.TaskID] = backend.TaskOutcome{Err: ctx.Err()}
				outcomesMutex.Unlock()
				return
			}
			if fileExists(filepath.Join(request.RuntimeDirectory, "cancel.requested")) {
				outcomesMutex.Lock()
				outcomes[manifest.TaskID] = backend.TaskOutcome{Err: context.Canceled}
				outcomesMutex.Unlock()
				return
			}

			result, runErr := localBackend.runTask(ctx, executable, manifest)
			outcomesMutex.Lock()
			outcomes[manifest.TaskID] = backend.TaskOutcome{Result: result, Err: runErr}
			outcomesMutex.Unlock()
		}()
	}
	workers.Wait()

	backendID := "local:" + request.SubmissionID
	for taskID, outcome := range outcomes {
		if outcome.Result != nil {
			outcome.Result.BackendID = backendID
			outcomes[taskID] = outcome
		}
	}
	return &backend.SubmissionResult{BackendID: backendID, Tasks: outcomes}, nil
}

var (
	verboseTimeOnce sync.Once
	verboseTime     string
)

// verboseTimePath reports a `time` binary that supports GNU-style
// `-v -o <file>` metrics capture, or "" when none is usable. Existence of
// /usr/bin/time is not enough: the BSD implementation shipped on macOS
// rejects -v, which would fail the task before the runner even starts.
func verboseTimePath() string {
	verboseTimeOnce.Do(func() {
		timePath, lookupErr := exec.LookPath("/usr/bin/time")
		if lookupErr != nil {
			return
		}
		probe := exec.Command(timePath, "-v", "-o", os.DevNull, timePath, "--version")
		if probe.Run() == nil {
			verboseTime = timePath
		}
	})
	return verboseTime
}

func (localBackend *Backend) runTask(ctx context.Context, executable string, manifest *protocol.TaskManifest) (*backend.Result, error) {
	if err := os.MkdirAll(manifest.RuntimeDirectory, 0o755); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(manifest.RuntimeDirectory, "manifest.json")
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		return nil, err
	}
	metricsPath := filepath.Join(manifest.RuntimeDirectory, "metrics.raw")
	arguments := []string{"__task-runner", "--manifest", manifestPath}
	commandName := executable
	if timePath := verboseTimePath(); timePath != "" {
		commandName = timePath
		arguments = append([]string{"-v", "-o", metricsPath, executable}, arguments...)
	}
	command := exec.Command(commandName, arguments...)
	command.Dir = manifest.WorkDirectory
	processgroup.Configure(command)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start task process: %w", err)
	}
	processGroupPath := filepath.Join(manifest.RuntimeDirectory, "process-group.pid")
	if err := os.WriteFile(processGroupPath, []byte(strconv.Itoa(command.Process.Pid)+"\n"), 0o644); err != nil {
		_ = processgroup.Terminate(command)
		_ = command.Wait()
		return nil, fmt.Errorf("record task process group: %w", err)
	}
	localBackend.mutex.Lock()
	localBackend.commands[manifest.TaskID] = command
	localBackend.mutex.Unlock()
	runErr := processgroup.Wait(ctx, command)
	_ = os.Remove(processGroupPath)
	localBackend.mutex.Lock()
	delete(localBackend.commands, manifest.TaskID)
	localBackend.mutex.Unlock()

	resultData, readErr := os.ReadFile(manifest.ResultPath)
	if readErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("task process failed: %w", runErr)
		}
		return nil, fmt.Errorf("read task result: %w", readErr)
	}
	taskResult, err := protocol.DecodeTaskResultForManifest(resultData, manifest, true)
	if err != nil {
		return nil, fmt.Errorf("parse task result: %w", err)
	}
	collected := &metrics.TaskMetrics{Source: "wall_clock", Quality: "limited"}
	if _, err := os.Stat(metricsPath); err == nil {
		if parsed, parseErr := metrics.ParseGNUTimeFile(metricsPath); parseErr == nil {
			collected = parsed
		}
	}
	result := &backend.Result{TaskResult: taskResult, Metrics: collected}
	if taskResult.Status != "succeeded" {
		return result, fmt.Errorf("task %s failed: %s", manifest.TaskID, taskResult.Error)
	}
	return result, nil
}

func (localBackend *Backend) RecoverSubmission(ctx context.Context, request backend.RecoveryRequest) (*backend.SubmissionResult, error) {
	outcomes := make(map[string]backend.TaskOutcome, len(request.Manifests))
	var recoveryErrors []error
	for _, manifest := range request.Manifests {
		result, loadErr := loadRecoveredResult(manifest, "local:"+request.SubmissionID)
		if loadErr == nil {
			outcomes[manifest.TaskID] = backend.TaskOutcome{Result: result, Err: recoveredTaskError(result)}
			continue
		}
		if !os.IsNotExist(errors.Unwrap(loadErr)) {
			outcomes[manifest.TaskID] = backend.TaskOutcome{Err: loadErr}
			recoveryErrors = append(recoveryErrors, loadErr)
			continue
		}

		processGroupID, processErr := readProcessGroupID(manifest.RuntimeDirectory)
		if processErr != nil {
			if !os.IsNotExist(processErr) {
				recoveryErrors = append(recoveryErrors, processErr)
			}
			outcomes[manifest.TaskID] = backend.TaskOutcome{Err: fmt.Errorf("local task result is unavailable after controller interruption")}
			continue
		}
		if !processgroup.GroupExists(processGroupID) {
			_ = os.Remove(filepath.Join(manifest.RuntimeDirectory, "process-group.pid"))
			outcomes[manifest.TaskID] = backend.TaskOutcome{Err: fmt.Errorf("local task process group %d is no longer running", processGroupID)}
			continue
		}
		belongsToManifest, ownershipErr := processBelongsToManifest(processGroupID, manifest)
		if ownershipErr != nil {
			recoveryErrors = append(recoveryErrors, ownershipErr)
			outcomes[manifest.TaskID] = backend.TaskOutcome{Err: ownershipErr}
			continue
		}
		if !belongsToManifest {
			ownershipErr = fmt.Errorf("refusing to terminate process group %d because it does not reference task manifest %q", processGroupID, filepath.Join(manifest.RuntimeDirectory, "manifest.json"))
			recoveryErrors = append(recoveryErrors, ownershipErr)
			outcomes[manifest.TaskID] = backend.TaskOutcome{Err: ownershipErr}
			continue
		}
		if terminateErr := processgroup.TerminateGroup(processGroupID); terminateErr != nil {
			recoveryErrors = append(recoveryErrors, terminateErr)
			outcomes[manifest.TaskID] = backend.TaskOutcome{Err: terminateErr}
			continue
		}
		waitContext, cancelWait := context.WithTimeout(ctx, 3*time.Second)
		waitErr := processgroup.WaitForGroupExit(waitContext, processGroupID)
		cancelWait()
		if waitErr != nil {
			recoveryErrors = append(recoveryErrors, waitErr)
		}
		_ = os.Remove(filepath.Join(manifest.RuntimeDirectory, "process-group.pid"))
		result, loadErr = loadRecoveredResult(manifest, "local:"+request.SubmissionID)
		if loadErr == nil {
			outcomes[manifest.TaskID] = backend.TaskOutcome{Result: result, Err: recoveredTaskError(result)}
			continue
		}
		outcomes[manifest.TaskID] = backend.TaskOutcome{Err: fmt.Errorf("local orphan task was terminated before producing a terminal result")}
	}
	return &backend.SubmissionResult{
		BackendID: request.BackendJobID,
		Tasks:     outcomes,
		Raw:       request.Metadata,
	}, errors.Join(recoveryErrors...)
}

func loadRecoveredResult(manifest *protocol.TaskManifest, backendID string) (*backend.Result, error) {
	resultData, err := os.ReadFile(manifest.ResultPath)
	if err != nil {
		return nil, fmt.Errorf("read recovered task result: %w", err)
	}
	taskResult, err := protocol.DecodeTaskResultForManifest(resultData, manifest, true)
	if err != nil {
		return nil, fmt.Errorf("parse recovered task result: %w", err)
	}
	return &backend.Result{TaskResult: taskResult, BackendID: backendID}, nil
}

func recoveredTaskError(result *backend.Result) error {
	if result == nil || result.TaskResult == nil || result.TaskResult.Status == "succeeded" {
		return nil
	}
	return fmt.Errorf("recovered task %s completed with status %s: %s", result.TaskResult.TaskID, result.TaskResult.Status, result.TaskResult.Error)
}

func readProcessGroupID(runtimeDirectory string) (int, error) {
	processGroupPath := filepath.Join(runtimeDirectory, "process-group.pid")
	processGroupData, err := os.ReadFile(processGroupPath)
	if err != nil {
		return 0, err
	}
	processGroupID, err := strconv.Atoi(strings.TrimSpace(string(processGroupData)))
	if err != nil || processGroupID <= 0 {
		return 0, fmt.Errorf("invalid process group ID in %q", processGroupPath)
	}
	return processGroupID, nil
}

func processBelongsToManifest(processGroupID int, manifest *protocol.TaskManifest) (bool, error) {
	commandLineData, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(processGroupID), "cmdline"))
	if err != nil {
		return false, fmt.Errorf("inspect process group %d command line: %w", processGroupID, err)
	}
	manifestPath := filepath.Join(manifest.RuntimeDirectory, "manifest.json")
	commandLine := strings.ReplaceAll(string(commandLineData), "\x00", " ")
	return strings.Contains(commandLine, "__task-runner") && strings.Contains(commandLine, manifestPath), nil
}

func (localBackend *Backend) CancelSubmission(_ context.Context, _ string, metadata map[string]any) error {
	runtimeDirectory, _ := metadata["runtime_directory"].(string)
	if strings.TrimSpace(runtimeDirectory) == "" {
		return fmt.Errorf("local submission metadata is missing runtime_directory")
	}
	if err := os.WriteFile(filepath.Join(runtimeDirectory, "cancel.requested"), []byte("cancelled\n"), 0o644); err != nil {
		return fmt.Errorf("record local cancellation request: %w", err)
	}
	taskRuntimeDirectories := metadataStrings(metadata["task_runtime_directories"])
	var cancellationErrors []error
	for _, taskRuntimeDirectory := range taskRuntimeDirectories {
		processGroupPath := filepath.Join(taskRuntimeDirectory, "process-group.pid")
		processGroupData, readErr := os.ReadFile(processGroupPath)
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				cancellationErrors = append(cancellationErrors, readErr)
			}
			continue
		}
		processGroupID, parseErr := strconv.Atoi(strings.TrimSpace(string(processGroupData)))
		if parseErr != nil || processGroupID <= 0 {
			cancellationErrors = append(cancellationErrors, fmt.Errorf("invalid process group ID in %q", processGroupPath))
			continue
		}
		if killErr := processgroup.TerminateGroup(processGroupID); killErr != nil {
			cancellationErrors = append(cancellationErrors, killErr)
		}
	}
	return errors.Join(cancellationErrors...)
}

func metadataStrings(value any) []string {
	switch typedValues := value.(type) {
	case []string:
		return typedValues
	case []any:
		values := make([]string, 0, len(typedValues))
		for _, value := range typedValues {
			if text, ok := value.(string); ok {
				values = append(values, text)
			}
		}
		return values
	default:
		return nil
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (localBackend *Backend) Cancel(context.Context) error {
	localBackend.mutex.Lock()
	defer localBackend.mutex.Unlock()
	for _, command := range localBackend.commands {
		_ = processgroup.Terminate(command)
	}
	return nil
}
