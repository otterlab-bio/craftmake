package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/controllerlog"
	"github.com/otterlab-bio/craftmake/internal/metrics"
	"github.com/otterlab-bio/craftmake/internal/store"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type Options struct {
	ProjectDirectory  string
	StateDirectory    string
	ConfigPath        string
	ConfigDigest      string
	WorkflowPath      string
	WorkflowDigest    string
	Backend           backend.Backend
	MaxParallel       int
	MaxCores          int
	MaxMemoryBytes    int64
	Force             bool
	Version           string
	RunID             string
	ResumedFromRunID  string
	LoaderKind        string
	ControllerLogger  *controllerlog.Logger
	ControllerLogPath string
}

type Scheduler struct {
	plan                   *compiler.Plan
	store                  *store.Store
	options                Options
	executable             string
	controllerLogger       *controllerlog.Logger
	controllerLogPath      string
	controllerLogOpenError error
	ownsControllerLogger   bool
}

type submissionCompletion struct {
	submissionID string
	taskStatuses map[string]string
	err          error
}

type submissionAdmission struct {
	mutex                sync.Mutex
	capacity             int
	slotsInUse           int
	maximumCores         int
	availableCores       int
	maximumMemoryBytes   int64
	availableMemoryBytes int64
	changed              chan struct{}
}

type submissionLease struct {
	mutex      sync.Mutex
	admission  *submissionAdmission
	resources  protocol.ResourceRequest
	scheduler  *Scheduler
	runID      string
	submission string
	held       bool
}

type taskAttemptContext struct {
	task         *compiler.Task
	attemptID    string
	submissionID string
	manifest     *protocol.TaskManifest
	resources    protocol.ResourceRequest
	startedAt    time.Time
}

func New(plan *compiler.Plan, stateStore *store.Store, options Options) (*Scheduler, error) {
	if options.Backend == nil {
		return nil, fmt.Errorf("backend is required")
	}
	if options.MaxParallel <= 0 {
		options.MaxParallel = 1
	}
	if options.RunID == "" {
		options.RunID = uuid.NewString()
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve craftmake executable: %w", err)
	}
	controllerLogPath := options.ControllerLogPath
	if controllerLogPath == "" {
		controllerLogPath = controllerlog.DefaultPath(options.StateDirectory, options.RunID)
	}
	controllerLogger := options.ControllerLogger
	ownsControllerLogger := false
	var controllerLogOpenError error
	if controllerLogger == nil {
		controllerLogger, controllerLogOpenError = controllerlog.Open(controllerLogPath, stateStore)
		if controllerLogOpenError != nil {
			controllerLogger = controllerlog.New(nil, stateStore)
		} else {
			ownsControllerLogger = true
		}
	}
	return &Scheduler{
		plan:                   plan,
		store:                  stateStore,
		options:                options,
		executable:             executable,
		controllerLogger:       controllerLogger,
		controllerLogPath:      controllerLogPath,
		controllerLogOpenError: controllerLogOpenError,
		ownsControllerLogger:   ownsControllerLogger,
	}, nil
}

func (taskScheduler *Scheduler) ControllerLogPath() string {
	return taskScheduler.controllerLogPath
}

func (taskScheduler *Scheduler) ControllerLogErrors() []error {
	logErrors := taskScheduler.controllerLogger.Errors()
	if taskScheduler.controllerLogOpenError != nil {
		logErrors = append([]error{taskScheduler.controllerLogOpenError}, logErrors...)
	}
	return logErrors
}

