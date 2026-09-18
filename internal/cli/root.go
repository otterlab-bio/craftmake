package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	actionadapter "github.com/otterlab-bio/craftmake/internal/adapters/action"
	"github.com/otterlab-bio/craftmake/internal/adapters/otter"
	"github.com/otterlab-bio/craftmake/internal/adapters/standalone"
	"github.com/otterlab-bio/craftmake/internal/backend"
	colabpkg "github.com/otterlab-bio/craftmake/internal/backend/colab"
	"github.com/otterlab-bio/craftmake/internal/backend/local"
	"github.com/otterlab-bio/craftmake/internal/backend/slurm"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/controllerlog"
	"github.com/otterlab-bio/craftmake/internal/engine"
	"github.com/otterlab-bio/craftmake/internal/report"
	runtimeexecutor "github.com/otterlab-bio/craftmake/internal/runtime"
	"github.com/otterlab-bio/craftmake/internal/scheduler"
	"github.com/otterlab-bio/craftmake/internal/spec"
	"github.com/otterlab-bio/craftmake/internal/store"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

var slurmAllocationTimePattern = regexp.MustCompile("^(?:[0-9]+-)?[0-9]{1,2}:[0-5][0-9]:[0-5][0-9]\\z")

type commonOptions struct {
	workflowPath         string
	configPath           string
	projectDir           string
	stateDir             string
	catalogDir           string
	phase                string
	format               string
	resolvedBackend      string
	resolvedRunID        string
	execution            compiler.ExecutionContext
	configKind           standalone.ConfigKind
	standaloneAssertion  bool
	legacyConfig         bool
	referenceBuildConfig bool
	gateMode             bool
}

func (options commonOptions) permitsMutableOverrides() bool {
	if options.gateMode {
		return options.configKind == standalone.ConfigKindLegacy || options.configKind == standalone.ConfigKindStandalone
	}
	return true
}

func executionRunID(resolvedRunID string, phase string, permitsMutableOverrides bool) string {
	if permitsMutableOverrides || resolvedRunID == "" || phase == "" {
		return resolvedRunID
	}
	return resolvedRunID + "--" + phase
}

func defaultMutableRunID() string {
	identifierBytes := make([]byte, 3)
	if _, err := rand.Read(identifierBytes); err != nil {
		return "run-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	return "run-" + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(identifierBytes)
}

func NewRootCommand(buildInfo BuildInfo) *cobra.Command {
	root := &cobra.Command{
		Use:           "craftmake",
		Short:         "Native workflow runner for otter pipelines",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s+%s (%s)", buildInfo.Version, buildInfo.Commit, buildInfo.Date),
	}
	commands := []*cobra.Command{newValidateCommand(), newPlanCommand(), newRunCommand(buildInfo), newActionCommand(buildInfo), newColabCommand(), newStatusCommand(), newCancelCommand(), newReportCommand(), newLogsCommand(), newDoctorCommand(), newResumeCommand(buildInfo), newTaskRunnerCommand()}
	for _, command := range commands {
		if command.Args == nil {
			command.Args = noArguments
		}
		command.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
			return WithExitCode(ExitUsage, err)
		})
	}
	root.AddCommand(commands...)
	return root
}

func noArguments(command *cobra.Command, arguments []string) error {
	if len(arguments) == 0 {
		return nil
	}
	return usageError("command %q does not accept positional arguments", command.CommandPath())
}

func newValidateCommand() *cobra.Command {
	options := commonOptions{format: "text"}
	command := &cobra.Command{Use: "validate", Short: "Validate workflow and configuration", RunE: func(command *cobra.Command, arguments []string) error {
		plan, err := loadPlan(&options)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(protocol.ValidatePayload{
			Workflow:        plan.Workflow,
			Phase:           plan.Phase,
			TaskCount:       len(plan.Tasks),
			SubmissionCount: len(plan.Submissions),
		})
		if err != nil {
			return internalFailureError(err)
		}
		envelope := protocol.NewCommandEnvelope("validate", true, options.resolvedRunID, "", "", payload)
		return writeCommandOutput(command, options.format, envelope, func() error {
			fmt.Fprintf(command.OutOrStdout(), "valid: %s %s (%d tasks, %d submissions)\n", plan.Workflow, plan.Phase, len(plan.Tasks), len(plan.Submissions))
			return nil
		})
	}}
	addPlanFlags(command, &options)
	command.Flags().StringVar(&options.format, "format", "text", "Output format (text/json/jsonl)")
	return command
}

func newPlanCommand() *cobra.Command {
	options := commonOptions{format: "text"}
	command := &cobra.Command{Use: "plan", Short: "Compile and display the task DAG", RunE: func(command *cobra.Command, arguments []string) error {
		plan, err := loadPlan(&options)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(plan)
		if err != nil {
			return internalFailureError(err)
		}
		envelope := protocol.NewCommandEnvelope("plan", true, options.resolvedRunID, "", "", payload)
		return writeCommandOutput(command, options.format, envelope, func() error {
			printPlan(command, plan)
			return nil
		})
	}}
	addPlanFlags(command, &options)
	command.Flags().StringVar(&options.format, "format", "text", "Output format (text/json/jsonl)")
	return command
}

