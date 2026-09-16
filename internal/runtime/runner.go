package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/otterlab-bio/craftmake/internal/processgroup"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func LoadManifest(path string) (*protocol.TaskManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read task manifest: %w", err)
	}
	manifest, err := protocol.DecodeTaskManifest(data)
	if err != nil {
		return nil, fmt.Errorf("parse task manifest: %w", err)
	}
	return manifest, nil
}

func Run(ctx context.Context, manifest *protocol.TaskManifest) (*protocol.TaskResult, error) {
	startedAt := time.Now().UTC()
	result := &protocol.TaskResult{ProtocolVersion: protocol.Version, RunID: manifest.RunID, TaskID: manifest.TaskID, Attempt: manifest.Attempt, Status: "running", StartedAt: startedAt}
	if err := prepareDirectories(manifest); err != nil {
		result.Status, result.Error, result.FinishedAt, result.ExitCode = "failed", err.Error(), time.Now().UTC(), 1
		result.Incident = protocol.ClassifyTaskIncident(*result, "")
		_ = writeResultAtomic(manifest.ResultPath, result)
		return result, err
	}

	for stepIndex := range manifest.Steps {
		step := manifest.Steps[stepIndex]
		stepResult := runStep(ctx, manifest, step)
		result.Steps = append(result.Steps, stepResult)
		if stepResult.ExitCode != 0 {
			result.Status = "failed"
			result.ExitCode = stepResult.ExitCode
			result.Error = stepResult.Error
			break
		}
	}
	if result.Status != "failed" {
		missingOutputs := validateOutputs(manifest.Outputs)
		if len(missingOutputs) > 0 {
			result.Status = "failed"
			result.ExitCode = 1
			result.Error = "declared outputs are missing"
			result.MissingOutputs = missingOutputs
		} else {
			result.Status = "succeeded"
		}
	}
	if result.Status == "succeeded" && manifest.CompressSuccessLogs {
		compressSuccessfulStepLogs(result)
	}
	result.FinishedAt = time.Now().UTC()
	if result.Status != "succeeded" {
		result.Incident = protocol.ClassifyTaskIncident(*result, "")
	}
	if err := writeResultAtomic(manifest.ResultPath, result); err != nil {
		return result, err
	}
	if result.Status != "succeeded" {
		return result, fmt.Errorf("task %s failed: %s", manifest.TaskID, result.Error)
	}
	return result, nil
}

func runStep(ctx context.Context, manifest *protocol.TaskManifest, step protocol.StepManifest) protocol.StepResult {
	startedAt := time.Now().UTC()
	stepDirectory := filepath.Join(manifest.RuntimeDirectory, "steps", fmt.Sprintf("%02d-%s", step.Index, safeName(step.Name)))
	_ = os.MkdirAll(stepDirectory, 0o755)
	stdoutPath := step.StdoutPath
	stderrPath := step.StderrPath
	if stdoutPath == "" {
		stdoutPath = filepath.Join(stepDirectory, "stdout.log")
	}
	if stderrPath == "" {
		stderrPath = filepath.Join(stepDirectory, "stderr.log")
	}
	stdout, stdoutErr := os.Create(stdoutPath)
	if stdoutErr != nil {
		return failedStep(step, startedAt, stdoutPath, stderrPath, stdoutErr)
	}
	defer stdout.Close()
	stderr, stderrErr := os.Create(stderrPath)
	if stderrErr != nil {
		return failedStep(step, startedAt, stdoutPath, stderrPath, stderrErr)
	}
	defer stderr.Close()

	scriptPath := filepath.Join(stepDirectory, "step.sh")
	script := "set -euo pipefail\n" + step.Command + "\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return failedStep(step, startedAt, stdoutPath, stderrPath, err)
	}
	command, err := buildEnvironmentCommand(step.Environment, choose(step.Shell, "bash"), scriptPath)
	if err != nil {
		return failedStep(step, startedAt, stdoutPath, stderrPath, err)
	}
	command.Dir = manifest.WorkDirectory
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = buildStepEnvironment(os.Environ(), step.Environment, map[string]string{
		"CRAFTMAKE_TEMP": manifest.TempDirectory,
		"CRAFTMAKE_WORK": manifest.WorkDirectory,
	}, step.Env)
	processgroup.Configure(command)
	if err := command.Start(); err != nil {
		return failedStep(step, startedAt, stdoutPath, stderrPath, err)
	}
	runErr := processgroup.Wait(ctx, command)
	exitCode := 0
	errorMessage := ""
	if runErr != nil {
		exitCode = 1
		errorMessage = runErr.Error()
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	return protocol.StepResult{Index: step.Index, Name: step.Name, Environment: step.Environment, StartedAt: startedAt, FinishedAt: time.Now().UTC(), ExitCode: exitCode, StdoutPath: stdoutPath, StderrPath: stderrPath, Error: errorMessage}
}