func (taskScheduler *Scheduler) Run(ctx context.Context) (string, error) {
	if taskScheduler.ownsControllerLogger {
		defer taskScheduler.controllerLogger.Close()
	}
	runID := taskScheduler.options.RunID
	startedAt := time.Now().UTC()
	if err := taskScheduler.store.CreateRun(ctx, store.Run{
		ID:               runID,
		Workflow:         taskScheduler.plan.Workflow,
		Phase:            taskScheduler.plan.Phase,
		ConfigPath:       taskScheduler.options.ConfigPath,
		ConfigDigest:     taskScheduler.options.ConfigDigest,
		WorkflowPath:     taskScheduler.options.WorkflowPath,
		WorkflowDigest:   taskScheduler.options.WorkflowDigest,
		Backend:          taskScheduler.options.Backend.Name(),
		CraftmakeVersion: taskScheduler.options.Version,
		ResumedFromRunID: taskScheduler.options.ResumedFromRunID,
		LoaderKind:       taskScheduler.options.LoaderKind,
		Status:           "running",
		StartedAt:        startedAt,
	}); err != nil {
		return runID, err
	}
	taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
		Timestamp: startedAt,
		Name:      "run.started",
		RunID:     runID,
		Backend:   taskScheduler.options.Backend.Name(),
		Status:    "running",
		Details: map[string]any{
			"workflow":            taskScheduler.plan.Workflow,
			"phase":               taskScheduler.plan.Phase,
			"task_count":          len(taskScheduler.plan.Tasks),
			"submission_count":    len(taskScheduler.plan.Submissions),
			"max_parallel":        taskScheduler.options.MaxParallel,
			"max_cores":           taskScheduler.options.MaxCores,
			"max_memory_bytes":    taskScheduler.options.MaxMemoryBytes,
			"resumed_from_run_id": taskScheduler.options.ResumedFromRunID,
		},
	})

	statuses, err := taskScheduler.initializeTasks(ctx, runID)
	if err != nil {
		if ctx.Err() != nil {
			return runID, taskScheduler.cancelRun(ctx, runID, startedAt)
		}
		finishedAt := time.Now().UTC()
		_ = taskScheduler.store.FinishRun(ctx, runID, "failed", finishedAt)
		taskScheduler.logRunFinished(ctx, runID, startedAt, finishedAt, "failed", err)
		return runID, err
	}
	unschedulableErrors := taskScheduler.markUnschedulableSubmissions(ctx, runID, statuses)

	completionChannel := make(chan submissionCompletion, len(taskScheduler.plan.Submissions))
	admission := newSubmissionAdmission(taskScheduler.options)
	runningSubmissions := make(map[string]bool)
	runningCount := 0
	completedCount := countTerminal(statuses)

	for completedCount < len(taskScheduler.plan.Tasks) {
		madeProgress := false

		for _, taskID := range taskScheduler.plan.Order {
			if statuses[taskID] != "pending" {
				continue
			}
			task := taskScheduler.plan.TaskByID[taskID]
			if !hasFailedDependency(task, statuses) {
				continue
			}
			statuses[taskID] = "blocked"
			completedCount++
			madeProgress = true
			_ = taskScheduler.store.UpdateTaskStatus(ctx, runID, taskID, "blocked")
			taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
				Level:   "warn",
				Name:    "task.blocked",
				RunID:   runID,
				TaskID:  taskID,
				Backend: taskScheduler.options.Backend.Name(),
				Status:  "blocked",
				Details: map[string]any{"dependencies": append([]string(nil), task.Dependencies...)},
			})
		}

		for _, submission := range taskScheduler.plan.Submissions {
			if runningSubmissions[submission.ID] {
				continue
			}
			pendingTasks := pendingSubmissionTasks(submission, taskScheduler.plan.TaskByID, statuses)
			if len(pendingTasks) == 0 || !allTasksReady(pendingTasks, statuses) {
				continue
			}
			readyTasks := make([]*compiler.Task, 0, len(pendingTasks))
			for _, task := range pendingTasks {
				missingInputs := taskScheduler.missingTaskInputs(task)
				if len(missingInputs) == 0 {
					readyTasks = append(readyTasks, task)
					continue
				}
				statuses[task.ID] = "failed"
				completedCount++
				madeProgress = true
				_ = taskScheduler.store.UpdateTaskStatus(ctx, runID, task.ID, "failed")
				taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
					Level:   "error",
					Name:    "task.inputs_missing",
					RunID:   runID,
					TaskID:  task.ID,
					Backend: taskScheduler.options.Backend.Name(),
					Status:  "failed",
					Details: map[string]any{"missing_inputs": append([]string(nil), missingInputs...)},
				})
			}
			pendingTasks = readyTasks
			if len(pendingTasks) == 0 {
				continue
			}

			for _, task := range pendingTasks {
				statuses[task.ID] = "queued"
			}
			runningSubmissions[submission.ID] = true
			runningCount++
			madeProgress = true

			go func(submission compiler.SubmissionGroup, tasks []*compiler.Task) {
				lease := &submissionLease{
					admission:  admission,
					resources:  submission.Resources,
					scheduler:  taskScheduler,
					runID:      runID,
					submission: submission.ID,
				}
				if acquireErr := lease.acquire(ctx); acquireErr != nil {
					taskStatuses := make(map[string]string, len(tasks))
					for _, task := range tasks {
						taskStatuses[task.ID] = "failed"
						_ = taskScheduler.store.UpdateTaskStatus(ctx, runID, task.ID, "failed")
					}
					completionChannel <- submissionCompletion{submissionID: submission.ID, taskStatuses: taskStatuses, err: acquireErr}
					return
				}
				defer lease.release()
				for _, task := range tasks {
					_ = taskScheduler.store.UpdateTaskStatus(ctx, runID, task.ID, "running")
				}
				taskStatuses, executionErr := taskScheduler.executeSubmission(ctx, runID, submission, tasks, lease)
				completionChannel <- submissionCompletion{submissionID: submission.ID, taskStatuses: taskStatuses, err: executionErr}
			}(submission, pendingTasks)
		}

		if runningCount == 0 {
			if !madeProgress {
				break
			}
			continue
		}

		completion := <-completionChannel
		delete(runningSubmissions, completion.submissionID)
		runningCount--
		for taskID, status := range completion.taskStatuses {
			statuses[taskID] = status
			completedCount++
		}
	}

	if ctx.Err() != nil {
		return runID, taskScheduler.cancelRun(ctx, runID, startedAt)
	}

	finalStatus := "succeeded"
	persistedRunStatus, persistedStatusErr := taskScheduler.store.RunStatus(ctx, runID)
	if persistedStatusErr == nil && persistedRunStatus == "cancelled" {
		finalStatus = "cancelled"
	} else {
		for _, status := range statuses {
			if status == "failed" || status == "blocked" || status == "pending" || status == "running" || status == "cancelled" {
				finalStatus = "failed"
				break
			}
		}
	}
	finishedAt := time.Now().UTC()
	if finalStatus != "cancelled" {
		_ = taskScheduler.store.FinishRun(ctx, runID, finalStatus, finishedAt)
	}
	var runErr error
	if finalStatus == "cancelled" {
		runErr = context.Canceled
	} else if finalStatus != "succeeded" {
		runErr = fmt.Errorf("workflow run %s completed with failed, blocked, or unschedulable tasks", runID)
		if len(unschedulableErrors) > 0 {
			runErr = errors.Join(runErr, errors.Join(unschedulableErrors...))
		}
	}
	taskScheduler.logRunFinished(ctx, runID, startedAt, finishedAt, finalStatus, runErr)
	return runID, runErr
}

func newSubmissionAdmission(options Options) *submissionAdmission {
	return &submissionAdmission{
		capacity:             options.MaxParallel,
		maximumCores:         options.MaxCores,
		availableCores:       options.MaxCores,
		maximumMemoryBytes:   options.MaxMemoryBytes,
		availableMemoryBytes: options.MaxMemoryBytes,
		changed:              make(chan struct{}, 1),
	}
}

func (admission *submissionAdmission) acquire(ctx context.Context, resources protocol.ResourceRequest) error {
	for {
		admission.mutex.Lock()
		if admission.capacity > admission.slotsInUse &&
			coresFit(resources.Cores, admission.availableCores, admission.maximumCores) &&
			memoryFits(resources.MemoryByte, admission.availableMemoryBytes, admission.maximumMemoryBytes) {
			admission.slotsInUse++
			if admission.maximumCores > 0 {
				admission.availableCores -= resources.Cores
			}
			if admission.maximumMemoryBytes > 0 {
				admission.availableMemoryBytes -= resources.MemoryByte
			}
			admission.mutex.Unlock()
			return nil
		}
		admission.mutex.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-admission.changed:
		}
	}
}