func newRunCommand(buildInfo BuildInfo) *cobra.Command {
	options := commonOptions{format: "text"}
	var backendName string
	var slurmPartition string
	var slurmAccount string
	var slurmQOS string
	var slurmDefaultTime string
	var slurmScratchRoot string
	var workers int
	var maxParallel int
	var maxCores int
	var maxMemory string
	var slurmSubmitAttempts int
	var slurmSubmitBackoff time.Duration
	var slurmSubmitMaximumBackoff time.Duration
	var slurmPendingTimeout time.Duration
	var force bool
	var dryRun bool
	var runID string
	var colabSessionID string
	var colabAuthConfig string
	command := &cobra.Command{Use: "run", Short: "Run a compiled workflow", RunE: func(command *cobra.Command, arguments []string) error {
		plan, err := loadPlan(&options)
		if err != nil {
			return err
		}
		if backendName == "" {
			backendName = options.resolvedBackend
		} else if !options.permitsMutableOverrides() && backendName != options.resolvedBackend {
			return configurationError(fmt.Errorf("--backend cannot override immutable run backend %q", options.resolvedBackend))
		}
		phaseScopedRunID := executionRunID(options.resolvedRunID, plan.Phase, options.permitsMutableOverrides())
		if runID == "" {
			runID = phaseScopedRunID
			if runID == "" && options.permitsMutableOverrides() {
				runID = defaultMutableRunID()
			}
		} else if !options.permitsMutableOverrides() && runID != phaseScopedRunID {
			return configurationError(fmt.Errorf("--run-id cannot override immutable phase execution id %q", phaseScopedRunID))
		}
		if backendName == "slurm" {
			slurmPartition, slurmAccount, slurmQOS, slurmDefaultTime, slurmScratchRoot, err = resolveSlurmExecutionOptions(
				command,
				options,
				slurmPartition,
				slurmAccount,
				slurmQOS,
				slurmDefaultTime,
				slurmScratchRoot,
			)
			if err != nil {
				return err
			}
		}
		if dryRun {
			planData, marshalErr := json.Marshal(plan)
			if marshalErr != nil {
				return internalFailureError(marshalErr)
			}
			payload, marshalErr := json.Marshal(protocol.RunPayload{Backend: backendName, Status: "planned", DryRun: true, Plan: planData})
			if marshalErr != nil {
				return internalFailureError(marshalErr)
			}
			envelope := protocol.NewCommandEnvelope("run", true, runID, "", "", payload)
			return writeCommandOutput(command, options.format, envelope, func() error {
				printPlan(command, plan)
				return nil
			})
		}
		effectiveWorkers, workerErr := resolveEffectiveWorkers(workers, maxParallel)
		if workerErr != nil {
			return workerErr
		}
		effectiveWorkers = capSlurmWorkers(
			backendName,
			effectiveWorkers,
			options.execution.Slurm.MaxJobs,
		)
		if backendName == "colab" && effectiveWorkers > 1 {
			effectiveWorkers = 1
		}
		effectiveMaxCores := effectiveSchedulerMaxCores(backendName, maxCores, command.Flags().Changed("max-cores"))
		var selectedBackend backend.Backend
		switch backendName {
		case "local":
			selectedBackend = local.New()
		case "colab":
			colabBackend, colabErr := buildColabBackend(command.Context(), colabBackendConfig{SessionID: colabSessionID, AuthConfig: colabAuthConfig, ProjectDirectory: options.projectDir})
			if colabErr != nil {
				return configurationError(colabErr)
			}
			selectedBackend = colabBackend
		case "slurm":
			slurmBackend, configureErr := configuredSlurmBackendWithResources(
				slurmPartition,
				slurmAccount,
				slurmQOS,
				slurmDefaultTime,
				slurmScratchRoot,
				slurmSubmitAttempts,
				slurmSubmitBackoff,
				slurmSubmitMaximumBackoff,
				slurmPendingTimeout,
			)
			if configureErr != nil {
				return configureErr
			}
			selectedBackend = slurmBackend
		default:
			return usageError("unsupported backend %q", backendName)
		}
		projectDirectory, stateDirectory, databasePath, err := resolveRuntimePaths(options)
		if err != nil {
			return stateFailureError(err)
		}
		memoryBytes, err := compiler.ParseMemory(maxMemory)
		if err != nil {
			return usageError("invalid --max-memory value %q: %v", maxMemory, err)
		}
		digests, err := calculatePlanDigests(options.configPath, options.workflowPath)
		if err != nil {
			return configurationError(err)
		}
		if runID == "" {
			runID = defaultMutableRunID()
		}
		runResult, runErr := engine.Run(command.Context(), engine.RunRequest{Plan: plan, DatabasePath: databasePath, ProjectDirectory: projectDirectory, StateDirectory: stateDirectory, ConfigPath: options.configPath, ConfigDigest: digests.Config, WorkflowPath: options.workflowPath, WorkflowDigest: digests.Workflow, Backend: selectedBackend, MaxParallel: effectiveWorkers, MaxCores: effectiveMaxCores, MaxMemoryBytes: memoryBytes, Force: force, Version: buildInfo.Version, RunID: runID, LoaderKind: string(options.configKind)})
		if runResult.RunID == "" && runErr != nil {
			return backendFailureError(runErr)
		}
		actualRunID := runResult.RunID
		for _, logErr := range runResult.ControllerLogErrors {
			fmt.Fprintf(command.ErrOrStderr(), "warning: controller log: %v\n", logErr)
		}
		status := "succeeded"
		if runErr != nil {
			status = "failed"
		}
		payload, marshalErr := json.Marshal(protocol.RunPayload{Backend: backendName, Status: status})
		if marshalErr != nil {
			return internalFailureError(marshalErr)
		}
		envelope := protocol.NewCommandEnvelope("run", runErr == nil, actualRunID, databasePath, runResult.ControllerLogPath, payload)
		if outputErr := writeCommandOutput(command, options.format, envelope, func() error {
			fmt.Fprintf(
				command.OutOrStdout(),
				"run_id: %s\nstate: %s\ncontroller_log: %s\n",
				actualRunID,
				databasePath,
				runResult.ControllerLogPath,
			)
			return nil
		}); outputErr != nil {
			return outputErr
		}
		return taskFailureError(runErr)
	}}
	addPlanFlags(command, &options)
	command.Flags().StringVar(&backendName, "backend", "", "Override the resolved execution backend (local/slurm/colab)")
	command.Flags().StringVar(&slurmPartition, "partition", os.Getenv("CRAFTMAKE_SLURM_PARTITION"), "Override the Slurm partition (or set CRAFTMAKE_SLURM_PARTITION)")
	command.Flags().StringVar(&slurmAccount, "account", "", "Slurm account")
	command.Flags().StringVar(&slurmQOS, "qos", "", "Slurm quality of service")
	command.Flags().StringVar(&slurmDefaultTime, "time", "", "Slurm allocation time limit")
	command.Flags().StringVar(&slurmScratchRoot, "scratch-root", "", "Scratch root exported to Slurm workers")
	command.Flags().IntVar(&workers, "workers", 0, "Maximum active submissions; zero uses --max-parallel")
	command.Flags().IntVar(&maxParallel, "max-parallel", runtime.NumCPU(), "Maximum active submissions when --workers is zero")
	command.Flags().IntVar(&maxCores, "max-cores", 0, "Scheduler CPU admission limit; Local defaults to machine CPUs, Slurm defaults to unlimited")
	command.Flags().StringVar(&maxMemory, "max-memory", "0", "Scheduler memory admission limit, 0 means unlimited")
	command.Flags().IntVar(&slurmSubmitAttempts, "slurm-submit-attempts", 8, "Maximum attempts for transient sbatch submission failures")
	command.Flags().DurationVar(&slurmSubmitBackoff, "slurm-submit-backoff", time.Second, "Initial delay after a transient sbatch failure")
	command.Flags().DurationVar(&slurmSubmitMaximumBackoff, "slurm-submit-max-backoff", 30*time.Second, "Maximum delay between transient sbatch retries")
	command.Flags().DurationVar(&slurmPendingTimeout, "slurm-pending-timeout", 0, "Cancel a Slurm job after this continuous pending duration, 0 disables")
	command.Flags().BoolVar(&force, "force", false, "Ignore fingerprint cache")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Compile and display the plan without executing")
	command.Flags().StringVar(&colabSessionID, "colab-session", "", "Named Colab session used to build the backend")
	command.Flags().StringVar(&colabAuthConfig, "colab-auth-config", "~/.config/craftmake/colab-auth.json", "Colab authentication config path")
	command.Flags().BoolVar(&options.gateMode, "gate", false, "Enforce immutable backend, run identity, and Slurm resources")
	command.Flags().StringVar(&runID, "run-id", "", "Override the resolved run identifier")
	command.Flags().StringVar(&options.format, "format", "text", "Output format (text/json/jsonl)")
	return command
}

