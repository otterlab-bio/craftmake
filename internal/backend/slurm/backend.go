package slurm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/metrics"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type Backend struct {
	PollInterval         time.Duration
	RecoveryMissingLimit int
	PartitionOverride    string
	AccountOverride      string
	QOSOverride          string
	DefaultTimeOverride  string
	ScratchRoot          string
	SubmitMaxAttempts    int
	SubmitInitialBackoff time.Duration
	SubmitMaximumBackoff time.Duration
	PendingTimeout       time.Duration
	mutex                sync.Mutex
	activeJobs           map[string]struct{}
}

type preparedWorker struct {
	manifest     *protocol.TaskManifest
	manifestPath string
	scriptPath   string
	stepIDPath   string
}

type scriptOptions struct {
	Partition   string
	Account     string
	QOS         string
	DefaultTime string
	ScratchRoot string
}

var errSlurmResultNotTerminal = errors.New("Slurm task result is not terminal")

func New() *Backend {
	return NewWithPartition("")
}

func NewWithPartition(partition string) *Backend {
	return &Backend{
		PollInterval:         5 * time.Second,
		RecoveryMissingLimit: 3,
		PartitionOverride:    strings.TrimSpace(partition),
		SubmitMaxAttempts:    8,
		SubmitInitialBackoff: time.Second,
		SubmitMaximumBackoff: 30 * time.Second,
		activeJobs:           make(map[string]struct{}),
	}
}

func (slurmBackend *Backend) Name() string { return "slurm" }

func (slurmBackend *Backend) RefreshMetrics(ctx context.Context, request backend.MetricsRefreshRequest) (*metrics.TaskMetrics, error) {
	if strings.TrimSpace(request.RuntimeDirectory) == "" {
		return nil, fmt.Errorf("refresh metrics for attempt %q: runtime directory is required", request.AttemptID)
	}
	stepIDPath := filepath.Join(request.RuntimeDirectory, "slurm-step-id")
	stepIDData, err := os.ReadFile(stepIDPath)
	if err != nil {
		return nil, fmt.Errorf("refresh metrics for attempt %q: read Slurm step ID: %w", request.AttemptID, err)
	}
	stepID := strings.TrimSpace(string(stepIDData))
	if stepID == "" {
		return nil, fmt.Errorf("refresh metrics for attempt %q: Slurm step ID is empty", request.AttemptID)
	}
	collected, _, err := queryStepMetrics(ctx, stepID)
	if err != nil {
		return nil, fmt.Errorf("refresh metrics for attempt %q and Slurm step %s: %w", request.AttemptID, stepID, err)
	}
	return collected, nil
}

func (slurmBackend *Backend) RunSubmission(ctx context.Context, executable string, request backend.SubmissionRequest) (*backend.SubmissionResult, error) {
	if len(request.Manifests) == 0 {
		return nil, fmt.Errorf("submission %q contains no task manifests", request.SubmissionID)
	}

	submissionDirectory := request.RuntimeDirectory
	if submissionDirectory == "" {
		submissionDirectory = filepath.Dir(request.Manifests[0].RuntimeDirectory)
	}
	if err := os.MkdirAll(submissionDirectory, 0o755); err != nil {
		return nil, err
	}

	workers := make([]preparedWorker, 0, len(request.Manifests))
	for _, manifest := range request.Manifests {
		worker, err := prepareWorker(executable, manifest)
		if err != nil {
			return nil, err
		}
		workers = append(workers, worker)
	}

	scriptPath := filepath.Join(submissionDirectory, "submission.sh")
	script, err := buildScriptWithOptions(request, workers, submissionDirectory, scriptOptions{
		Partition:   slurmBackend.PartitionOverride,
		Account:     slurmBackend.AccountOverride,
		QOS:         slurmBackend.QOSOverride,
		DefaultTime: slurmBackend.DefaultTimeOverride,
		ScratchRoot: slurmBackend.ScratchRoot,
	})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return nil, err
	}

	jobName := shortJobName(request.SubmissionID)
	jobID, err := slurmBackend.submitWithRetry(ctx, scriptPath, jobName, request.OnStateChanged)
	if err != nil {
		return nil, err
	}
	if request.OnStarted != nil {
		if err := request.OnStarted(jobID, map[string]any{"job_id": jobID, "submission_script": scriptPath, "runtime_directory": submissionDirectory}); err != nil {
			_ = exec.CommandContext(ctx, "scancel", jobID).Run()
			return nil, err
		}
	}
	slurmBackend.trackJob(jobID, true)
	defer slurmBackend.trackJob(jobID, false)

	waitErr := slurmBackend.wait(ctx, jobID, request.OnStateChanged)
	outcomes := make(map[string]backend.TaskOutcome, len(workers))
	for _, worker := range workers {
		result, taskErr := slurmBackend.loadTaskOutcome(ctx, jobID, worker, true)
		outcomes[worker.manifest.TaskID] = backend.TaskOutcome{Result: result, Err: taskErr}
	}

	return &backend.SubmissionResult{
		BackendID: jobID,
		Tasks:     outcomes,
		Raw:       map[string]any{"job_id": jobID, "submission_script": scriptPath},
	}, waitErr
}