func (admission *submissionAdmission) release(resources protocol.ResourceRequest) {
	admission.mutex.Lock()
	admission.slotsInUse--
	if admission.maximumCores > 0 {
		admission.availableCores += resources.Cores
	}
	if admission.maximumMemoryBytes > 0 {
		admission.availableMemoryBytes += resources.MemoryByte
	}
	admission.mutex.Unlock()
	select {
	case admission.changed <- struct{}{}:
	default:
	}
}

func (lease *submissionLease) acquire(ctx context.Context) error {
	lease.mutex.Lock()
	if lease.held {
		lease.mutex.Unlock()
		return nil
	}
	lease.mutex.Unlock()
	if err := lease.admission.acquire(ctx, lease.resources); err != nil {
		return err
	}
	lease.mutex.Lock()
	lease.held = true
	lease.mutex.Unlock()
	lease.log("submission.slot_acquired", "running")
	return nil
}

func (lease *submissionLease) release() {
	lease.mutex.Lock()
	if !lease.held {
		lease.mutex.Unlock()
		return
	}
	lease.held = false
	lease.mutex.Unlock()
	lease.admission.release(lease.resources)
	lease.log("submission.slot_released", "waiting")
}

func (lease *submissionLease) log(eventName, status string) {
	lease.admission.mutex.Lock()
	slotsInUse := lease.admission.slotsInUse
	lease.admission.mutex.Unlock()
	lease.scheduler.controllerLogger.Log(context.Background(), controllerlog.Event{
		Name:         eventName,
		RunID:        lease.runID,
		SubmissionID: lease.submission,
		Backend:      lease.scheduler.options.Backend.Name(),
		Status:       status,
		Details: map[string]any{
			"slots_in_use":  slotsInUse,
			"slot_capacity": lease.admission.capacity,
		},
	})
}

func (taskScheduler *Scheduler) cancelRun(ctx context.Context, runID string, startedAt time.Time) error {
	cleanupContext, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelCleanup()

	taskScheduler.controllerLogger.Log(cleanupContext, controllerlog.Event{
		Name:    "run.cancellation_requested",
		RunID:   runID,
		Backend: taskScheduler.options.Backend.Name(),
		Status:  "running",
	})
	backendCancellationErr := taskScheduler.options.Backend.Cancel(cleanupContext)
	finishedAt := time.Now().UTC()
	if err := taskScheduler.store.CancelRun(cleanupContext, runID, finishedAt); err != nil {
		cancellationErr := fmt.Errorf("cancel workflow run: %w", err)
		taskScheduler.logRunFinished(cleanupContext, runID, startedAt, finishedAt, "failed", cancellationErr)
		return cancellationErr
	}
	taskScheduler.logRunFinished(cleanupContext, runID, startedAt, finishedAt, "cancelled", backendCancellationErr)
	return context.Canceled
}

func (taskScheduler *Scheduler) initializeTasks(ctx context.Context, runID string) (map[string]string, error) {
	statuses := make(map[string]string, len(taskScheduler.plan.Tasks))
	for _, taskID := range taskScheduler.plan.Order {
		task := taskScheduler.plan.TaskByID[taskID]
		if task == nil {
			return nil, fmt.Errorf("task %q from plan order is missing", taskID)
		}
		definitionFingerprint := task.Fingerprint
		fingerprint, dependencyFingerprint := runtimeFingerprints(task, taskScheduler.plan.TaskByID, definitionFingerprint)
		task.Fingerprint = fingerprint

		cacheDecision := store.CacheDecision{}
		switch {
		case taskScheduler.options.Force:
			cacheDecision = store.CacheDecision{
				Decision:   "bypass",
				ReasonCode: "forced",
				Detail:     "cache lookup was bypassed by --force",
			}
		case !allDependenciesCached(task, statuses):
			cacheDecision = store.CacheDecision{
				Decision:   "miss",
				ReasonCode: "dependency_not_cached",
				Detail:     dependencyCacheMissDetail(task, statuses),
			}
		default:
			var err error
			cacheDecision, err = taskScheduler.store.EvaluateCache(
				ctx,
				task.ID,
				fingerprint,
				definitionFingerprint,
				dependencyFingerprint,
				taskScheduler.options.ProjectDirectory,
				task.Inputs,
				task.Outputs,
			)
			if err != nil {
				return nil, err
			}
		}
		status := "pending"
		if cacheDecision.Hit {
			status = "cached"
		}
		statuses[task.ID] = status
		if err := taskScheduler.store.UpsertTask(ctx, store.TaskInstance{
			RunID:                 runID,
			TaskID:                task.ID,
			JobID:                 task.JobID,
			Dimensions:            task.Dimensions,
			Inputs:                task.Inputs,
			Outputs:               task.Outputs,
			Fingerprint:           fingerprint,
			DefinitionFingerprint: definitionFingerprint,
			DependencyFingerprint: dependencyFingerprint,
			CacheDecision:         cacheDecision.Decision,
			CacheReasonCode:       cacheDecision.ReasonCode,
			CacheReasonDetail:     cacheDecision.Detail,
			Status:                status,
		}); err != nil {
			return nil, err
		}
		taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
			Name:    "task.cache_evaluated",
			RunID:   runID,
			TaskID:  task.ID,
			Backend: taskScheduler.options.Backend.Name(),
			Status:  status,
			Details: map[string]any{
				"decision":    cacheDecision.Decision,
				"reason_code": cacheDecision.ReasonCode,
				"detail":      cacheDecision.Detail,
				"fingerprint": abbreviatedRuntimeFingerprint(fingerprint),
			},
		})
		for _, dependencyID := range task.Dependencies {
			if err := taskScheduler.store.AddDependency(ctx, store.Dependency{RunID: runID, UpstreamTaskID: dependencyID, DownstreamTaskID: task.ID, Type: "artifact"}); err != nil {
				return nil, err
			}
		}
	}
	return statuses, nil
}