func newStatusCommand() *cobra.Command {
	var statePath string
	var runID string
	var verbose bool
	var format string
	command := &cobra.Command{Use: "status", Short: "Show workflow run status", RunE: func(command *cobra.Command, arguments []string) error {
		stateStore, err := store.Open(command.Context(), statePath)
		if err != nil {
			return stateFailureError(err)
		}
		defer stateStore.Close()
		if runID == "" || runID == "latest" {
			runID, err = stateStore.LatestRunID(command.Context())
			if err != nil {
				return stateFailureError(err)
			}
		}
		run, counts, err := stateStore.RunSummary(command.Context(), runID)
		if err != nil {
			return stateFailureError(err)
		}
		statuses := make([]string, 0, len(counts))
		for status := range counts {
			statuses = append(statuses, status)
		}
		sort.Strings(statuses)
		statusCounts := make([]protocol.StatusCount, 0, len(statuses))
		for _, status := range statuses {
			statusCounts = append(statusCounts, protocol.StatusCount{Status: status, Count: counts[status]})
		}
		cacheDecisions := make([]protocol.CacheDecisionPayload, 0)
		if verbose {
			rows, queryErr := stateStore.QueryRows(command.Context(), `
				SELECT task_id, status, COALESCE(cache_decision, ''), COALESCE(cache_reason_code, ''), COALESCE(cache_reason_detail, '')
				FROM task_instances
				WHERE run_id = ?
				ORDER BY task_id
			`, runID)
			if queryErr != nil {
				return stateFailureError(queryErr)
			}
			defer rows.Close()
			for rows.Next() {
				var decision protocol.CacheDecisionPayload
				if err := rows.Scan(&decision.TaskID, &decision.Status, &decision.Decision, &decision.ReasonCode, &decision.ReasonDetail); err != nil {
					return stateFailureError(err)
				}
				cacheDecisions = append(cacheDecisions, decision)
			}
			if err := rows.Err(); err != nil {
				return stateFailureError(err)
			}
		}
		payload, err := json.Marshal(protocol.StatusPayload{
			Workflow:       run.Workflow,
			Phase:          run.Phase,
			Backend:        run.Backend,
			Status:         run.Status,
			Counts:         statusCounts,
			CacheDecisions: cacheDecisions,
		})
		if err != nil {
			return internalFailureError(err)
		}
		controllerLogPath := controllerlog.DefaultPath(filepath.Dir(statePath), runID)
		envelope := protocol.NewCommandEnvelope("status", true, runID, statePath, controllerLogPath, payload)
		return writeCommandOutput(command, format, envelope, func() error {
			fmt.Fprintf(command.OutOrStdout(), "run: %s\nworkflow: %s\nphase: %s\nbackend: %s\nstatus: %s\n", run.ID, run.Workflow, run.Phase, run.Backend, run.Status)
			for _, count := range statusCounts {
				fmt.Fprintf(command.OutOrStdout(), "%s: %d\n", count.Status, count.Count)
			}
			if verbose {
				fmt.Fprintln(command.OutOrStdout(), "cache_decisions:")
				for _, decision := range cacheDecisions {
					fmt.Fprintf(command.OutOrStdout(), "%s\tstatus=%s\tdecision=%s\treason=%s\tdetail=%s\n", decision.TaskID, decision.Status, decision.Decision, decision.ReasonCode, decision.ReasonDetail)
				}
			}
			return nil
		})
	}}
	command.Flags().StringVar(&statePath, "state", "workflow/.craftmake/state.sqlite", "State database path")
	command.Flags().StringVar(&runID, "run", "latest", "Run identifier")
	command.Flags().BoolVar(&verbose, "verbose", false, "Show per-task cache decisions and reasons")
	command.Flags().StringVar(&format, "format", "text", "Output format (text/json/jsonl)")
	return command
}

func newCancelCommand() *cobra.Command {
	var statePath string
	var runID string
	var format string
	var cancelColabSession string
	var cancelColabAuthConfig string
	command := &cobra.Command{Use: "cancel", Short: "Cancel a running workflow", RunE: func(command *cobra.Command, arguments []string) error {
		stateStore, err := store.Open(command.Context(), statePath)
		if err != nil {
			return stateFailureError(err)
		}
		defer stateStore.Close()
		if runID == "" || runID == "latest" {
			runID, err = stateStore.LatestRunID(command.Context())
			if err != nil {
				return stateFailureError(err)
			}
		}
		run, _, err := stateStore.RunSummary(command.Context(), runID)
		if err != nil {
			return stateFailureError(err)
		}
		if run.Status != "running" {
			return usageError("run %s is not running (status: %s)", runID, run.Status)
		}
		selectedBackend, err := backendForNameWithColab(command.Context(), run.Backend, colabBackendConfig{SessionID: cancelColabSession, AuthConfig: cancelColabAuthConfig, ProjectDirectory: filepath.Dir(run.ConfigPath)})
		if err != nil {
			return err
		}
		submissions, err := stateStore.RunningSubmissions(command.Context(), runID)
		if err != nil {
			return stateFailureError(err)
		}
		structuredLogger, controllerLogPath := openControllerLogger(command, stateStore, statePath, runID)
		structuredLogger.Log(command.Context(), controllerlog.Event{
			Name:    "run.cancellation_requested",
			RunID:   runID,
			Backend: run.Backend,
			Status:  "running",
			Details: map[string]any{"submission_count": len(submissions)},
		})
		finishedAt := time.Now().UTC()
		if err := stateStore.CancelRun(command.Context(), runID, finishedAt); err != nil {
			structuredLogger.Log(command.Context(), controllerlog.Event{
				Timestamp:            finishedAt,
				Level:                "error",
				Name:                 "run.finished",
				RunID:                runID,
				Backend:              run.Backend,
				Status:               "failed",
				DurationMilliseconds: finishedAt.Sub(run.StartedAt).Milliseconds(),
				Error:                err.Error(),
			})
			_ = structuredLogger.Close()
			reportControllerLogErrors(command, structuredLogger)
			return stateFailureError(err)
		}
		var cancellationErrors []string
		failures := make([]protocol.CancelFailurePayload, 0)
		cancelledCount := 0
		for _, submission := range submissions {
			metadata := map[string]any{}
			if len(submission.RawMetadata) > 0 {
				if err := json.Unmarshal(submission.RawMetadata, &metadata); err != nil {
					message := fmt.Sprintf("submission %s metadata: %v", submission.ID, err)
					cancellationErrors = append(cancellationErrors, message)
					failures = append(failures, protocol.CancelFailurePayload{SubmissionID: submission.ID, Message: message})
					structuredLogger.Log(command.Context(), controllerlog.Event{
						Level:        "error",
						Name:         "submission.cancellation_finished",
						RunID:        runID,
						SubmissionID: submission.ID,
						Backend:      run.Backend,
						BackendJobID: submission.BackendJobID,
						Status:       "failed",
						Error:        err.Error(),
					})
					continue
				}
			}
			cancellationErr := selectedBackend.CancelSubmission(command.Context(), submission.BackendJobID, metadata)
			submissionStatus := "cancelled"
			if cancellationErr != nil {
				submissionStatus = "failed"
				message := fmt.Sprintf("submission %s: %v", submission.ID, cancellationErr)
				cancellationErrors = append(cancellationErrors, message)
				failures = append(failures, protocol.CancelFailurePayload{SubmissionID: submission.ID, Message: message})
			} else {
				cancelledCount++
			}
			structuredLogger.Log(command.Context(), controllerlog.Event{
				Level:        controllerEventLevelForStatus(submissionStatus),
				Name:         "submission.cancellation_finished",
				RunID:        runID,
				SubmissionID: submission.ID,
				Backend:      run.Backend,
				BackendJobID: submission.BackendJobID,
				Status:       submissionStatus,
				Error:        controllerErrorMessage(cancellationErr),
			})
		}
		var cancellationErr error
		if len(cancellationErrors) > 0 {
			cancellationErr = fmt.Errorf("cancel run %s: %s", runID, strings.Join(cancellationErrors, "; "))
		}
		controllerFinishedAt := time.Now().UTC()
		structuredLogger.Log(command.Context(), controllerlog.Event{
			Timestamp:            controllerFinishedAt,
			Level:                "warn",
			Name:                 "run.finished",
			RunID:                runID,
			Backend:              run.Backend,
			Status:               "cancelled",
			DurationMilliseconds: controllerFinishedAt.Sub(run.StartedAt).Milliseconds(),
			Error:                controllerErrorMessage(cancellationErr),
		})
		_ = structuredLogger.Close()
		reportControllerLogErrors(command, structuredLogger)
		payload, marshalErr := json.Marshal(protocol.CancelPayload{
			SubmissionCount: len(submissions),
			CancelledCount:  cancelledCount,
			FailureCount:    len(failures),
			Partial:         cancellationErr != nil,
			Failures:        failures,
		})
		if marshalErr != nil {
			return internalFailureError(marshalErr)
		}
		envelope := protocol.NewCommandEnvelope("cancel", cancellationErr == nil, runID, statePath, controllerLogPath, payload)
		if outputErr := writeCommandOutput(command, format, envelope, func() error {
			if cancellationErr == nil {
				fmt.Fprintf(command.OutOrStdout(), "cancelled: %s\ncontroller_log: %s\n", runID, controllerLogPath)
			}
			return nil
		}); outputErr != nil {
			return outputErr
		}
		if cancellationErr != nil {
			return backendFailureError(cancellationErr)
		}
		return nil
	}}
	command.Flags().StringVar(&statePath, "state", "workflow/.craftmake/state.sqlite", "State database path")
	command.Flags().StringVar(&runID, "run", "latest", "Run identifier")
	command.Flags().StringVar(&format, "format", "text", "Output format (text/json/jsonl)")
	command.Flags().StringVar(&cancelColabSession, "colab-session", "", "Named Colab session used to rebuild the backend")
	command.Flags().StringVar(&cancelColabAuthConfig, "colab-auth-config", "~/.config/craftmake/colab-auth.json", "Colab authentication config path")
	return command
}