func (slurmBackend *Backend) RecoverSubmission(ctx context.Context, request backend.RecoveryRequest) (*backend.SubmissionResult, error) {
	if strings.TrimSpace(request.BackendJobID) == "" {
		return nil, fmt.Errorf("cannot recover Slurm submission %q without a backend job ID", request.SubmissionID)
	}

	outcomes := make(map[string]backend.TaskOutcome, len(request.Manifests))
	pendingManifests := make([]*protocol.TaskManifest, 0, len(request.Manifests))
	var recoveryErrors []error
	for _, manifest := range request.Manifests {
		result, loadErr := slurmBackend.loadRecoveredTaskOutcome(ctx, request.BackendJobID, manifest)
		if loadErr == nil {
			outcomes[manifest.TaskID] = backend.TaskOutcome{Result: result, Err: slurmTaskError(manifest.TaskID, result)}
			continue
		}
		if errors.Is(loadErr, os.ErrNotExist) || errors.Is(loadErr, errSlurmResultNotTerminal) {
			pendingManifests = append(pendingManifests, manifest)
			continue
		}
		outcomes[manifest.TaskID] = backend.TaskOutcome{Err: loadErr}
		recoveryErrors = append(recoveryErrors, loadErr)
	}

	var waitErr error
	if len(pendingManifests) > 0 {
		slurmBackend.trackJob(request.BackendJobID, true)
		waitErr = slurmBackend.waitForRecovery(ctx, request.BackendJobID)
		slurmBackend.trackJob(request.BackendJobID, false)
		if waitErr != nil {
			recoveryErrors = append(recoveryErrors, waitErr)
		}
		for _, manifest := range pendingManifests {
			result, loadErr := slurmBackend.loadRecoveredTaskOutcome(ctx, request.BackendJobID, manifest)
			if loadErr != nil {
				outcomes[manifest.TaskID] = backend.TaskOutcome{Err: loadErr}
				if !errors.Is(loadErr, os.ErrNotExist) {
					recoveryErrors = append(recoveryErrors, loadErr)
				}
				continue
			}
			outcomes[manifest.TaskID] = backend.TaskOutcome{Result: result, Err: slurmTaskError(manifest.TaskID, result)}
		}
	}

	return &backend.SubmissionResult{
		BackendID: request.BackendJobID,
		Tasks:     outcomes,
		Raw:       request.Metadata,
	}, errors.Join(recoveryErrors...)
}

func (slurmBackend *Backend) loadRecoveredTaskOutcome(ctx context.Context, jobID string, manifest *protocol.TaskManifest) (*backend.Result, error) {
	worker := preparedWorker{
		manifest:   manifest,
		stepIDPath: filepath.Join(manifest.RuntimeDirectory, "slurm-step-id"),
	}
	result, loadErr := slurmBackend.loadTaskOutcome(ctx, jobID, worker, false)
	if loadErr != nil {
		if result == nil || result.TaskResult == nil || errors.Is(loadErr, errSlurmResultNotTerminal) {
			return nil, loadErr
		}
	}
	return result, nil
}