func (taskScheduler *Scheduler) executeSubmission(ctx context.Context, runID string, submission compiler.SubmissionGroup, tasks []*compiler.Task, lease *submissionLease) (map[string]string, error) {
	taskStatuses := make(map[string]string, len(tasks))
	attemptNumbers := make(map[string]int, len(tasks))
	remainingTasks := append([]*compiler.Task(nil), tasks...)
	var lastSubmissionErr error

	for len(remainingTasks) > 0 {
		if ctx.Err() != nil {
			lastSubmissionErr = ctx.Err()
			for _, task := range remainingTasks {
				taskStatuses[task.ID] = "failed"
				_ = taskScheduler.store.UpdateTaskStatus(ctx, runID, task.ID, "failed")
			}
			break
		}

		attemptTasks := make([]*compiler.Task, 0, len(remainingTasks))
		for _, task := range remainingTasks {
			if attemptNumbers[task.ID] >= task.MaxAttempts {
				taskStatuses[task.ID] = "failed"
				continue
			}
			attemptNumbers[task.ID]++
			attemptTasks = append(attemptTasks, task)
		}
		if len(attemptTasks) == 0 {
			break
		}

		roundNumber := maximumAttemptNumber(attemptTasks, attemptNumbers)
		roundStatuses, roundErr := taskScheduler.executeSubmissionRound(ctx, runID, submission, attemptTasks, attemptNumbers, roundNumber, lease)
		lastSubmissionErr = roundErr
		remainingTasks = remainingTasks[:0]
		for _, task := range attemptTasks {
			status := roundStatuses[task.ID]
			taskStatuses[task.ID] = status
			if status == "failed" && attemptNumbers[task.ID] < task.MaxAttempts {
				remainingTasks = append(remainingTasks, task)
				taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
					Level:         "warn",
					Name:          "attempt.retry_scheduled",
					RunID:         runID,
					TaskID:        task.ID,
					AttemptNumber: attemptNumbers[task.ID],
					Backend:       taskScheduler.options.Backend.Name(),
					Status:        "failed",
					Details: map[string]any{
						"next_attempt": attemptNumbers[task.ID] + 1,
						"max_attempts": task.MaxAttempts,
					},
				})
			}
		}
	}

	return taskStatuses, lastSubmissionErr
}

func (taskScheduler *Scheduler) executeSubmissionRound(ctx context.Context, runID string, submission compiler.SubmissionGroup, tasks []*compiler.Task, attemptNumbers map[string]int, roundNumber int, lease *submissionLease) (map[string]string, error) {
	submissionID := runID + ":" + stableFilesystemName(submission.ID) + fmt.Sprintf("-round-%03d", roundNumber)
	submissionRuntimeDirectory := filepath.Join(taskScheduler.options.StateDirectory, "runs", runID, "submissions", stableFilesystemName(submission.ID), fmt.Sprintf("round-%03d", roundNumber))
	startedAt := time.Now().UTC()
	resourcesJSON, _ := json.Marshal(submission.Resources)
	if err := taskScheduler.store.CreateSubmission(ctx, store.Submission{
		ID:        submissionID,
		RunID:     runID,
		Backend:   taskScheduler.options.Backend.Name(),
		Scope:     submission.Scope,
		GroupKey:  submission.GroupKey,
		Resources: resourcesJSON,
		Status:    "running",
		StartedAt: &startedAt,
	}); err != nil {
		return failedTaskStatuses(tasks), err
	}
	taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
		Timestamp:    startedAt,
		Name:         "submission.created",
		RunID:        runID,
		SubmissionID: submissionID,
		Backend:      taskScheduler.options.Backend.Name(),
		Status:       "running",
		Details: map[string]any{
			"scope":             submission.Scope,
			"group_key":         submission.GroupKey,
			"task_count":        len(tasks),
			"round":             roundNumber,
			"cores":             submission.Resources.Cores,
			"memory_bytes":      submission.Resources.MemoryByte,
			"runtime_directory": submissionRuntimeDirectory,
		},
	})

	attempts := make(map[string]taskAttemptContext, len(tasks))
	manifests := make([]*protocol.TaskManifest, 0, len(tasks))
	for _, task := range tasks {
		_ = taskScheduler.store.UpdateTaskStatus(ctx, runID, task.ID, "running")
		attempt, err := taskScheduler.createTaskAttempt(ctx, runID, submissionID, task, attemptNumbers[task.ID])
		if err != nil {
			finishedAt := time.Now().UTC()
			_ = taskScheduler.store.FinishSubmission(ctx, submissionID, "failed", "", finishedAt, marshalMetadata(map[string]any{"error": err.Error()}))
			taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
				Timestamp:            finishedAt,
				Level:                "error",
				Name:                 "submission.finished",
				RunID:                runID,
				SubmissionID:         submissionID,
				Backend:              taskScheduler.options.Backend.Name(),
				Status:               "failed",
				DurationMilliseconds: durationMilliseconds(startedAt, finishedAt),
				Error:                err.Error(),
			})
			return failedTaskStatuses(tasks), err
		}
		attempts[task.ID] = attempt
		manifests = append(manifests, attempt.manifest)
	}

	workerResources := protocol.ResourceRequest{}
	workerMaxParallel := 1
	if submission.Worker != nil {
		workerResources = submission.Worker.Resources
		workerMaxParallel = effectiveWorkerParallelForTaskCount(submission, len(tasks))
	}
	request := backend.SubmissionRequest{
		SubmissionID:      submissionID,
		Scope:             submission.Scope,
		GroupKey:          submission.GroupKey,
		RuntimeDirectory:  submissionRuntimeDirectory,
		Resources:         submission.Resources,
		WorkerResources:   workerResources,
		WorkerMaxParallel: workerMaxParallel,
		Manifests:         manifests,
		OnStarted: func(backendJobID string, metadata map[string]any) error {
			if err := taskScheduler.store.UpdateSubmissionStarted(ctx, submissionID, backendJobID, marshalMetadata(metadata)); err != nil {
				return err
			}
			taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
				Name:         "submission.backend_started",
				RunID:        runID,
				SubmissionID: submissionID,
				Backend:      taskScheduler.options.Backend.Name(),
				BackendJobID: backendJobID,
				Status:       "running",
				Details:      cloneEventDetails(metadata),
			})
			return nil
		},
		OnStateChanged: func(state backend.SubmissionState) error {
			backendJobID, _ := state.Details["job_id"].(string)
			taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
				Level:        submissionStateEventLevel(state.State),
				Name:         submissionStateEventName(state.State),
				RunID:        runID,
				SubmissionID: submissionID,
				Backend:      taskScheduler.options.Backend.Name(),
				BackendJobID: backendJobID,
				Status:       strings.ToLower(state.State),
				Error:        state.Reason,
				Details:      cloneEventDetails(state.Details),
			})
			switch strings.ToUpper(state.State) {
			case "SUBMIT_RETRY_WAIT":
				lease.release()
			case "SUBMIT_RETRY_READY":
				if err := lease.acquire(ctx); err != nil {
					return err
				}
			}
			return nil
		},
	}
	submissionResult, submissionErr := taskScheduler.options.Backend.RunSubmission(ctx, taskScheduler.executable, request)
	finishedAt := time.Now().UTC()
	persistedRunStatus, persistedRunStatusErr := taskScheduler.store.RunStatus(ctx, runID)
	if persistedRunStatusErr == nil && persistedRunStatus == "cancelled" {
		cancelledStatuses := make(map[string]string, len(tasks))
		for _, task := range tasks {
			cancelledStatuses[task.ID] = "cancelled"
		}
		return cancelledStatuses, context.Canceled
	}
	taskStatuses := make(map[string]string, len(tasks))
	allTasksSucceeded := true

	for _, task := range tasks {
		attempt := attempts[task.ID]
		outcome, exists := submissionOutcome(submissionResult, task.ID)
		if !exists {
			outcome.Err = fmt.Errorf("backend returned no outcome for task %s", task.ID)
			if submissionErr != nil {
				outcome.Err = fmt.Errorf("backend submission failed: %w", submissionErr)
			}
		}
		status := taskScheduler.finishTaskAttempt(ctx, runID, attempt, outcome, finishedAt)
		taskStatuses[task.ID] = status
		if status != "succeeded" {
			allTasksSucceeded = false
		}
	}

	submissionStatus := "succeeded"
	if submissionErr != nil || !allTasksSucceeded {
		submissionStatus = "failed"
	}
	backendJobID := ""
	metadata := map[string]any{}
	if submissionResult != nil {
		backendJobID = submissionResult.BackendID
		metadata = submissionResult.Raw
	}
	if submissionErr != nil {
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["error"] = submissionErr.Error()
	}
	_ = taskScheduler.store.FinishSubmission(ctx, submissionID, submissionStatus, backendJobID, finishedAt, marshalMetadata(metadata))
	taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
		Timestamp:            finishedAt,
		Level:                eventLevelForStatus(submissionStatus),
		Name:                 "submission.finished",
		RunID:                runID,
		SubmissionID:         submissionID,
		Backend:              taskScheduler.options.Backend.Name(),
		BackendJobID:         backendJobID,
		Status:               submissionStatus,
		DurationMilliseconds: durationMilliseconds(startedAt, finishedAt),
		Error:                errorMessage(submissionErr),
		Details:              cloneEventDetails(metadata),
	})
	return taskStatuses, submissionErr
}