func openControllerLogger(
	command *cobra.Command,
	stateStore *store.Store,
	statePath string,
	runID string,
) (*controllerlog.Logger, string) {
	controllerLogPath := controllerlog.DefaultPath(filepath.Dir(statePath), runID)
	structuredLogger, err := controllerlog.Open(controllerLogPath, stateStore)
	if err != nil {
		fmt.Fprintf(command.ErrOrStderr(), "warning: controller log: %v\n", err)
		structuredLogger = controllerlog.New(nil, stateStore)
	}
	return structuredLogger, controllerLogPath
}

func reportControllerLogErrors(command *cobra.Command, structuredLogger *controllerlog.Logger) {
	for _, logErr := range structuredLogger.Errors() {
		fmt.Fprintf(command.ErrOrStderr(), "warning: controller log: %v\n", logErr)
	}
}

func controllerEventLevelForStatus(status string) string {
	switch status {
	case "failed":
		return "error"
	case "cancelled", "interrupted":
		return "warn"
	default:
		return "info"
	}
}

func controllerErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func backendForName(backendName string) (backend.Backend, error) {
	return backendForNameWithPartition(backendName, "")
}

func resolveSlurmExecutionOptions(
	command *cobra.Command,
	options commonOptions,
	partition string,
	account string,
	qos string,
	defaultTime string,
	scratchRoot string,
) (string, string, string, string, string, error) {
	if options.permitsMutableOverrides() {
		resolvedSlurm := options.execution.Slurm
		if partition == "" {
			partition = resolvedSlurm.Partition
		}
		if account == "" {
			account = resolvedSlurm.Account
		}
		if qos == "" {
			qos = resolvedSlurm.QOS
		}
		if defaultTime == "" {
			defaultTime = resolvedSlurm.DefaultTime
		}
		if scratchRoot == "" {
			scratchRoot = resolvedSlurm.ScratchRoot
		}
	}
	if !options.permitsMutableOverrides() {
		for _, flagName := range []string{"partition", "account", "qos", "time", "scratch-root"} {
			flag := command.Flags().Lookup(flagName)
			if flag != nil && flag.Changed {
				return "", "", "", "", "", configurationError(
					fmt.Errorf("--%s cannot override immutable run SLURM resources", flagName),
				)
			}
		}
		resolvedSlurm := options.execution.Slurm
		return resolvedSlurm.Partition,
			resolvedSlurm.Account,
			resolvedSlurm.QOS,
			resolvedSlurm.DefaultTime,
			resolvedSlurm.ScratchRoot,
			nil
	}
	if strings.TrimSpace(defaultTime) != "" && !slurmAllocationTimePattern.MatchString(defaultTime) {
		return "", "", "", "", "", usageError(
			"--time %q must use D-HH:MM:SS or HH:MM:SS format",
			defaultTime,
		)
	}
	if strings.TrimSpace(scratchRoot) != "" && !filepath.IsAbs(scratchRoot) {
		return "", "", "", "", "", usageError("--scratch-root must be an absolute path")
	}
	return strings.TrimSpace(partition),
		strings.TrimSpace(account),
		strings.TrimSpace(qos),
		strings.TrimSpace(defaultTime),
		strings.TrimSpace(scratchRoot),
		nil
}

func capSlurmWorkers(backendName string, requestedWorkers, maximumJobs int) int {
	if backendName != "slurm" || maximumJobs <= 0 || requestedWorkers <= maximumJobs {
		return requestedWorkers
	}
	return maximumJobs
}

func backendForNameWithPartition(backendName, slurmPartition string) (backend.Backend, error) {
	switch backendName {
	case "local":
		return local.New(), nil
	case "slurm":
		return slurm.NewWithPartition(slurmPartition), nil
	default:
		return nil, backendFailureError(fmt.Errorf("unsupported persisted backend %q", backendName))
	}
}

func resolveEffectiveWorkers(workers, maxParallel int) (int, error) {
	if workers < 0 {
		return 0, usageError("--workers must be zero or positive")
	}
	if maxParallel <= 0 {
		return 0, usageError("--max-parallel must be positive")
	}
	if workers > 0 {
		return workers, nil
	}
	return maxParallel, nil
}

func effectiveSchedulerMaxCores(backendName string, maxCores int, maxCoresFlagChanged bool) int {
	if backendName == "local" && !maxCoresFlagChanged {
		return runtime.NumCPU()
	}
	return maxCores
}

func configuredSlurmBackend(
	partition string,
	submitAttempts int,
	submitBackoff time.Duration,
	submitMaximumBackoff time.Duration,
	pendingTimeout time.Duration,
) (*slurm.Backend, error) {
	return configuredSlurmBackendWithResources(
		partition,
		"",
		"",
		"",
		"",
		submitAttempts,
		submitBackoff,
		submitMaximumBackoff,
		pendingTimeout,
	)
}

func configuredSlurmBackendWithResources(
	partition string,
	account string,
	qos string,
	defaultTime string,
	scratchRoot string,
	submitAttempts int,
	submitBackoff time.Duration,
	submitMaximumBackoff time.Duration,
	pendingTimeout time.Duration,
) (*slurm.Backend, error) {
	if submitAttempts <= 0 {
		return nil, usageError("--slurm-submit-attempts must be positive")
	}
	if submitBackoff <= 0 {
		return nil, usageError("--slurm-submit-backoff must be positive")
	}
	if submitMaximumBackoff <= 0 {
		return nil, usageError("--slurm-submit-max-backoff must be positive")
	}
	if submitMaximumBackoff < submitBackoff {
		return nil, usageError("--slurm-submit-max-backoff must be at least --slurm-submit-backoff")
	}
	if pendingTimeout < 0 {
		return nil, usageError("--slurm-pending-timeout must be zero or positive")
	}
	selectedBackend := slurm.NewWithPartition(partition)
	selectedBackend.AccountOverride = strings.TrimSpace(account)
	selectedBackend.QOSOverride = strings.TrimSpace(qos)
	selectedBackend.DefaultTimeOverride = strings.TrimSpace(defaultTime)
	selectedBackend.ScratchRoot = strings.TrimSpace(scratchRoot)
	selectedBackend.SubmitMaxAttempts = submitAttempts
	selectedBackend.SubmitInitialBackoff = submitBackoff
	selectedBackend.SubmitMaximumBackoff = submitMaximumBackoff
	selectedBackend.PendingTimeout = pendingTimeout
	return selectedBackend, nil
}