func buildEnvironmentCommand(environment, shell, scriptPath string) (*exec.Cmd, error) {
	if environment == "" {
		return exec.Command(shell, scriptPath), nil
	}
	if _, err := exec.LookPath("enva"); err == nil {
		environmentPrefix, prefixErr := configuredEnvironmentPrefix(environment)
		if prefixErr != nil {
			return nil, prefixErr
		}
		if environmentPrefix != "" {
			runArguments, runArgumentsErr := buildExplicitPrefixRunArguments(environmentPrefix, shell, scriptPath)
			if runArgumentsErr != nil {
				return nil, runArgumentsErr
			}
			return exec.Command("enva", runArguments...), nil
		}
		return exec.Command("enva", "--quiet", "run", environment, "--", shell, scriptPath), nil
	}
	if _, err := exec.LookPath("conda"); err == nil {
		return exec.Command("conda", "run", "--no-capture-output", "-n", environment, shell, scriptPath), nil
	}
	return nil, fmt.Errorf("step requires environment %q, but neither enva nor conda is available", environment)
}

func configuredEnvironmentPrefix(environment string) (string, error) {
	const prefixVariable = "CRAFTMAKE_ENV_PREFIX"

	prefix := strings.TrimSpace(os.Getenv(prefixVariable))
	if prefix == "" {
		return "", nil
	}
	if !filepath.IsAbs(prefix) {
		return "", fmt.Errorf("%s must be an absolute environment prefix, got %q", prefixVariable, prefix)
	}
	cleanPrefix := filepath.Clean(prefix)
	if filepath.Base(cleanPrefix) != environment {
		return "", fmt.Errorf(
			"%s prefix %q does not match workflow environment %q",
			prefixVariable,
			cleanPrefix,
			environment,
		)
	}
	fileInfo, err := os.Stat(cleanPrefix)
	if err != nil {
		return "", fmt.Errorf("inspect %s prefix %q: %w", prefixVariable, cleanPrefix, err)
	}
	if !fileInfo.IsDir() {
		return "", fmt.Errorf("%s prefix %q is not a directory", prefixVariable, cleanPrefix)
	}
	return cleanPrefix, nil
}

func buildExplicitPrefixRunArguments(environmentPrefix, shell, scriptPath string) ([]string, error) {
	const releaseRootVariable = "OTTER_GATE6_RELEASE_ROOT"

	environmentPath := filepath.Join(environmentPrefix, "bin")
	pathEntries := []string{environmentPath}
	additionalEnvironment := []string{"CRAFTMAKE_ENV_PREFIX=" + environmentPrefix}

	releaseRoot := strings.TrimSpace(os.Getenv(releaseRootVariable))
	if releaseRoot != "" {
		if !filepath.IsAbs(releaseRoot) {
			return nil, fmt.Errorf("%s must be an absolute release root, got %q", releaseRootVariable, releaseRoot)
		}
		releaseBinPath := filepath.Join(filepath.Clean(releaseRoot), "bin")
		releaseBinInfo, err := os.Stat(releaseBinPath)
		if err != nil {
			return nil, fmt.Errorf("inspect %s bin directory %q: %w", releaseRootVariable, releaseBinPath, err)
		}
		if !releaseBinInfo.IsDir() {
			return nil, fmt.Errorf("%s bin path %q is not a directory", releaseRootVariable, releaseBinPath)
		}
		pathEntries = append(pathEntries, releaseBinPath)
		additionalEnvironment = append(additionalEnvironment, releaseRootVariable+"="+filepath.Clean(releaseRoot))
	}
	pathEntries = append(pathEntries, os.Getenv("PATH"))
	additionalEnvironment = append(additionalEnvironment, "PATH="+strings.Join(pathEntries, string(os.PathListSeparator)))

	runArguments := []string{"--quiet", "run", "--prefix", environmentPrefix}
	for _, environmentEntry := range additionalEnvironment {
		runArguments = append(runArguments, "-E", environmentEntry)
	}
	runArguments = append(runArguments, "--", shell, scriptPath)
	return runArguments, nil
}