func (taskScheduler *Scheduler) createTaskAttempt(ctx context.Context, runID, submissionID string, task *compiler.Task, attemptNumber int) (taskAttemptContext, error) {
	attemptID := runID + ":" + stableFilesystemName(task.ID) + fmt.Sprintf("-attempt-%03d", attemptNumber)
	runtimeDirectory := filepath.Join(taskScheduler.options.StateDirectory, "runs", runID, "tasks", stableFilesystemName(task.ID), fmt.Sprintf("attempt-%03d", attemptNumber))
	resources := task.Resources
	if task.Scope == "batch" && task.Worker != nil {
		resources = task.Worker.Resources
	}
	manifest := &protocol.TaskManifest{
		ProtocolVersion:     protocol.Version,
		RunID:               runID,
		TaskID:              task.ID,
		JobID:               task.JobID,
		Attempt:             attemptNumber,
		Workflow:            task.Workflow,
		Phase:               task.Phase,
		Scope:               task.Scope,
		Dimensions:          task.Dimensions,
		Inputs:              task.Inputs,
		Outputs:             task.Outputs,
		Resources:           resources,
		WorkDirectory:       taskScheduler.options.ProjectDirectory,
		TempDirectory:       filepath.Join(runtimeDirectory, "tmp"),
		RuntimeDirectory:    runtimeDirectory,
		ResultPath:          filepath.Join(runtimeDirectory, "result.json"),
		CompressSuccessLogs: task.CompressSuccessLogs,
		Steps:               cloneSteps(task.Steps),
	}
	startedAt := time.Now().UTC()
	if err := taskScheduler.store.CreateAttempt(ctx, store.TaskAttempt{
		ID:            attemptID,
		RunID:         runID,
		TaskID:        task.ID,
		AttemptNumber: attemptNumber,
		SubmissionID:  submissionID,
		Status:        "running",
		StartedAt:     &startedAt,
		ResultPath:    manifest.ResultPath,
	}); err != nil {
		return taskAttemptContext{}, err
	}
	taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
		Timestamp:     startedAt,
		Name:          "attempt.started",
		RunID:         runID,
		SubmissionID:  submissionID,
		TaskID:        task.ID,
		AttemptID:     attemptID,
		AttemptNumber: attemptNumber,
		Backend:       taskScheduler.options.Backend.Name(),
		Status:        "running",
		Details: map[string]any{
			"scope":             task.Scope,
			"cores":             resources.Cores,
			"memory_bytes":      resources.MemoryByte,
			"runtime_directory": runtimeDirectory,
			"result_path":       manifest.ResultPath,
		},
	})
	return taskAttemptContext{
		task:         task,
		attemptID:    attemptID,
		submissionID: submissionID,
		manifest:     manifest,
		resources:    resources,
		startedAt:    startedAt,
	}, nil
}