func newReportCommand() *cobra.Command {
	var statePath string
	var runID string
	var outputDirectory string
	var format string
	var refreshMetrics bool
	command := &cobra.Command{Use: "report", Short: "Export run metrics and timings", RunE: func(command *cobra.Command, arguments []string) error {
		if format == "csv" {
			format = "text"
		}
		stateStore, err := store.Open(command.Context(), statePath)
		if err != nil {
			return stateFailureError(err)
		}
		defer stateStore.Close()
		if runID == "" || runID == "latest" {
			runID, err = stateStore.LatestRunID(command.Context())
			if err != nil {
				return stateFailureError(err)
			}
		}
		var metricsRefresh *protocol.MetricsRefreshPayload
		if refreshMetrics {
			refreshSummary, refreshErr := report.RefreshMetrics(command.Context(), stateStore, runID, slurm.New())
			if refreshErr != nil {
				return stateFailureError(refreshErr)
			}
			for _, refreshFailure := range refreshSummary.Failures {
				fmt.Fprintf(command.ErrOrStderr(), "warning: metrics refresh for attempt %s failed: %v\n", refreshFailure.AttemptID, refreshFailure.Err)
			}
			metricsRefresh = &protocol.MetricsRefreshPayload{
				Candidates:  refreshSummary.Candidates,
				Refreshed:   refreshSummary.Refreshed,
				Unavailable: len(refreshSummary.Failures),
			}
		}
		if outputDirectory == "" {
			outputDirectory = filepath.Join(filepath.Dir(statePath), "runs", runID, "reports")
		}
		if err := report.ExportCSV(command.Context(), stateStore, runID, outputDirectory); err != nil {
			return stateFailureError(err)
		}
		controllerLogPath := controllerlog.DefaultPath(filepath.Dir(statePath), runID)
		if _, err := report.ExportEvidenceBundle(command.Context(), stateStore, runID, outputDirectory, controllerLogPath); err != nil {
			return stateFailureError(err)
		}
		payload, err := json.Marshal(protocol.ReportPayload{OutputDirectory: outputDirectory, ReportFormat: "csv", MetricsRefresh: metricsRefresh})
		if err != nil {
			return internalFailureError(err)
		}
		envelope := protocol.NewCommandEnvelope("report", true, runID, statePath, controllerlog.DefaultPath(filepath.Dir(statePath), runID), payload)
		return writeCommandOutput(command, format, envelope, func() error {
			if metricsRefresh != nil {
				fmt.Fprintf(command.OutOrStdout(), "metrics_refresh_candidates: %d\nmetrics_refreshed: %d\nmetrics_unavailable: %d\n", metricsRefresh.Candidates, metricsRefresh.Refreshed, metricsRefresh.Unavailable)
			}
			fmt.Fprintln(command.OutOrStdout(), outputDirectory)
			return nil
		})
	}}
	command.Flags().StringVar(&statePath, "state", "workflow/.craftmake/state.sqlite", "State database path")
	command.Flags().StringVar(&runID, "run", "latest", "Run identifier")
	command.Flags().StringVar(&outputDirectory, "output", "", "Report output directory")
	command.Flags().StringVar(&format, "format", "text", "Output format (text/json/jsonl); csv remains a text compatibility alias")
	command.Flags().BoolVar(&refreshMetrics, "refresh-metrics", false, "Retry unavailable Slurm accounting metrics before exporting")
	return command
}

func newLogsCommand() *cobra.Command {
	var statePath string
	var runID string
	var failedOnly bool
	var format string
	command := &cobra.Command{Use: "logs", Short: "List task log paths", RunE: func(command *cobra.Command, arguments []string) error {
		stateStore, err := store.Open(command.Context(), statePath)
		if err != nil {
			return stateFailureError(err)
		}
		defer stateStore.Close()
		if runID == "" || runID == "latest" {
			runID, err = stateStore.LatestRunID(command.Context())
			if err != nil {
				return stateFailureError(err)
			}
		}
		controllerLogPath := controllerlog.DefaultPath(filepath.Dir(statePath), runID)
		logs := []protocol.LogPayload{{Kind: "controller", Path: controllerLogPath}}
		query := `SELECT task_id, status, result_path FROM task_attempts WHERE run_id=?`
		if failedOnly {
			query += ` AND status='failed'`
		}
		query += ` ORDER BY task_id, attempt_number`
		rows, err := stateStore.QueryRows(command.Context(), query, runID)
		if err != nil {
			return stateFailureError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var taskID, status, resultPath string
			if err := rows.Scan(&taskID, &status, &resultPath); err != nil {
				return stateFailureError(err)
			}
			logs = append(logs, protocol.LogPayload{Kind: "task", TaskID: taskID, Status: status, Path: filepath.Dir(resultPath)})
		}
		if err := rows.Err(); err != nil {
			return stateFailureError(err)
		}
		payload, err := json.Marshal(protocol.LogsPayload{Logs: logs})
		if err != nil {
			return internalFailureError(err)
		}
		envelope := protocol.NewCommandEnvelope("logs", true, runID, statePath, controllerLogPath, payload)
		return writeCommandOutput(command, format, envelope, func() error {
			fmt.Fprintf(command.OutOrStdout(), "controller\t%s\t%s\n", runID, controllerLogPath)
			for _, log := range logs[1:] {
				fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", log.Status, log.TaskID, log.Path)
			}
			return nil
		})
	}}
	command.Flags().StringVar(&statePath, "state", "workflow/.craftmake/state.sqlite", "State database path")
	command.Flags().StringVar(&runID, "run", "latest", "Run identifier")
	command.Flags().BoolVar(&failedOnly, "failed", false, "Show only failed task logs")
	command.Flags().StringVar(&format, "format", "text", "Output format (text/json/jsonl)")
	return command
}

func newDoctorCommand() *cobra.Command {
	var backendName string
	var colabSessionID string
	var colabAuthConfig string
	command := &cobra.Command{Use: "doctor", Short: "Check runtime dependencies", RunE: func(command *cobra.Command, arguments []string) error {
		switch backendName {
		case "local":
			fmt.Fprintf(command.OutOrStdout(), "local: ok\ngnu_time: %t\nenva_or_conda: optional\n", fileExists("/usr/bin/time"))
			return nil
		case "colab":
			configPath, pathErr := expandUserPath(colabAuthConfig)
			if pathErr != nil {
				return usageError("invalid --colab-auth-config: %v", pathErr)
			}
			if _, loadErr := colabpkg.LoadSessionAuth(configPath, colabSessionID); loadErr != nil {
				return backendFailureError(fmt.Errorf("Colab session %q is not ready: %v", colabSessionID, loadErr))
			}
			fmt.Fprintf(command.OutOrStdout(), "colab: ok\nsession: %s\nauth_config: %s\n", colabSessionID, configPath)
			return nil
		case "slurm":
		}
		missing := []string{}
		for _, executable := range []string{"sinfo", "sbatch", "squeue", "sacct", "scancel", "srun"} {
			if _, err := execLookPath(executable); err != nil {
				missing = append(missing, executable)
			}
		}
		if len(missing) > 0 {
			return backendFailureError(fmt.Errorf("missing Slurm commands: %s", strings.Join(missing, ", ")))
		}
		fmt.Fprintln(command.OutOrStdout(), "slurm: ok")
		return nil
	}}
	command.Flags().StringVar(&backendName, "backend", "local", "Backend to check")
	command.Flags().StringVar(&colabSessionID, "colab-session", "", "Named Colab session to check")
	command.Flags().StringVar(&colabAuthConfig, "colab-auth-config", "~/.config/craftmake/colab-auth.json", "Colab authentication config path")
	return command
}

