package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	actionadapter "github.com/otterlab-bio/craftmake/internal/adapters/action"
	"github.com/otterlab-bio/craftmake/internal/adapters/standalone"
	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/backend/local"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/engine"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
	"github.com/spf13/cobra"
)

func loadActionPlan(projectDir, name string, overrides map[string]string) (*compiler.Plan, commonOptions, error) {
	path, err := actionadapter.Resolve(projectDir, name)
	if err != nil {
		return nil, commonOptions{}, err
	}
	loaded, err := actionadapter.Load(path, overrides, projectDir, filepath.Join(projectDir, ".craftmake", "state"))
	if err != nil {
		return nil, commonOptions{}, err
	}
	plan, err := compiler.Compile(loaded.Workflow, loaded.Context)
	if err != nil {
		return nil, commonOptions{}, fmt.Errorf("compile action %q: %w", name, err)
	}
	options := commonOptions{workflowPath: path, projectDir: projectDir, stateDir: filepath.Join(projectDir, ".craftmake", "state"), resolvedBackend: loaded.Context.Workflow.Backend, configKind: standalone.ConfigKindAction}
	return plan, options, nil
}

func newActionCommand(buildInfo BuildInfo) *cobra.Command {
	parent := &cobra.Command{Use: "action", Short: "Discover and run .craftmake actions"}
	parent.AddCommand(newActionListCommand(), newActionPlanCommand(), newActionRunCommand(buildInfo))
	return parent
}

func newActionListCommand() *cobra.Command {
	var dir string
	command := &cobra.Command{Use: "list", Short: "List actions", Args: noArguments, RunE: func(command *cobra.Command, _ []string) error {
		if dir == "" {
			dir = "."
		}
		entries, err := actionadapter.Discover(dir)
		if err != nil {
			return configurationError(err)
		}
		for _, entry := range entries {
			fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", entry.Name, entry.Kind, filepath.Base(entry.Path))
		}
		return nil
	}}
	command.Flags().StringVar(&dir, "dir", ".", "Project directory")
	return command
}

func newActionPlanCommand() *cobra.Command {
	var dir, config string
	var inputValues, argValues []string
	command := &cobra.Command{Use: "plan NAME", Short: "Compile an action plan", Args: exactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		overrideValues := append(append([]string{}, inputValues...), argValues...)
		overrides, err := parseActionInputs(overrideValues)
		if err != nil {
			return usageError("%s", err.Error())
		}
		if config != "" {
			options := commonOptions{workflowPath: filepath.Join(dir, ".craftmake", args[0]+".yaml"), configPath: config, projectDir: dir}
			plan, err := loadPlan(&options)
			if err != nil {
				return err
			}
			printPlan(command, plan)
			return nil
		}
		plan, _, err := loadActionPlan(dir, args[0], overrides)
		if err != nil {
			return configurationError(err)
		}
		printPlan(command, plan)
		return nil
	}}
	command.Flags().StringVar(&dir, "dir", ".", "Project directory")
	command.Flags().StringVar(&config, "config", "", "Existing workflow configuration")
	command.Flags().StringArrayVar(&inputValues, "input", nil, "Deprecated alias for --arg KEY=VALUE")
	command.Flags().StringArrayVar(&argValues, "arg", nil, "Canonical command-line action argument KEY=VALUE")
	return command
}

func parseActionInputs(values []string) (map[string]string, error) {
	result := map[string]string{}
	for _, value := range values {
		key, raw, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("input must use KEY=VALUE: %q", value)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("input %q was provided more than once", key)
		}
		result[key] = raw
	}
	return result, nil
}

func exactArgs(count int) cobra.PositionalArgs {
	return func(command *cobra.Command, args []string) error {
		if len(args) != count {
			return usageError("command %q requires %d argument(s)", command.CommandPath(), count)
		}
		return nil
	}
}