func (taskScheduler *Scheduler) finishTaskAttempt(ctx context.Context, runID string, attempt taskAttemptContext, outcome backend.TaskOutcome, finishedAt time.Time) string {
	status := "succeeded"
	if outcome.Err != nil || outcome.Result == nil || outcome.Result.TaskResult == nil || outcome.Result.TaskResult.Status != "succeeded" {
		status = "failed"
	}

	result := &protocol.TaskResult{
		ProtocolVersion: protocol.Version,
		RunID:           runID,
		TaskID:          attempt.task.ID,
		Attempt:         attempt.manifest.Attempt,
		Status:          status,
		StartedAt:       attempt.startedAt,
		FinishedAt:      finishedAt,
		ExitCode:        -1,
	}
	if outcome.Result != nil && outcome.Result.TaskResult != nil {
		result = outcome.Result.TaskResult
	}
	if outcome.Err != nil && result.Error == "" {
		result.Error = outcome.Err.Error()
	}
	backendJobID := ""
	if outcome.Result != nil {
		backendJobID = outcome.Result.BackendID
	}
	if status != "succeeded" {
		result.Incident = protocol.ClassifyTaskIncident(*result, taskScheduler.options.Backend.Name())
		if result.Incident != nil {
			result.Incident.BackendJobID = backendJobID
			result.Incident.EvidencePaths = []string{attempt.manifest.ResultPath}
			_ = taskScheduler.store.SaveRuntimeIncident(ctx, store.RuntimeIncident{
				ID:                attempt.attemptID + "-incident",
				RunID:             runID,
				AttemptID:         attempt.attemptID,
				SchemaVersion:     result.Incident.SchemaVersion,
				Category:          string(result.Incident.Category),
				Scope:             string(result.Incident.Scope),
				RetrySafe:         result.Incident.RetrySafe,
				RetryPolicy:       string(result.Incident.RetryPolicy),
				Owner:             result.Incident.Owner,
				Escalation:        result.Incident.Escalation,
				RemediationStatus: string(result.Incident.RemediationStatus),
				Summary:           result.Incident.Summary,
				FirstObservedAt:   result.Incident.FirstObservedAt,
				Executor:          result.Incident.Executor,
				Backend:           result.Incident.Backend,
				BackendJobID:      result.Incident.BackendJobID,
				ExitCode:          &result.Incident.ExitCode,
				Signal:            result.Incident.Signal,
				DiagnosticPaths:   result.Incident.DiagnosticPaths,
				EvidencePaths:     result.Incident.EvidencePaths,
			})
		}
	}
	artifacts := collectTaskArtifacts(taskScheduler.options.ProjectDirectory, attempt.task)
	_ = taskScheduler.store.FinishAttempt(ctx, attempt.attemptID, status, result, artifacts)
	collectedMetrics := &metrics.TaskMetrics{Source: "artifact_snapshot", Quality: "metadata", Raw: map[string]string{}}
	if outcome.Result != nil && outcome.Result.Metrics != nil {
		collectedMetrics = outcome.Result.Metrics
	}
	inputArtifactBytes, outputArtifactBytes := artifactByteTotals(artifacts)
	collectedMetrics.InputArtifactBytes = &inputArtifactBytes
	collectedMetrics.OutputArtifactBytes = &outputArtifactBytes
	_ = taskScheduler.store.SaveMetrics(ctx, attempt.attemptID, collectedMetrics, int64(attempt.resources.Cores), attempt.resources.MemoryByte)
	_ = taskScheduler.store.UpdateTaskStatus(ctx, runID, attempt.task.ID, status)
	taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
		Timestamp:            result.FinishedAt,
		Level:                eventLevelForStatus(status),
		Name:                 "attempt.finished",
		RunID:                runID,
		SubmissionID:         attempt.submissionID,
		TaskID:               attempt.task.ID,
		AttemptID:            attempt.attemptID,
		AttemptNumber:        attempt.manifest.Attempt,
		Backend:              taskScheduler.options.Backend.Name(),
		BackendJobID:         backendJobID,
		Status:               status,
		DurationMilliseconds: durationMilliseconds(result.StartedAt, result.FinishedAt),
		Error:                result.Error,
		Details: map[string]any{
			"exit_code":           result.ExitCode,
			"metric_source":       collectedMetrics.Source,
			"metric_quality":      collectedMetrics.Quality,
			"result_path":         attempt.manifest.ResultPath,
			"incident_category":   incidentCategory(result.Incident),
			"incident_retry_safe": incidentRetrySafe(result.Incident),
		},
	})
	return status
}

func incidentCategory(incident *protocol.Incident) string {
	if incident == nil {
		return ""
	}
	return string(incident.Category)
}

func incidentRetrySafe(incident *protocol.Incident) bool {
	return incident != nil && incident.RetrySafe
}

func collectTaskArtifacts(projectDirectory string, task *compiler.Task) []store.Artifact {
	artifacts := make([]store.Artifact, 0, len(task.Inputs)+len(task.Outputs))
	inputNames := make([]string, 0, len(task.Inputs))
	for inputName := range task.Inputs {
		inputNames = append(inputNames, inputName)
	}
	sort.Strings(inputNames)
	for _, inputName := range inputNames {
		inputPaths := task.Inputs[inputName]
		for pathIndex, inputPath := range inputPaths {
			artifactName := inputName
			if len(inputPaths) > 1 {
				artifactName = fmt.Sprintf("%s[%d]", inputName, pathIndex)
			}
			artifacts = append(artifacts, snapshotArtifact(projectDirectory, "input", artifactName, inputPath))
		}
	}

	outputNames := make([]string, 0, len(task.Outputs))
	for outputName := range task.Outputs {
		outputNames = append(outputNames, outputName)
	}
	sort.Strings(outputNames)
	for _, outputName := range outputNames {
		artifacts = append(artifacts, snapshotArtifact(projectDirectory, "output", outputName, task.Outputs[outputName]))
	}
	return artifacts
}

func snapshotArtifact(projectDirectory, role, name, artifactPath string) store.Artifact {
	resolvedPath := strings.TrimSpace(artifactPath)
	artifact := store.Artifact{Role: role, Name: name, Path: resolvedPath, ValidationStatus: "missing"}
	if resolvedPath == "" {
		return artifact
	}
	if !filepath.IsAbs(resolvedPath) {
		resolvedPath = filepath.Join(projectDirectory, resolvedPath)
	}
	resolvedPath = filepath.Clean(resolvedPath)
	artifact.Path = resolvedPath
	fileInfo, err := os.Stat(resolvedPath)
	if err != nil {
		if !os.IsNotExist(err) {
			artifact.ValidationStatus = "unreadable"
		}
		return artifact
	}
	sizeBytes := fileInfo.Size()
	modificationTime := fileInfo.ModTime().UTC()
	artifact.SizeBytes = &sizeBytes
	artifact.ModificationTime = &modificationTime
	artifact.ValidationStatus = "valid"
	return artifact
}