func buildStepEnvironment(inheritedEnvironment []string, managedEnvironment string, runtimeVariables, stepVariables map[string]string) []string {
	environmentVariables := make(map[string]string, len(inheritedEnvironment)+len(runtimeVariables)+len(stepVariables))
	for _, environmentEntry := range inheritedEnvironment {
		variableName, variableValue, found := strings.Cut(environmentEntry, "=")
		if !found {
			continue
		}
		if managedEnvironment != "" && isManagedEnvironmentContaminant(variableName) {
			continue
		}
		environmentVariables[variableName] = variableValue
	}
	for variableName, variableValue := range runtimeVariables {
		environmentVariables[variableName] = variableValue
	}
	for variableName, variableValue := range stepVariables {
		environmentVariables[variableName] = variableValue
	}

	variableNames := make([]string, 0, len(environmentVariables))
	for variableName := range environmentVariables {
		variableNames = append(variableNames, variableName)
	}
	sort.Strings(variableNames)

	commandEnvironment := make([]string, 0, len(variableNames))
	for _, variableName := range variableNames {
		commandEnvironment = append(commandEnvironment, variableName+"="+environmentVariables[variableName])
	}
	return commandEnvironment
}

func isManagedEnvironmentContaminant(variableName string) bool {
	switch variableName {
	case "CLASSPATH", "JAVA_HOME", "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "JRE_HOME", "LD_LIBRARY_PATH", "_JAVA_OPTIONS":
		return true
	default:
		return false
	}
}

func prepareDirectories(manifest *protocol.TaskManifest) error {
	for _, directory := range []string{manifest.WorkDirectory, manifest.TempDirectory, manifest.RuntimeDirectory, filepath.Dir(manifest.ResultPath)} {
		if directory == "" {
			continue
		}
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create runtime directory %q: %w", directory, err)
		}
	}
	return nil
}

func validateOutputs(outputs map[string]string) []string {
	var missing []string
	for name, path := range outputs {
		if _, err := os.Stat(path); err != nil {
			missing = append(missing, name+"="+path)
		}
	}
	return missing
}

func compressSuccessfulStepLogs(result *protocol.TaskResult) {
	for stepIndex := range result.Steps {
		step := &result.Steps[stepIndex]
		compressedStdoutPath, stdoutErr := compressLogIfPresent(step.StdoutPath)
		if stdoutErr != nil {
			result.ObservabilityErrors = append(result.ObservabilityErrors, stdoutErr.Error())
		} else if compressedStdoutPath != "" {
			step.StdoutPath = compressedStdoutPath
		}
		compressedStderrPath, stderrErr := compressLogIfPresent(step.StderrPath)
		if stderrErr != nil {
			result.ObservabilityErrors = append(result.ObservabilityErrors, stderrErr.Error())
		} else if compressedStderrPath != "" {
			step.StderrPath = compressedStderrPath
		}
	}
}

func compressLogIfPresent(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect log %q before compression: %w", path, err)
	}
	compressedPath, err := CompressLog(path)
	if err != nil {
		return "", fmt.Errorf("compress log %q: %w", path, err)
	}
	return compressedPath, nil
}

func writeResultAtomic(path string, result *protocol.TaskResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task result: %w", err)
	}
	temporaryPath := path + ".tmp"
	if err := os.WriteFile(temporaryPath, data, 0o644); err != nil {
		return fmt.Errorf("write task result: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit task result: %w", err)
	}
	return nil
}

func failedStep(step protocol.StepManifest, startedAt time.Time, stdoutPath, stderrPath string, err error) protocol.StepResult {
	return protocol.StepResult{Index: step.Index, Name: step.Name, Environment: step.Environment, StartedAt: startedAt, FinishedAt: time.Now().UTC(), ExitCode: 1, StdoutPath: stdoutPath, StderrPath: stderrPath, Error: err.Error()}
}

func safeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			return character
		}
		return '-'
	}, value)
	return strings.Trim(value, "-")
}

func choose(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