func newResumeCommand(buildInfo BuildInfo) *cobra.Command {
	var statePath string
	var runID string
	var slurmPartition string
	var workers int
	var maxParallel int
	var maxCores int
	var maxMemory string
	var slurmSubmitAttempts int
	var slurmSubmitBackoff time.Duration
	var slurmSubmitMaximumBackoff time.Duration
	var slurmPendingTimeout time.Duration
	var format string
	var legacyConfig bool
	var gateMode bool
	var resumeColabSession string
	var resumeColabAuthConfig string
	command := &cobra.Command{Use: "resume", Short: "Recover and resume a prior run using its workflow, config, and backend", RunE: func(command *cobra.Command, arguments []string) error {
		stateStore, err := store.Open(command.Context(), statePath)
		if err != nil {
			return stateFailureError(err)
		}
		defer stateStore.Close()
		if runID == "" || runID == "latest" {
			runID, err = stateStore.LatestRunID(command.Context())
			if err != nil {
				return stateFailureError(err)
			}
		}
		run, _, err := stateStore.RunSummary(command.Context(), runID)
		if err != nil {
			return stateFailureError(err)
		}
		digests, err := validatePersistedRunDigests(run)
		if err != nil {
			return configurationError(err)
		}
		projectDirectory := filepath.Dir(run.ConfigPath)
		options := commonOptions{
			workflowPath: run.WorkflowPath,
			configPath:   run.ConfigPath,
			projectDir:   projectDirectory,
			stateDir:     filepath.Dir(statePath),
			legacyConfig: legacyConfig,
			gateMode:     gateMode,
		}
		plan, planErr := loadPlan(&options)
		if planErr != nil {
			return planErr
		}
		if !options.permitsMutableOverrides() && options.resolvedBackend != run.Backend {
			return configurationError(fmt.Errorf("persisted backend %q does not match immutable run backend %q", run.Backend, options.resolvedBackend))
		}
		effectiveWorkers, workerErr := resolveEffectiveWorkers(workers, maxParallel)
		if workerErr != nil {
			return workerErr
		}
		effectiveWorkers = capSlurmWorkers(run.Backend, effectiveWorkers, options.execution.Slurm.MaxJobs)
		effectiveMaxCores := effectiveSchedulerMaxCores(run.Backend, maxCores, command.Flags().Changed("max-cores"))
		var selectedBackend backend.Backend
		switch run.Backend {
		case "local":
			selectedBackend = local.New()
		case "colab":
			colabBackend, colabErr := buildColabBackend(command.Context(), colabBackendConfig{SessionID: resumeColabSession, AuthConfig: resumeColabAuthConfig, ProjectDirectory: projectDirectory})
			if colabErr != nil {
				return configurationError(colabErr)
			}
			selectedBackend = colabBackend
		case "slurm":
			partition, account, qos, defaultTime, scratchRoot, resolveErr := resolveSlurmExecutionOptions(
				command,
				options,
				slurmPartition,
				"",
				"",
				"",
				"",
			)
			if resolveErr != nil {
				return resolveErr
			}
			configuredBackend, configureErr := configuredSlurmBackendWithResources(
				partition,
				account,
				qos,
				defaultTime,
				scratchRoot,
				slurmSubmitAttempts,
				slurmSubmitBackoff,
				slurmSubmitMaximumBackoff,
				slurmPendingTimeout,
			)
			if configureErr != nil {
				return configureErr
			}
			selectedBackend = configuredBackend
		default:
			return backendFailureError(fmt.Errorf("unsupported persisted backend %q", run.Backend))
		}
		var recoveryPayload *protocol.RecoveryPayload
		if run.Status == "running" {
			recoveryLogPath := controllerlog.DefaultPath(filepath.Dir(statePath), runID)
			recoveryLogger, recoveryLogOpenErr := controllerlog.Open(recoveryLogPath, stateStore)
			if recoveryLogOpenErr != nil {
				fmt.Fprintf(command.ErrOrStderr(), "warning: controller log: %v\n", recoveryLogOpenErr)
				recoveryLogger = controllerlog.New(nil, stateStore)
			}
			recoverySummary, recoveryErr := scheduler.ReconcileRun(command.Context(), stateStore, selectedBackend, runID, projectDirectory, recoveryLogger)
			_ = recoveryLogger.Close()
			for _, logErr := range recoveryLogger.Errors() {
				fmt.Fprintf(command.ErrOrStderr(), "warning: controller log: %v\n", logErr)
			}
			if recoveryErr != nil {
				return backendFailureError(fmt.Errorf("recover source run %s: %w", runID, recoveryErr))
			}
			recoveryPayload = &protocol.RecoveryPayload{
				RunID:         runID,
				FinalStatus:   recoverySummary.FinalStatus,
				Succeeded:     recoverySummary.Succeeded,
				Failed:        recoverySummary.Failed,
				Cancelled:     recoverySummary.Cancelled,
				Interrupted:   recoverySummary.Interrupted,
				ControllerLog: recoveryLogPath,
			}
		}

		memoryBytes, err := compiler.ParseMemory(maxMemory)
		if err != nil {
			return usageError("invalid --max-memory value %q: %v", maxMemory, err)
		}
		taskScheduler, err := scheduler.New(plan, stateStore, scheduler.Options{
			ProjectDirectory: projectDirectory,
			StateDirectory:   options.stateDir,
			ConfigPath:       options.configPath,
			ConfigDigest:     digests.Config,
			WorkflowPath:     options.workflowPath,
			WorkflowDigest:   digests.Workflow,
			Backend:          selectedBackend,
			MaxParallel:      effectiveWorkers,
			MaxCores:         effectiveMaxCores,
			MaxMemoryBytes:   memoryBytes,
			Version:          buildInfo.Version,
			ResumedFromRunID: runID,
			LoaderKind:       string(options.configKind),
		})
		if err != nil {
			return backendFailureError(err)
		}
		newRunID, runErr := taskScheduler.Run(command.Context())
		for _, logErr := range taskScheduler.ControllerLogErrors() {
			fmt.Fprintf(command.ErrOrStderr(), "warning: controller log: %v\n", logErr)
		}
		status := "succeeded"
		if runErr != nil {
			status = "failed"
		}
		payload, marshalErr := json.Marshal(protocol.ResumePayload{ResumedFrom: runID, Backend: run.Backend, Status: status, Recovery: recoveryPayload})
		if marshalErr != nil {
			return internalFailureError(marshalErr)
		}
		envelope := protocol.NewCommandEnvelope("resume", runErr == nil, newRunID, statePath, taskScheduler.ControllerLogPath(), payload)
		if outputErr := writeCommandOutput(command, format, envelope, func() error {
			if recoveryPayload != nil {
				fmt.Fprintf(
					command.OutOrStdout(),
					"recovered_run: %s\nrecovered_status: %s\nrecovered_succeeded: %d\nrecovered_failed: %d\nrecovered_cancelled: %d\nrecovered_interrupted: %d\nrecovered_controller_log: %s\n",
					recoveryPayload.RunID,
					recoveryPayload.FinalStatus,
					recoveryPayload.Succeeded,
					recoveryPayload.Failed,
					recoveryPayload.Cancelled,
					recoveryPayload.Interrupted,
					recoveryPayload.ControllerLog,
				)
			}
			fmt.Fprintf(command.OutOrStdout(), "resumed_from: %s\nrun_id: %s\nbackend: %s\ncontroller_log: %s\n", runID, newRunID, run.Backend, taskScheduler.ControllerLogPath())
			return nil
		}); outputErr != nil {
			return outputErr
		}
		return taskFailureError(runErr)
	}}
	command.Flags().StringVar(&statePath, "state", "workflow/.craftmake/state.sqlite", "State database path")
	command.Flags().StringVar(&runID, "run", "latest", "Run identifier")
	command.Flags().StringVar(&slurmPartition, "partition", os.Getenv("CRAFTMAKE_SLURM_PARTITION"), "Override the Slurm partition (or set CRAFTMAKE_SLURM_PARTITION)")
	command.Flags().IntVar(&workers, "workers", 0, "Maximum active submissions; zero uses --max-parallel")
	command.Flags().IntVar(&maxParallel, "max-parallel", runtime.NumCPU(), "Maximum active submissions when --workers is zero")
	command.Flags().IntVar(&maxCores, "max-cores", 0, "Scheduler CPU admission limit; Local defaults to machine CPUs, Slurm defaults to unlimited")
	command.Flags().StringVar(&maxMemory, "max-memory", "0", "Scheduler memory admission limit, 0 means unlimited")
	command.Flags().IntVar(&slurmSubmitAttempts, "slurm-submit-attempts", 8, "Maximum attempts for transient sbatch submission failures")
	command.Flags().DurationVar(&slurmSubmitBackoff, "slurm-submit-backoff", time.Second, "Initial delay after a transient sbatch failure")
	command.Flags().DurationVar(&slurmSubmitMaximumBackoff, "slurm-submit-max-backoff", 30*time.Second, "Maximum delay between transient sbatch retries")
	command.Flags().DurationVar(&slurmPendingTimeout, "slurm-pending-timeout", 0, "Cancel a Slurm job after this continuous pending duration, 0 disables")
	command.Flags().StringVar(&format, "format", "text", "Output format (text/json/jsonl)")
	command.Flags().BoolVar(&gateMode, "gate", false, "Enforce immutable backend and Slurm resources")
	command.Flags().BoolVar(&legacyConfig, "legacy-config", false, "Load the stored configuration through the legacy compatibility adapter")
	_ = command.Flags().MarkHidden("legacy-config")
	command.Flags().StringVar(&resumeColabSession, "colab-session", "", "Named Colab session used to rebuild the backend")
	command.Flags().StringVar(&resumeColabAuthConfig, "colab-auth-config", "~/.config/craftmake/colab-auth.json", "Colab authentication config path")
	return command
}