func artifactByteTotals(artifacts []store.Artifact) (int64, int64) {
	var inputArtifactBytes int64
	var outputArtifactBytes int64
	for _, artifact := range artifacts {
		if artifact.ValidationStatus != "valid" || artifact.SizeBytes == nil {
			continue
		}
		switch artifact.Role {
		case "input":
			inputArtifactBytes += *artifact.SizeBytes
		case "output":
			outputArtifactBytes += *artifact.SizeBytes
		}
	}
	return inputArtifactBytes, outputArtifactBytes
}

func (taskScheduler *Scheduler) missingTaskInputs(task *compiler.Task) []string {
	missingInputs := make([]string, 0)
	inputNames := make([]string, 0, len(task.Inputs))
	for inputName := range task.Inputs {
		inputNames = append(inputNames, inputName)
	}
	sort.Strings(inputNames)
	for _, inputName := range inputNames {
		for _, inputPath := range task.Inputs[inputName] {
			resolvedPath := strings.TrimSpace(inputPath)
			if resolvedPath != "" && !filepath.IsAbs(resolvedPath) {
				resolvedPath = filepath.Join(taskScheduler.options.ProjectDirectory, resolvedPath)
			}
			if resolvedPath == "" {
				missingInputs = append(missingInputs, inputName+"=<empty>")
				continue
			}
			if _, err := os.Stat(resolvedPath); err != nil {
				missingInputs = append(missingInputs, inputName+"="+resolvedPath)
			}
		}
	}
	return missingInputs
}

func pendingSubmissionTasks(submission compiler.SubmissionGroup, taskByID map[string]*compiler.Task, statuses map[string]string) []*compiler.Task {
	tasks := make([]*compiler.Task, 0, len(submission.TaskIDs))
	for _, taskID := range submission.TaskIDs {
		if statuses[taskID] == "pending" {
			tasks = append(tasks, taskByID[taskID])
		}
	}
	return tasks
}

func allTasksReady(tasks []*compiler.Task, statuses map[string]string) bool {
	for _, task := range tasks {
		if !dependenciesCompleted(task, statuses) {
			return false
		}
	}
	return true
}

func failedTaskStatuses(tasks []*compiler.Task) map[string]string {
	statuses := make(map[string]string, len(tasks))
	for _, task := range tasks {
		statuses[task.ID] = "failed"
	}
	return statuses
}

func submissionOutcome(result *backend.SubmissionResult, taskID string) (backend.TaskOutcome, bool) {
	if result == nil || result.Tasks == nil {
		return backend.TaskOutcome{}, false
	}
	outcome, exists := result.Tasks[taskID]
	return outcome, exists
}

func effectiveWorkerParallel(submission compiler.SubmissionGroup) int {
	return effectiveWorkerParallelForTaskCount(submission, len(submission.TaskIDs))
}

func effectiveWorkerParallelForTaskCount(submission compiler.SubmissionGroup, taskCount int) int {
	if submission.Worker == nil {
		return 1
	}
	maximumParallelWorkers := submission.Worker.MaxParallel
	if maximumParallelWorkers <= 0 {
		maximumParallelWorkers = 1
	}
	if taskCount > 0 && maximumParallelWorkers > taskCount {
		maximumParallelWorkers = taskCount
	}
	if workerCores := submission.Worker.Resources.Cores; workerCores > 0 {
		coreCapacity := submission.Resources.Cores / workerCores
		if coreCapacity > 0 && maximumParallelWorkers > coreCapacity {
			maximumParallelWorkers = coreCapacity
		}
	}
	if workerMemory := submission.Worker.Resources.MemoryByte; workerMemory > 0 && submission.Resources.MemoryByte > 0 {
		memoryCapacity := int(submission.Resources.MemoryByte / workerMemory)
		if memoryCapacity > 0 && maximumParallelWorkers > memoryCapacity {
			maximumParallelWorkers = memoryCapacity
		}
	}
	if maximumParallelWorkers <= 0 {
		return 1
	}
	return maximumParallelWorkers
}

func maximumAttemptNumber(tasks []*compiler.Task, attemptNumbers map[string]int) int {
	maximum := 1
	for _, task := range tasks {
		if attemptNumbers[task.ID] > maximum {
			maximum = attemptNumbers[task.ID]
		}
	}
	return maximum
}

func abbreviatedRuntimeFingerprint(fingerprint string) string {
	if len(fingerprint) <= 12 {
		return fingerprint
	}
	return fingerprint[:12]
}

func runtimeFingerprints(
	task *compiler.Task,
	tasksByID map[string]*compiler.Task,
	definitionFingerprint string,
) (string, string) {
	runtimeHash := sha256.New()
	runtimeHash.Write([]byte(definitionFingerprint))
	dependencyHash := sha256.New()

	dependencyIDs := append([]string(nil), task.Dependencies...)
	sort.Strings(dependencyIDs)
	for _, dependencyID := range dependencyIDs {
		dependencyTask := tasksByID[dependencyID]
		if dependencyTask == nil {
			continue
		}
		fingerprintComponent := "dependency=" + dependencyID + ":" + dependencyTask.Fingerprint
		runtimeHash.Write([]byte(fingerprintComponent))
		dependencyHash.Write([]byte(fingerprintComponent))
	}
	return hex.EncodeToString(runtimeHash.Sum(nil)), hex.EncodeToString(dependencyHash.Sum(nil))
}

func resolveProjectPath(projectDirectory, artifactPath string) string {
	resolvedPath := artifactPath
	if !filepath.IsAbs(resolvedPath) {
		resolvedPath = filepath.Join(projectDirectory, resolvedPath)
	}
	return filepath.Clean(resolvedPath)
}

func allDependenciesCached(task *compiler.Task, statuses map[string]string) bool {
	for _, dependencyID := range task.Dependencies {
		if statuses[dependencyID] != "cached" {
			return false
		}
	}
	return true
}