func newActionRunCommand(buildInfo BuildInfo) *cobra.Command {
	var dir, config, backendName, stateDir, runID, format string
	var colabSessionID, colabAuthConfig string
	var inputValues, argValues []string
	var workers, maxParallel, maxCores int
	var maxMemory string
	var force, dryRun bool
	command := &cobra.Command{Use: "run NAME", Short: "Run an action", Args: exactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		overrideValues := append(append([]string{}, inputValues...), argValues...)
		overrides, err := parseActionInputs(overrideValues)
		if err != nil {
			return usageError("%s", err.Error())
		}
		var plan *compiler.Plan
		var options commonOptions
		if config != "" {
			workflowPath, resolveErr := actionadapter.Resolve(dir, args[0])
			if resolveErr != nil {
				return configurationError(resolveErr)
			}
			options = commonOptions{workflowPath: workflowPath, configPath: config, projectDir: dir, stateDir: stateDir}
			plan, err = loadPlan(&options)
		} else {
			plan, options, err = loadActionPlan(dir, args[0], overrides)
			if stateDir != "" {
				options.stateDir = stateDir
			}
		}
		if err != nil {
			return configurationError(err)
		}
		options.format = format
		if backendName == "" {
			backendName = options.resolvedBackend
		}
		if backendName == "" {
			backendName = "local"
		}
		if backendName != "local" && backendName != "colab" {
			return usageError("action backend %q is not implemented yet; local and colab are available", backendName)
		}
		if dryRun {
			printPlan(command, plan)
			return nil
		}
		if workers == 0 {
			workers = maxParallel
		}
		if workers <= 0 {
			workers = runtime.NumCPU()
		}
		if backendName == "colab" && workers > 1 {
			workers = 1
		}
		options.resolvedBackend = backendName
		projectDirectory, stateDirectory, databasePath, err := resolveRuntimePaths(options)
		if err != nil {
			return stateFailureError(err)
		}
		digests, err := calculatePlanDigests(options.configPath, options.workflowPath)
		if err != nil {
			return configurationError(err)
		}
		memoryBytes, err := compiler.ParseMemory(maxMemory)
		if err != nil {
			return usageError("invalid --max-memory value %q: %v", maxMemory, err)
		}
		var selectedBackend backend.Backend
		if backendName == "colab" {
			colabBackend, colabErr := buildColabBackend(command.Context(), colabBackendConfig{SessionID: colabSessionID, AuthConfig: colabAuthConfig, ProjectDirectory: options.projectDir})
			if colabErr != nil {
				return configurationError(colabErr)
			}
			selectedBackend = colabBackend
		} else {
			selectedBackend = local.New()
		}
		if runID == "" {
			runID = defaultMutableRunID()
		}
		runResult, runErr := engine.Run(command.Context(), engine.RunRequest{Plan: plan, DatabasePath: databasePath, ProjectDirectory: projectDirectory, StateDirectory: stateDirectory, ConfigPath: options.configPath, ConfigDigest: digests.Config, WorkflowPath: options.workflowPath, WorkflowDigest: digests.Workflow, Backend: selectedBackend, MaxParallel: workers, MaxCores: effectiveSchedulerMaxCores(backendName, maxCores, command.Flags().Changed("max-cores")), MaxMemoryBytes: memoryBytes, Force: force, Version: buildInfo.Version, RunID: runID, LoaderKind: string(options.configKind)})
		if runResult.RunID == "" && runErr != nil {
			return backendFailureError(runErr)
		}
		actualRunID := runResult.RunID
		payload, marshalErr := json.Marshal(protocol.RunPayload{Backend: backendName, Status: map[bool]string{true: "succeeded", false: "failed"}[runErr == nil]})
		if marshalErr != nil {
			return internalFailureError(marshalErr)
		}
		envelope := protocol.NewCommandEnvelope("action run", runErr == nil, actualRunID, databasePath, runResult.ControllerLogPath, payload)
		if outputErr := writeCommandOutput(command, options.format, envelope, func() error {
			fmt.Fprintf(command.OutOrStdout(), "run_id: %s\nstate: %s\ncontroller_log: %s\n", actualRunID, databasePath, runResult.ControllerLogPath)
			return nil
		}); outputErr != nil {
			return outputErr
		}
		return taskFailureError(runErr)
	}}
	command.Flags().StringVar(&dir, "dir", ".", "Project directory")
	command.Flags().StringVar(&config, "config", "", "Existing workflow configuration")
	command.Flags().StringArrayVar(&inputValues, "input", nil, "Deprecated alias for --arg KEY=VALUE")
	command.Flags().StringArrayVar(&argValues, "arg", nil, "Canonical command-line action argument KEY=VALUE")
	command.Flags().StringVar(&backendName, "backend", "", "Override the action backend (local/colab)")
	command.Flags().StringVar(&colabSessionID, "colab-session", "", "Named Colab session used to build the backend")
	command.Flags().StringVar(&colabAuthConfig, "colab-auth-config", "~/.config/craftmake/colab-auth.json", "Colab authentication config path")
	command.Flags().StringVar(&stateDir, "state-dir", "", "Craftmake state directory")
	command.Flags().StringVar(&runID, "run-id", "", "Run identifier")
	command.Flags().IntVar(&workers, "workers", 0, "Maximum active submissions")
	command.Flags().IntVar(&maxParallel, "max-parallel", runtime.NumCPU(), "Maximum active submissions when workers is zero")
	command.Flags().IntVar(&maxCores, "max-cores", 0, "Scheduler CPU admission limit")
	command.Flags().StringVar(&maxMemory, "max-memory", "0", "Scheduler memory admission limit, 0 means unlimited")
	command.Flags().BoolVar(&force, "force", false, "Ignore fingerprint cache")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Compile and display without executing")
	command.Flags().StringVar(&format, "format", "text", "Output format (text/json/jsonl)")
	return command
}