func newTaskRunnerCommand() *cobra.Command {
	var manifestPath string
	command := &cobra.Command{Use: "__task-runner", Hidden: true, RunE: func(command *cobra.Command, arguments []string) error {
		manifest, err := runtimeexecutor.LoadManifest(manifestPath)
		if err != nil {
			return internalFailureError(err)
		}
		_, err = runtimeexecutor.Run(command.Context(), manifest)
		return taskFailureError(err)
	}}
	command.Flags().StringVar(&manifestPath, "manifest", "", "Task manifest path")
	_ = command.MarkFlagRequired("manifest")
	return command
}

func addPlanFlags(command *cobra.Command, options *commonOptions) {
	command.Flags().StringVarP(&options.workflowPath, "workflow", "w", "", "Workflow YAML path; overrides automatic catalog routing")
	command.Flags().StringVarP(&options.configPath, "config", "c", "", "otter run-v1 YAML path")
	command.Flags().StringVar(&options.phase, "phase", "", "Workflow phase to resolve automatically, for example step2-check")
	command.Flags().StringVar(&options.catalogDir, "catalog", "", "Workflow catalog root; defaults to CRAFTMAKE_WORKFLOW_CATALOG or installed workflows")
	command.Flags().StringVar(&options.projectDir, "project-dir", "", "Project working directory")
	command.Flags().StringVar(&options.stateDir, "state-dir", "", "Craftmake state directory")
	command.Flags().BoolVar(&options.standaloneAssertion, "standalone", false, "Require a craftmake.standalone/v1 configuration")
	_ = command.Flags().MarkHidden("standalone")
	command.Flags().BoolVar(&options.legacyConfig, "legacy-config", false, "Assert legacy Otter configuration routing")
	_ = command.Flags().MarkHidden("legacy-config")
	command.Flags().BoolVar(&options.referenceBuildConfig, "reference-build-config", false, "Load an immutable Gate 6 reference-build configuration")
	_ = command.MarkFlagRequired("config")
}