func dependencyCacheMissDetail(task *compiler.Task, statuses map[string]string) string {
	uncachedDependencies := make([]string, 0, len(task.Dependencies))
	for _, dependencyID := range task.Dependencies {
		dependencyStatus := statuses[dependencyID]
		if dependencyStatus == "cached" {
			continue
		}
		if dependencyStatus == "" {
			dependencyStatus = "not_initialized"
		}
		uncachedDependencies = append(uncachedDependencies, dependencyID+"="+dependencyStatus)
	}
	sort.Strings(uncachedDependencies)
	return "dependencies were not cache hits: " + strings.Join(uncachedDependencies, ", ")
}

func cloneSteps(steps []protocol.StepManifest) []protocol.StepManifest {
	result := make([]protocol.StepManifest, len(steps))
	copy(result, steps)
	return result
}

func dependenciesCompleted(task *compiler.Task, statuses map[string]string) bool {
	for _, dependencyID := range task.Dependencies {
		status := statuses[dependencyID]
		if status != "succeeded" && status != "cached" {
			return false
		}
	}
	return true
}

func hasFailedDependency(task *compiler.Task, statuses map[string]string) bool {
	for _, dependencyID := range task.Dependencies {
		status := statuses[dependencyID]
		if status == "failed" || status == "blocked" {
			return true
		}
	}
	return false
}

func (taskScheduler *Scheduler) markUnschedulableSubmissions(
	ctx context.Context,
	runID string,
	statuses map[string]string,
) []error {
	var unschedulableErrors []error
	for _, submission := range taskScheduler.plan.Submissions {
		pendingTasks := pendingSubmissionTasks(submission, taskScheduler.plan.TaskByID, statuses)
		if len(pendingTasks) == 0 {
			continue
		}
		capacityErr := submissionCapacityError(submission, taskScheduler.options.MaxCores, taskScheduler.options.MaxMemoryBytes)
		if capacityErr == nil {
			continue
		}
		unschedulableErrors = append(unschedulableErrors, capacityErr)
		details := map[string]any{
			"required_cores":        submission.Resources.Cores,
			"required_memory_bytes": submission.Resources.MemoryByte,
			"maximum_cores":         taskScheduler.options.MaxCores,
			"maximum_memory_bytes":  taskScheduler.options.MaxMemoryBytes,
		}
		taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
			Level:        "error",
			Name:         "submission.unschedulable",
			RunID:        runID,
			SubmissionID: submission.ID,
			Backend:      taskScheduler.options.Backend.Name(),
			Status:       "failed",
			Error:        capacityErr.Error(),
			Details:      cloneEventDetails(details),
		})
		for _, task := range pendingTasks {
			statuses[task.ID] = "failed"
			_ = taskScheduler.store.UpdateTaskStatus(ctx, runID, task.ID, "failed")
			taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
				Level:   "error",
				Name:    "task.unschedulable",
				RunID:   runID,
				TaskID:  task.ID,
				Backend: taskScheduler.options.Backend.Name(),
				Status:  "failed",
				Error:   capacityErr.Error(),
				Details: cloneEventDetails(details),
			})
		}
	}
	return unschedulableErrors
}

func submissionCapacityError(submission compiler.SubmissionGroup, maximumCores int, maximumMemoryBytes int64) error {
	switch {
	case maximumCores > 0 && submission.Resources.Cores > maximumCores:
		return fmt.Errorf(
			"submission %s requires %d cores but the scheduler admission limit is %d",
			submission.ID,
			submission.Resources.Cores,
			maximumCores,
		)
	case maximumMemoryBytes > 0 && submission.Resources.MemoryByte > maximumMemoryBytes:
		return fmt.Errorf(
			"submission %s requires %d bytes of memory but the scheduler admission limit is %d",
			submission.ID,
			submission.Resources.MemoryByte,
			maximumMemoryBytes,
		)
	default:
		return nil
	}
}

func coresFit(requested, available, maximum int) bool {
	return maximum <= 0 || requested <= available
}

func memoryFits(requested, available, maximum int64) bool {
	return maximum <= 0 || requested <= available
}

func countTerminal(statuses map[string]string) int {
	count := 0
	for _, status := range statuses {
		if status == "cached" || status == "succeeded" || status == "failed" || status == "blocked" || status == "cancelled" {
			count++
		}
	}
	return count
}

func stableFilesystemName(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:12])
}

func (taskScheduler *Scheduler) logRunFinished(
	ctx context.Context,
	runID string,
	startedAt time.Time,
	finishedAt time.Time,
	status string,
	runErr error,
) {
	taskScheduler.controllerLogger.Log(ctx, controllerlog.Event{
		Timestamp:            finishedAt,
		Level:                eventLevelForStatus(status),
		Name:                 "run.finished",
		RunID:                runID,
		Backend:              taskScheduler.options.Backend.Name(),
		Status:               status,
		DurationMilliseconds: durationMilliseconds(startedAt, finishedAt),
		Error:                errorMessage(runErr),
	})
}

func durationMilliseconds(startedAt time.Time, finishedAt time.Time) int64 {
	if startedAt.IsZero() || finishedAt.IsZero() || finishedAt.Before(startedAt) {
		return 0
	}
	return finishedAt.Sub(startedAt).Milliseconds()
}

func submissionStateEventName(state string) string {
	switch strings.ToUpper(state) {
	case "SUBMIT_RETRY_WAIT":
		return "submission.submit_retry_scheduled"
	case "PENDING":
		return "submission.pending"
	case "RUNNING", "CONFIGURING", "COMPLETING":
		return "submission.backend_state_changed"
	case "PENDING_TIMEOUT":
		return "submission.pending_timeout"
	default:
		return "submission.backend_state_changed"
	}
}

func submissionStateEventLevel(state string) string {
	switch strings.ToUpper(state) {
	case "SUBMIT_RETRY_WAIT", "PENDING", "PENDING_TIMEOUT":
		return "warn"
	default:
		return "info"
	}
}

func eventLevelForStatus(status string) string {
	switch status {
	case "failed":
		return "error"
	case "cancelled", "interrupted", "blocked":
		return "warn"
	default:
		return "info"
	}
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func cloneEventDetails(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	clonedValues := make(map[string]any, len(values))
	for key, value := range values {
		clonedValues[key] = value
	}
	return clonedValues
}

func marshalMetadata(values map[string]any) json.RawMessage {
	if len(values) == 0 {
		return nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	return encoded
}