func slurmTaskError(taskID string, result *backend.Result) error {
	if result == nil || result.TaskResult == nil || result.TaskResult.Status == "succeeded" {
		return nil
	}
	return fmt.Errorf("Slurm task %s failed: %s", taskID, result.TaskResult.Error)
}

func (slurmBackend *Backend) waitForRecovery(ctx context.Context, jobID string) error {
	missingLimit := slurmBackend.RecoveryMissingLimit
	if missingLimit <= 0 {
		missingLimit = 3
	}
	interval := slurmBackend.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	missingCount := 0
	for {
		state, complete, queryErr := queryState(ctx, jobID)
		if queryErr != nil {
			return queryErr
		}
		if complete {
			return terminalStateError(jobID, state)
		}
		if state == "" {
			missingCount++
			if missingCount >= missingLimit {
				return fmt.Errorf("Slurm job %s is not present in squeue or sacct after %d checks", jobID, missingCount)
			}
		} else {
			missingCount = 0
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (slurmBackend *Backend) waitWithoutCancellation(ctx context.Context, jobID string) error {
	interval := slurmBackend.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		state, complete, err := queryState(ctx, jobID)
		if err == nil && complete {
			return terminalStateError(jobID, state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func terminalStateError(jobID, state string) error {
	switch state {
	case "COMPLETED":
		return nil
	case "FAILED", "CANCELLED", "TIMEOUT", "OUT_OF_MEMORY", "NODE_FAIL", "PREEMPTED":
		return fmt.Errorf("Slurm job %s completed in state %s", jobID, state)
	default:
		return fmt.Errorf("Slurm job %s completed in unrecognized state %s", jobID, state)
	}
}

func prepareWorker(executable string, manifest *protocol.TaskManifest) (preparedWorker, error) {
	if err := os.MkdirAll(manifest.RuntimeDirectory, 0o755); err != nil {
		return preparedWorker{}, err
	}
	manifestPath := filepath.Join(manifest.RuntimeDirectory, "manifest.json")
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return preparedWorker{}, err
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		return preparedWorker{}, err
	}

	stepIDPath := filepath.Join(manifest.RuntimeDirectory, "slurm-step-id")
	workerScriptPath := filepath.Join(manifest.RuntimeDirectory, "worker.sh")
	workerScript := fmt.Sprintf(`#!/usr/bin/env bash
set -uo pipefail
printf '%%s.%%s\n' "${SLURM_JOB_ID}" "${SLURM_STEP_ID}" > %s
cd %s
exec %s __task-runner --manifest %s
`, shellValue(stepIDPath), shellValue(manifest.WorkDirectory), shellValue(executable), shellValue(manifestPath))
	if err := os.WriteFile(workerScriptPath, []byte(workerScript), 0o755); err != nil {
		return preparedWorker{}, err
	}
	return preparedWorker{manifest: manifest, manifestPath: manifestPath, scriptPath: workerScriptPath, stepIDPath: stepIDPath}, nil
}

func buildScript(request backend.SubmissionRequest, workers []preparedWorker, submissionDirectory, partitionOverride string) (string, error) {
	return buildScriptWithOptions(request, workers, submissionDirectory, scriptOptions{Partition: partitionOverride})
}

func buildScriptWithOptions(request backend.SubmissionRequest, workers []preparedWorker, submissionDirectory string, options scriptOptions) (string, error) {
	if len(workers) == 0 {
		return "", fmt.Errorf("cannot build a Slurm script without workers")
	}
	allocationResources := request.Resources
	if allocationResources.Cores <= 0 {
		allocationResources.Cores = 1
	}
	if options.Partition = strings.TrimSpace(options.Partition); options.Partition != "" {
		allocationResources.Partition = options.Partition
	}
	var allocationDirectives strings.Builder
	if allocationResources.Partition != "" {
		fmt.Fprintf(&allocationDirectives, "#SBATCH --partition=%s\n", shellValue(allocationResources.Partition))
	}
	if options.Account = strings.TrimSpace(options.Account); options.Account != "" {
		fmt.Fprintf(&allocationDirectives, "#SBATCH --account=%s\n", shellValue(options.Account))
	}
	if options.QOS = strings.TrimSpace(options.QOS); options.QOS != "" {
		fmt.Fprintf(&allocationDirectives, "#SBATCH --qos=%s\n", shellValue(options.QOS))
	}
	timeLimit := strings.TrimSpace(allocationResources.Time)
	if timeLimit == "" {
		timeLimit = strings.TrimSpace(options.DefaultTime)
	}
	if timeLimit == "" {
		timeLimit = "24:00:00"
	}
	scratchExport := ""
	if options.ScratchRoot = strings.TrimSpace(options.ScratchRoot); options.ScratchRoot != "" {
		scratchExport = "export CRAFTMAKE_SCRATCH_ROOT=" + shellValue(options.ScratchRoot) + "\n"
	}
	maximumParallelWorkers := request.WorkerMaxParallel
	if maximumParallelWorkers <= 0 || maximumParallelWorkers > len(workers) {
		maximumParallelWorkers = len(workers)
	}

	var script strings.Builder
	fmt.Fprintf(&script, `#!/usr/bin/env bash
#SBATCH --job-name=%s
%s#SBATCH --nodes=1
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=%d
#SBATCH --mem=%s
#SBATCH --time=%s
#SBATCH --output=%s
#SBATCH --error=%s
set -uo pipefail
%smaximum_parallel_workers=%d
active_worker_pids=()
run_worker_with_retry() {
    local task_id="$1"
    local step_id_path="$2"
    local result_path="$3"
    local launch_error_path="$4"
    shift 4

    local attempt_number=1
    local maximum_attempts=8
    local retry_delay_seconds=1
    while true; do
        printf 'attempt=%%d task=%%s started_at=%%s\n' "$attempt_number" "$task_id" "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" >> "$launch_error_path"
        "$@" 2>> "$launch_error_path"
        local worker_exit_code=$?
        if (( worker_exit_code == 0 )); then
            return 0
        fi
        if [[ -s "$step_id_path" || -s "$result_path" ]]; then
            return "$worker_exit_code"
        fi
        local recent_error
        recent_error="$(tail -n 20 "$launch_error_path" 2>/dev/null | tr '[:upper:]' '[:lower:]' || true)"
        case "$recent_error" in
            *"unable to create step"*|*"slurm_receive_msg"*|*"socket timed out"*|*"temporarily unable"*|*"resource temporarily unavailable"*)
                ;;
            *)
                return "$worker_exit_code"
                ;;
        esac
        if (( attempt_number >= maximum_attempts )); then
            return "$worker_exit_code"
        fi
        sleep "$retry_delay_seconds"
        attempt_number=$((attempt_number + 1))
        retry_delay_seconds=$((retry_delay_seconds * 2))
        if (( retry_delay_seconds > 30 )); then
            retry_delay_seconds=30
        fi
    done
}
wait_for_oldest_worker() {
    local oldest_worker_pid="${active_worker_pids[0]}"
    wait "${oldest_worker_pid}" || true
    active_worker_pids=("${active_worker_pids[@]:1}")
}
`, shellValue(shortJobName(request.SubmissionID)), allocationDirectives.String(), allocationResources.Cores, metrics.FormatSlurmMemory(allocationResources.MemoryByte), shellValue(timeLimit), shellValue(filepath.Join(submissionDirectory, "slurm-%j.out")), shellValue(filepath.Join(submissionDirectory, "slurm-%j.err")), scratchExport, maximumParallelWorkers)

	for _, worker := range workers {
		workerResources := worker.manifest.Resources
		if workerResources.Cores <= 0 {
			workerResources.Cores = 1
		}
		fmt.Fprintf(&script, `run_worker_with_retry %s %s %s %s srun --exclusive --nodes=1 --ntasks=1 --cpus-per-task=%d --mem=%s --job-name=%s --output=%s --error=%s %s &
active_worker_pids+=("$!")
if (( ${#active_worker_pids[@]} >= maximum_parallel_workers )); then
    wait_for_oldest_worker
fi
`, shellValue(worker.manifest.TaskID), shellValue(worker.stepIDPath), shellValue(worker.manifest.ResultPath), shellValue(filepath.Join(worker.manifest.RuntimeDirectory, "srun-launch.err")), workerResources.Cores, metrics.FormatSlurmMemory(workerResources.MemoryByte), shellValue(shortJobName(worker.manifest.TaskID)), shellValue(filepath.Join(worker.manifest.RuntimeDirectory, "slurm-step-%j.out")), shellValue(filepath.Join(worker.manifest.RuntimeDirectory, "slurm-step-%j.err")), shellValue(worker.scriptPath))
	}
	script.WriteString(`if (( ${#active_worker_pids[@]} > 0 )); then
    for worker_pid in "${active_worker_pids[@]}"; do
        wait "${worker_pid}" || true
    done
fi
`)
	return script.String(), nil
}

func (slurmBackend *Backend) loadTaskOutcome(ctx context.Context, jobID string, worker preparedWorker, retryMetrics bool) (*backend.Result, error) {
	result := &backend.Result{BackendID: jobID}
	resultData, readErr := os.ReadFile(worker.manifest.ResultPath)
	if readErr != nil {
		return result, fmt.Errorf("read task result after Slurm completion: %w", readErr)
	}
	taskResult, err := protocol.DecodeTaskResultForManifest(resultData, worker.manifest, false)
	if err != nil {
		return result, fmt.Errorf("parse task result: %w", err)
	}
	result.TaskResult = taskResult

	stepIDData, stepIDErr := os.ReadFile(worker.stepIDPath)
	if stepIDErr == nil {
		stepID := strings.TrimSpace(string(stepIDData))
		var collected *metrics.TaskMetrics
		var raw map[string]string
		if retryMetrics {
			collected, raw = slurmBackend.collectMetrics(ctx, stepID)
		} else {
			collected, raw = slurmBackend.collectMetricsOnce(ctx, stepID)
		}
		result.Metrics = collected
		result.Raw = mapStringToAny(raw)
		result.Raw["step_id"] = stepID
	} else {
		result.Metrics = &metrics.TaskMetrics{Source: "slurm_sacct", Quality: "unavailable", Raw: map[string]string{"JobID": jobID}}
		result.Raw = map[string]any{"job_id": jobID, "step_id_error": stepIDErr.Error()}
	}
	if !protocol.IsTerminalTaskStatus(taskResult.Status) {
		return result, fmt.Errorf("%w: task result status %q", errSlurmResultNotTerminal, taskResult.Status)
	}
	if taskResult.Status != "succeeded" {
		return result, fmt.Errorf("Slurm task %s failed: %s", worker.manifest.TaskID, taskResult.Error)
	}
	return result, nil
}

func (slurmBackend *Backend) trackJob(jobID string, active bool) {
	slurmBackend.mutex.Lock()
	defer slurmBackend.mutex.Unlock()
	if active {
		slurmBackend.activeJobs[jobID] = struct{}{}
		return
	}
	delete(slurmBackend.activeJobs, jobID)
}

func (slurmBackend *Backend) CancelSubmission(ctx context.Context, backendJobID string, _ map[string]any) error {
	if strings.TrimSpace(backendJobID) == "" {
		return fmt.Errorf("Slurm submission is missing backend job ID")
	}
	if output, err := exec.CommandContext(ctx, "scancel", backendJobID).CombinedOutput(); err != nil {
		return fmt.Errorf("scancel %s failed: %w: %s", backendJobID, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (slurmBackend *Backend) Cancel(ctx context.Context) error {
	slurmBackend.mutex.Lock()
	jobIDs := make([]string, 0, len(slurmBackend.activeJobs))
	for jobID := range slurmBackend.activeJobs {
		jobIDs = append(jobIDs, jobID)
	}
	slurmBackend.mutex.Unlock()
	if len(jobIDs) == 0 {
		return nil
	}
	if output, err := exec.CommandContext(ctx, "scancel", jobIDs...).CombinedOutput(); err != nil {
		return fmt.Errorf("scancel failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (slurmBackend *Backend) submitWithRetry(
	ctx context.Context,
	scriptPath string,
	jobName string,
	onStateChanged func(backend.SubmissionState) error,
) (string, error) {
	maximumAttempts := slurmBackend.SubmitMaxAttempts
	if maximumAttempts <= 0 {
		maximumAttempts = 1
	}
	backoff := slurmBackend.SubmitInitialBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	maximumBackoff := slurmBackend.SubmitMaximumBackoff
	if maximumBackoff <= 0 {
		maximumBackoff = 30 * time.Second
	}

	var lastSubmitErr error
	for attemptNumber := 1; attemptNumber <= maximumAttempts; attemptNumber++ {
		jobID, submitErr := submit(ctx, scriptPath)
		if submitErr == nil {
			return jobID, nil
		}
		lastSubmitErr = submitErr

		reconciledJobID, reconcileErr := reconcileSubmittedJob(ctx, jobName)
		if reconcileErr != nil {
			return "", fmt.Errorf("reconcile uncertain Slurm submission %q: %w", jobName, reconcileErr)
		}
		if reconciledJobID != "" {
			if notifyErr := notifySubmissionState(onStateChanged, backend.SubmissionState{
				State:  "SUBMIT_RECONCILED",
				Reason: submitErr.Error(),
				Details: map[string]any{
					"attempt":      attemptNumber,
					"job_id":       reconciledJobID,
					"job_name":     jobName,
					"submit_error": submitErr.Error(),
				},
			}); notifyErr != nil {
				return "", notifyErr
			}
			return reconciledJobID, nil
		}

		if !isRetryableSubmitError(submitErr) || attemptNumber == maximumAttempts {
			break
		}
		notifyErr := notifySubmissionState(onStateChanged, backend.SubmissionState{
			State:  "SUBMIT_RETRY_WAIT",
			Reason: submitErr.Error(),
			Details: map[string]any{
				"attempt":        attemptNumber,
				"max_attempts":   maximumAttempts,
				"retry_after_ms": backoff.Milliseconds(),
			},
		})
		if notifyErr != nil {
			return "", notifyErr
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
		if notifyErr := notifySubmissionState(onStateChanged, backend.SubmissionState{
			State: "SUBMIT_RETRY_READY",
			Details: map[string]any{
				"attempt":      attemptNumber + 1,
				"max_attempts": maximumAttempts,
			},
		}); notifyErr != nil {
			return "", notifyErr
		}
		backoff *= 2
		if backoff > maximumBackoff {
			backoff = maximumBackoff
		}
	}
	return "", fmt.Errorf("submit Slurm job after %d attempts: %w", maximumAttempts, lastSubmitErr)
}

func reconcileSubmittedJob(ctx context.Context, jobName string) (string, error) {
	const maximumChecks = 3
	const initialDelay = 250 * time.Millisecond

	checkDelay := initialDelay
	for checkNumber := 1; checkNumber <= maximumChecks; checkNumber++ {
		jobIDs, queryErr := queryJobIDsByName(ctx, jobName)
		if queryErr == nil {
			switch len(jobIDs) {
			case 0:
			case 1:
				return jobIDs[0], nil
			default:
				return "", fmt.Errorf("multiple Slurm jobs match job name %q: %s", jobName, strings.Join(jobIDs, ", "))
			}
		}
		if checkNumber == maximumChecks {
			return "", nil
		}
		timer := time.NewTimer(checkDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
		checkDelay *= 2
	}
	return "", nil
}

func queryJobIDsByName(ctx context.Context, jobName string) ([]string, error) {
	jobIDSet := make(map[string]struct{})
	var queryErrors []error

	squeueOutput, squeueErr := exec.CommandContext(ctx, "squeue", "-h", "-n", jobName, "-o", "%i|%j").Output()
	if squeueErr != nil {
		queryErrors = append(queryErrors, fmt.Errorf("query squeue by job name: %w", squeueErr))
	} else {
		collectJobIDsByName(jobIDSet, squeueOutput, jobName)
	}

	sacctOutput, sacctErr := exec.CommandContext(ctx, "sacct", "-n", "-P", "-S", "now-1hour", "--name", jobName, "-o", "JobIDRaw,JobName").Output()
	if sacctErr != nil {
		queryErrors = append(queryErrors, fmt.Errorf("query sacct by job name: %w", sacctErr))
	} else {
		collectJobIDsByName(jobIDSet, sacctOutput, jobName)
	}

	if len(jobIDSet) == 0 && len(queryErrors) == 2 {
		return nil, errors.Join(queryErrors...)
	}
	jobIDs := make([]string, 0, len(jobIDSet))
	for jobID := range jobIDSet {
		jobIDs = append(jobIDs, jobID)
	}
	sort.Strings(jobIDs)
	return jobIDs, nil
}

func collectJobIDsByName(jobIDSet map[string]struct{}, commandOutput []byte, expectedJobName string) {
	for _, outputLine := range strings.Split(string(commandOutput), "\n") {
		fields := strings.SplitN(outputLine, "|", 3)
		if len(fields) < 2 || strings.TrimSpace(fields[1]) != expectedJobName {
			continue
		}
		jobID := strings.TrimSpace(fields[0])
		if _, parseErr := strconv.ParseUint(jobID, 10, 64); parseErr == nil {
			jobIDSet[jobID] = struct{}{}
		}
	}
}

func isRetryableSubmitError(submitErr error) bool {
	if submitErr == nil {
		return false
	}
	normalizedMessage := strings.ToLower(submitErr.Error())
	retryableFragments := []string{
		"qosmaxsubmitjobperuserlimit",
		"maxsubmitjobsperuser",
		"assocmaxsubmitjoblimit",
		"maximum number of jobs",
		"job violates accounting/qos policy",
		"temporarily unable to contact slurm controller",
		"unable to contact slurm controller",
		"slurm_receive_msg",
		"socket timed out",
		"resource temporarily unavailable",
	}
	for _, fragment := range retryableFragments {
		if strings.Contains(normalizedMessage, fragment) {
			return true
		}
	}
	return false
}

func notifySubmissionState(callback func(backend.SubmissionState) error, state backend.SubmissionState) error {
	if callback != nil {
		return callback(state)
	}
	return nil
}

func submit(ctx context.Context, scriptPath string) (string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, "sbatch", "--parsable", scriptPath)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("sbatch failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	jobID := strings.Split(strings.TrimSpace(stdout.String()), ";")[0]
	if _, err := strconv.ParseUint(jobID, 10, 64); err != nil {
		return "", fmt.Errorf("unexpected sbatch job ID %q", jobID)
	}
	return jobID, nil
}

type jobState struct {
	State    string
	Reason   string
	Complete bool
}

func (slurmBackend *Backend) wait(
	ctx context.Context,
	jobID string,
	onStateChanged func(backend.SubmissionState) error,
) error {
	interval := slurmBackend.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var pendingStartedAt time.Time
	lastState := ""
	lastReason := ""
	for {
		currentState, queryErr := queryJobState(ctx, jobID)
		if queryErr == nil {
			if currentState.State != lastState || currentState.Reason != lastReason {
				if stateErr := notifySubmissionState(onStateChanged, backend.SubmissionState{
					State:  currentState.State,
					Reason: currentState.Reason,
					Details: map[string]any{
						"job_id": jobID,
						"reason": currentState.Reason,
					},
				}); stateErr != nil {
					return stateErr
				}
				lastState = currentState.State
				lastReason = currentState.Reason
			}
			if currentState.Complete {
				return terminalStateError(jobID, currentState.State)
			}
			if currentState.State == "PENDING" {
				if pendingStartedAt.IsZero() {
					pendingStartedAt = time.Now()
				}
				if slurmBackend.PendingTimeout > 0 && time.Since(pendingStartedAt) >= slurmBackend.PendingTimeout {
					notifySubmissionState(onStateChanged, backend.SubmissionState{
						State:  "PENDING_TIMEOUT",
						Reason: currentState.Reason,
						Details: map[string]any{
							"job_id":             jobID,
							"reason":             currentState.Reason,
							"pending_timeout_ms": slurmBackend.PendingTimeout.Milliseconds(),
						},
					})
					_ = exec.CommandContext(context.WithoutCancel(ctx), "scancel", jobID).Run()
					return fmt.Errorf(
						"Slurm job %s exceeded pending timeout %s (reason: %s)",
						jobID,
						slurmBackend.PendingTimeout,
						currentState.Reason,
					)
				}
			} else {
				pendingStartedAt = time.Time{}
			}
		}
		select {
		case <-ctx.Done():
			_ = exec.Command("scancel", jobID).Run()
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func queryJobState(ctx context.Context, jobID string) (jobState, error) {
	squeueOutput, squeueErr := exec.CommandContext(ctx, "squeue", "-h", "-j", jobID, "-o", "%T|%R").Output()
	squeueState := ""
	squeueReason := ""
	if squeueErr == nil && strings.TrimSpace(string(squeueOutput)) != "" {
		firstLine := strings.TrimSpace(strings.Split(string(squeueOutput), "\n")[0])
		fields := strings.SplitN(firstLine, "|", 2)
		squeueState = strings.ToUpper(strings.TrimSpace(fields[0]))
		if len(fields) == 2 {
			squeueReason = strings.TrimSpace(fields[1])
		}
		if squeueState != "COMPLETING" {
			return jobState{State: squeueState, Reason: squeueReason}, nil
		}
	}

	accountingState, accountingErr := queryAccountingState(ctx, jobID)
	if accountingErr != nil {
		if squeueState != "" {
			return jobState{State: squeueState, Reason: squeueReason}, nil
		}
		return jobState{}, accountingErr
	}
	if accountingState == "" {
		return jobState{State: squeueState, Reason: squeueReason}, nil
	}
	return jobState{State: accountingState, Reason: squeueReason, Complete: isTerminalSlurmState(accountingState)}, nil
}

func queryState(ctx context.Context, jobID string) (string, bool, error) {
	currentState, err := queryJobState(ctx, jobID)
	return currentState.State, currentState.Complete, err
}

func queryAccountingState(ctx context.Context, jobID string) (string, error) {
	output, err := exec.CommandContext(ctx, "sacct", "-n", "-P", "-j", jobID, "-o", "JobIDRaw,State").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 2 || strings.TrimSpace(fields[0]) != jobID {
			continue
		}
		state := strings.Split(strings.TrimSpace(fields[1]), "+")[0]
		return strings.ToUpper(state), nil
	}
	return "", nil
}

func isTerminalSlurmState(state string) bool {
	switch state {
	case "COMPLETED", "FAILED", "CANCELLED", "TIMEOUT", "OUT_OF_MEMORY", "NODE_FAIL", "PREEMPTED":
		return true
	default:
		return false
	}
}

func (slurmBackend *Backend) collectMetrics(ctx context.Context, stepID string) (*metrics.TaskMetrics, map[string]string) {
	for attempt := 0; attempt < 4; attempt++ {
		collected, raw, err := queryStepMetrics(ctx, stepID)
		if err == nil {
			return collected, raw
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}
	return unavailableSlurmMetrics(stepID)
}

func (slurmBackend *Backend) collectMetricsOnce(ctx context.Context, stepID string) (*metrics.TaskMetrics, map[string]string) {
	collected, raw, err := queryStepMetrics(ctx, stepID)
	if err == nil {
		return collected, raw
	}
	return unavailableSlurmMetrics(stepID)
}

func queryStepMetrics(ctx context.Context, stepID string) (*metrics.TaskMetrics, map[string]string, error) {
	fields := "JobID,State,ExitCode,ElapsedRaw,AllocCPUS,TotalCPU,UserCPU,SystemCPU,MaxRSS,AveRSS,MaxVMSize,MaxDiskRead,MaxDiskWrite,ReqMem,NodeList,Start,End"
	output, err := exec.CommandContext(ctx, "sacct", "-n", "-P", "-j", stepID, "-o", fields).Output()
	if err != nil {
		return nil, nil, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parsed, raw, parseErr := metrics.ParseSlurmParsable(line)
		if parseErr == nil && raw["JobID"] == stepID {
			return parsed, raw, nil
		}
	}
	return nil, nil, fmt.Errorf("sacct returned no metrics for Slurm step %s", stepID)
}

func unavailableSlurmMetrics(stepID string) (*metrics.TaskMetrics, map[string]string) {
	raw := map[string]string{"JobID": stepID}
	return &metrics.TaskMetrics{Source: "slurm_sacct", Quality: "unavailable", Raw: raw}, raw
}

func shellValue(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func shortJobName(value string) string {
	if len(value) <= 80 {
		return value
	}
	return value[len(value)-80:]
}

func stableFilesystemName(value string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	stableName := replacer.Replace(value)
	if len(stableName) <= 100 {
		return stableName
	}
	return stableName[len(stableName)-100:]
}

func mapStringToAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