func loadPlan(options *commonOptions) (*compiler.Plan, error) {
	if options.configPath == "" && options.workflowPath != "" {
		kind, detectErr := standalone.DetectConfigKind(options.workflowPath)
		if detectErr == nil && kind == standalone.ConfigKindAction {
			projectDir := options.projectDir
			if projectDir == "" {
				projectDir = filepath.Dir(filepath.Dir(options.workflowPath))
			}
			stateDir := options.stateDir
			if stateDir == "" {
				stateDir = filepath.Join(projectDir, ".craftmake", "state")
			}
			loaded, loadErr := actionadapter.Load(options.workflowPath, nil, projectDir, stateDir)
			if loadErr != nil {
				return nil, configurationError(loadErr)
			}
			plan, compileErr := compiler.Compile(loaded.Workflow, loaded.Context)
			if compileErr != nil {
				return nil, compilationError(compileErr)
			}
			options.projectDir, options.stateDir, options.configKind, options.resolvedBackend, options.execution = projectDir, stateDir, kind, loaded.Context.Workflow.Backend, loaded.Context.Execution
			return plan, nil
		}
		return nil, usageError("--config is required")
	}
	absoluteConfigPath, err := filepath.Abs(options.configPath)
	if err != nil {
		return nil, configurationError(fmt.Errorf("resolve config path: %w", err))
	}
	options.configPath = absoluteConfigPath
	if options.legacyConfig && options.referenceBuildConfig {
		return nil, usageError("--legacy-config and --reference-build-config cannot be combined")
	}
	if options.legacyConfig {
		if _, statErr := os.Stat(options.configPath); statErr != nil {
			_, loadErr := otter.LoadLegacy(options.configPath)
			return nil, configurationError(loadErr)
		}
	}
	if options.standaloneAssertion && (options.legacyConfig || options.referenceBuildConfig) {
		return nil, usageError("--standalone cannot be combined with --legacy-config or --reference-build-config")
	}

	configKind, detectErr := standalone.DetectConfigKind(options.configPath)
	if detectErr != nil {
		if !options.referenceBuildConfig {
			return nil, configurationError(detectErr)
		}
		configKind = "reference-build"
	}
	if options.legacyConfig && configKind != standalone.ConfigKindLegacy {
		return nil, configurationError(fmt.Errorf("--legacy-config requires a complete legacy Otter configuration, detected %q", configKind))
	}
	if options.standaloneAssertion && configKind != standalone.ConfigKindStandalone {
		return nil, configurationError(fmt.Errorf("--standalone requires schema_version %q, detected %q", standalone.SchemaVersion, configKind))
	}
	if options.referenceBuildConfig && configKind != "reference-build" {
		return nil, configurationError(fmt.Errorf("--reference-build-config is only valid for the dedicated immutable reference-build configuration"))
	}

	var context *compiler.Context
	switch configKind {
	case standalone.ConfigKindOtterRun:
		context, err = otter.Load(options.configPath)
	case standalone.ConfigKindLegacy:
		context, err = otter.LoadLegacy(options.configPath)
	case standalone.ConfigKindStandalone:
		context, err = standalone.Load(options.configPath)
	case "reference-build":
		context, err = otter.LoadReferenceBuild(options.configPath)
	default:
		return nil, configurationError(fmt.Errorf("unsupported configuration kind %q", configKind))
	}
	if err != nil {
		return nil, configurationError(err)
	}
	options.configKind = configKind
	options.resolvedBackend = context.Workflow.Backend
	options.execution = context.Execution
	if options.configKind == standalone.ConfigKindLegacy {
		options.resolvedRunID = ""
		if options.resolvedBackend == "" {
			options.resolvedBackend = "local"
		}
	} else {
		options.resolvedRunID = context.Workflow.JobID
	}
	if options.projectDir == "" {
		options.projectDir = context.Paths["project"]
	}
	if options.stateDir == "" {
		options.stateDir = context.Paths["state"]
	}

	if options.workflowPath == "" {
		if options.phase == "" {
			return nil, usageError("--phase is required when --workflow is not provided")
		}
		workflowPath, resolveErr := resolveCatalogWorkflow(options.catalogDir, context.Workflow.WorkflowName, options.phase)
		if resolveErr != nil {
			return nil, configurationError(resolveErr)
		}
		options.workflowPath = workflowPath
	} else {
		absoluteWorkflowPath, resolveErr := filepath.Abs(options.workflowPath)
		if resolveErr != nil {
			return nil, configurationError(fmt.Errorf("resolve workflow path: %w", resolveErr))
		}
		options.workflowPath = absoluteWorkflowPath
	}

	workflow, err := spec.Load(options.workflowPath)
	if err != nil {
		return nil, configurationError(err)
	}
	if options.configKind != standalone.ConfigKindStandalone && !strings.EqualFold(workflow.On.Otter.Workflow, context.Workflow.WorkflowName) {
		return nil, configurationError(fmt.Errorf("workflow %q targets %s, but config resolves to %s", options.workflowPath, workflow.On.Otter.Workflow, context.Workflow.WorkflowName))
	}
	if options.phase != "" && workflow.On.Otter.Phase != options.phase {
		return nil, configurationError(fmt.Errorf("workflow %q declares phase %q, not requested phase %q", options.workflowPath, workflow.On.Otter.Phase, options.phase))
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		return nil, compilationError(err)
	}
	return plan, nil
}

func resolveCatalogWorkflow(configuredCatalog, workflowName, phase string) (string, error) {
	if !isCatalogComponent(workflowName) {
		return "", configurationError(fmt.Errorf("config resolved to invalid workflow name %q", workflowName))
	}
	if !isCatalogComponent(phase) {
		return "", usageError("invalid workflow phase %q", phase)
	}

	catalogCandidates := workflowCatalogCandidates(configuredCatalog)
	searchedPaths := make([]string, 0, len(catalogCandidates))
	for _, catalogDirectory := range catalogCandidates {
		workflowPath := filepath.Join(catalogDirectory, workflowName, phase+".yaml")
		searchedPaths = append(searchedPaths, workflowPath)
		fileInfo, statErr := os.Stat(workflowPath)
		if statErr == nil && !fileInfo.IsDir() {
			absoluteWorkflowPath, absoluteErr := filepath.Abs(workflowPath)
			if absoluteErr != nil {
				return "", fmt.Errorf("resolve discovered workflow path: %w", absoluteErr)
			}
			return absoluteWorkflowPath, nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect workflow %q: %w", workflowPath, statErr)
		}
	}
	return "", fmt.Errorf("workflow catalog entry %s/%s.yaml was not found; searched: %s", workflowName, phase, strings.Join(searchedPaths, ", "))
}

func workflowCatalogCandidates(configuredCatalog string) []string {
	rawCandidates := []string{}
	if configuredCatalog != "" {
		rawCandidates = append(rawCandidates, configuredCatalog)
	} else if environmentCatalog := os.Getenv("CRAFTMAKE_WORKFLOW_CATALOG"); environmentCatalog != "" {
		rawCandidates = append(rawCandidates, environmentCatalog)
	} else {
		if executablePath, err := os.Executable(); err == nil {
			executableDirectory := filepath.Dir(executablePath)
			rawCandidates = append(rawCandidates,
				filepath.Join(executableDirectory, "workflows"),
				filepath.Join(executableDirectory, "..", "share", "craftmake", "workflows"),
			)
		}
		if workingDirectory, err := os.Getwd(); err == nil {
			rawCandidates = append(rawCandidates, filepath.Join(workingDirectory, "workflows"))
		}
		if _, sourcePath, _, ok := runtime.Caller(0); ok {
			rawCandidates = append(rawCandidates, filepath.Join(filepath.Dir(sourcePath), "..", "..", "workflows"))
		}
	}

	uniqueCandidates := make([]string, 0, len(rawCandidates))
	seenCandidates := make(map[string]bool)
	for _, rawCandidate := range rawCandidates {
		absoluteCandidate, err := filepath.Abs(rawCandidate)
		if err != nil {
			continue
		}
		cleanCandidate := filepath.Clean(absoluteCandidate)
		if !seenCandidates[cleanCandidate] {
			seenCandidates[cleanCandidate] = true
			uniqueCandidates = append(uniqueCandidates, cleanCandidate)
		}
	}
	return uniqueCandidates
}

func isCatalogComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	return !strings.ContainsAny(value, `/\\`)
}

func resolveRuntimePaths(options commonOptions) (string, string, string, error) {
	projectDirectory := options.projectDir
	if projectDirectory == "" {
		projectDirectory = filepath.Dir(options.configPath)
	}
	absoluteProject, err := filepath.Abs(projectDirectory)
	if err != nil {
		return "", "", "", err
	}
	stateDirectory := options.stateDir
	if stateDirectory == "" {
		stateDirectory = filepath.Join(absoluteProject, "workflow", ".craftmake")
	}
	absoluteState, err := filepath.Abs(stateDirectory)
	if err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(absoluteState, 0o755); err != nil {
		return "", "", "", err
	}
	return absoluteProject, absoluteState, filepath.Join(absoluteState, "state.sqlite"), nil
}

func printPlan(command *cobra.Command, plan *compiler.Plan) {
	fmt.Fprintf(command.OutOrStdout(), "Workflow: %s\nPhase: %s\nTasks: %d\nSubmissions: %d\n\n", plan.Workflow, plan.Phase, len(plan.Tasks), len(plan.Submissions))
	for orderIndex, taskID := range plan.Order {
		task := plan.TaskByID[taskID]
		accel := task.Accelerator
		if accel == "" {
			accel = "-"
		}
		fmt.Fprintf(command.OutOrStdout(), "%03d  %-8s  accel=%-4s  cores=%d  memory=%d  %s\n", orderIndex+1, task.Scope, accel, task.Resources.Cores, task.Resources.MemoryByte, task.ID)
		if len(task.Dependencies) > 0 {
			fmt.Fprintf(command.OutOrStdout(), "     needs: %s\n", strings.Join(task.Dependencies, ", "))
		}
	}
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
