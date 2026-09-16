package runtime

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestLoadManifestAcceptsCompatibleVersionAndRunEmitsCurrentResult(t *testing.T) {
	temporaryDirectory := t.TempDir()
	manifestPath := filepath.Join(temporaryDirectory, "manifest.json")
	resultPath := filepath.Join(temporaryDirectory, "result.json")
	manifestPayload := map[string]any{
		"protocol_version":      protocol.MinimumCompatibleVersion,
		"run_id":                "compatible-run",
		"task_id":               "compatible-task",
		"attempt":               1,
		"work_directory":        temporaryDirectory,
		"runtime_directory":     filepath.Join(temporaryDirectory, "runtime"),
		"temp_directory":        filepath.Join(temporaryDirectory, "temp"),
		"result_path":           resultPath,
		"resources":             map[string]any{"cores": 1, "memory_bytes": 1024},
		"steps":                 []any{},
		"future_optional_field": map[string]any{"enabled": true},
	}
	manifestData, err := json.Marshal(manifestPayload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}

	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ProtocolVersion != protocol.MinimumCompatibleVersion {
		t.Fatalf("unexpected compatible manifest version %d", manifest.ProtocolVersion)
	}
	if _, err := Run(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	resultData, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := protocol.DecodeTaskResultForManifest(resultData, manifest, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtocolVersion != protocol.Version {
		t.Fatalf("task runner emitted protocol version %d, expected %d", result.ProtocolVersion, protocol.Version)
	}
}

func TestLoadManifestRejectsFutureProtocolVersion(t *testing.T) {
	temporaryDirectory := t.TempDir()
	manifestPath := filepath.Join(temporaryDirectory, "manifest.json")
	manifestPayload := map[string]any{
		"protocol_version":  protocol.Version + 1,
		"run_id":            "future-run",
		"task_id":           "future-task",
		"attempt":           1,
		"work_directory":    temporaryDirectory,
		"runtime_directory": filepath.Join(temporaryDirectory, "runtime"),
		"result_path":       filepath.Join(temporaryDirectory, "result.json"),
		"resources":         map[string]any{"cores": 1, "memory_bytes": 1024},
		"steps":             []any{},
	}
	manifestData, err := json.Marshal(manifestPayload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = LoadManifest(manifestPath)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected future protocol rejection, got %v", err)
	}
}

func TestRunCompressesSuccessfulStepLogs(t *testing.T) {
	temporaryDirectory := t.TempDir()
	manifest := &protocol.TaskManifest{
		ProtocolVersion:     protocol.Version,
		RunID:               "run-success",
		TaskID:              "task-success",
		Attempt:             1,
		WorkDirectory:       temporaryDirectory,
		TempDirectory:       filepath.Join(temporaryDirectory, "tmp"),
		RuntimeDirectory:    filepath.Join(temporaryDirectory, "runtime"),
		ResultPath:          filepath.Join(temporaryDirectory, "result.json"),
		CompressSuccessLogs: true,
		Steps: []protocol.StepManifest{{
			Index:   1,
			Name:    "write logs",
			Shell:   "bash",
			Command: "printf 'hello stdout\\n'; printf 'hello stderr\\n' >&2",
		}},
	}

	result, err := Run(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" {
		t.Fatalf("unexpected result status %q", result.Status)
	}
	step := result.Steps[0]
	if !strings.HasSuffix(step.StdoutPath, ".gz") || !strings.HasSuffix(step.StderrPath, ".gz") {
		t.Fatalf("expected compressed log paths, got stdout=%q stderr=%q", step.StdoutPath, step.StderrPath)
	}
	if _, err := os.Stat(strings.TrimSuffix(step.StdoutPath, ".gz")); !os.IsNotExist(err) {
		t.Fatalf("expected original stdout log to be removed, stat error=%v", err)
	}
	if read := readCompressedLog(t, step.StdoutPath); read != "hello stdout\n" {
		t.Fatalf("unexpected compressed stdout %q", read)
	}
	if read := readCompressedLog(t, step.StderrPath); read != "hello stderr\n" {
		t.Fatalf("unexpected compressed stderr %q", read)
	}
}

func TestRunPreservesFailureLogs(t *testing.T) {
	temporaryDirectory := t.TempDir()
	manifest := &protocol.TaskManifest{
		ProtocolVersion:     protocol.Version,
		RunID:               "run-failure",
		TaskID:              "task-failure",
		Attempt:             1,
		WorkDirectory:       temporaryDirectory,
		TempDirectory:       filepath.Join(temporaryDirectory, "tmp"),
		RuntimeDirectory:    filepath.Join(temporaryDirectory, "runtime"),
		ResultPath:          filepath.Join(temporaryDirectory, "result.json"),
		CompressSuccessLogs: true,
		Steps: []protocol.StepManifest{{
			Index:   1,
			Name:    "fail with logs",
			Shell:   "bash",
			Command: "printf 'failure stdout\\n'; printf 'failure stderr\\n' >&2; exit 9",
		}},
	}

	result, err := Run(context.Background(), manifest)
	if err == nil {
		t.Fatal("expected task failure")
	}
	if result.Status != "failed" {
		t.Fatalf("unexpected result status %q", result.Status)
	}
	if result.Incident == nil || result.Incident.Category != protocol.IncidentToolInvocation {
		t.Fatalf("expected classified tool incident, got %#v", result.Incident)
	}
	step := result.Steps[0]
	if strings.HasSuffix(step.StdoutPath, ".gz") || strings.HasSuffix(step.StderrPath, ".gz") {
		t.Fatalf("failure logs must remain uncompressed: stdout=%q stderr=%q", step.StdoutPath, step.StderrPath)
	}
	if read := readPlainLog(t, step.StdoutPath); read != "failure stdout\n" {
		t.Fatalf("unexpected failure stdout %q", read)
	}
	if read := readPlainLog(t, step.StderrPath); read != "failure stderr\n" {
		t.Fatalf("unexpected failure stderr %q", read)
	}
}

func TestBuildEnvironmentCommandUsesMatchingConfiguredPrefix(t *testing.T) {
	toolDirectory := t.TempDir()
	envaPath := filepath.Join(toolDirectory, "enva")
	if err := os.WriteFile(envaPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	environmentPrefix := filepath.Join(t.TempDir(), "otter-core")
	if err := os.Mkdir(environmentPrefix, 0o755); err != nil {
		t.Fatal(err)
	}
	releaseRoot := filepath.Join(t.TempDir(), "release")
	if err := os.MkdirAll(filepath.Join(releaseRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	inheritedPath := toolDirectory + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", inheritedPath)
	t.Setenv("CRAFTMAKE_ENV_PREFIX", environmentPrefix)
	t.Setenv("OTTER_GATE6_RELEASE_ROOT", releaseRoot)

	command, err := buildEnvironmentCommand("otter-core", "bash", "/runtime/step.sh")
	if err != nil {
		t.Fatal(err)
	}
	expectedPath := strings.Join([]string{
		filepath.Join(environmentPrefix, "bin"),
		filepath.Join(releaseRoot, "bin"),
		inheritedPath,
	}, string(os.PathListSeparator))
	expectedArguments := []string{
		"enva", "--quiet", "run", "--prefix", environmentPrefix,
		"-E", "CRAFTMAKE_ENV_PREFIX=" + environmentPrefix,
		"-E", "OTTER_GATE6_RELEASE_ROOT=" + releaseRoot,
		"-E", "PATH=" + expectedPath,
		"--", "bash", "/runtime/step.sh",
	}
	if strings.Join(command.Args, "\x00") != strings.Join(expectedArguments, "\x00") {
		t.Fatalf("unexpected Enva prefix command: got %#v, want %#v", command.Args, expectedArguments)
	}
}

func TestConfiguredEnvironmentPrefixRejectsInvalidPrefix(t *testing.T) {
	environmentPrefix := filepath.Join(t.TempDir(), "otter-core")
	if err := os.Mkdir(environmentPrefix, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name        string
		prefix      string
		environment string
		want        string
	}{
		{name: "relative path", prefix: "otter-core", environment: "otter-core", want: "must be an absolute"},
		{name: "mismatched basename", prefix: environmentPrefix, environment: "otter-extra", want: "does not match workflow environment"},
		{name: "missing directory", prefix: filepath.Join(t.TempDir(), "otter-core"), environment: "otter-core", want: "inspect CRAFTMAKE_ENV_PREFIX prefix"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CRAFTMAKE_ENV_PREFIX", testCase.prefix)
			_, err := configuredEnvironmentPrefix(testCase.environment)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("expected error containing %q, got %v", testCase.want, err)
			}
		})
	}
}

func TestBuildStepEnvironmentIsolatesManagedEnvironmentFromHostJava(t *testing.T) {
	inheritedEnvironment := []string{
		"PATH=/host/bin",
		"JAVA_HOME=/host/java",
		"JRE_HOME=/host/jre",
		"CLASSPATH=/host/classes",
		"JAVA_TOOL_OPTIONS=-Xmx2g",
		"JDK_JAVA_OPTIONS=-Xms1g",
		"_JAVA_OPTIONS=-Dhost=true",
		"LD_LIBRARY_PATH=/host/lib",
		"PRESERVED_VALUE=host",
	}
	commandEnvironment := buildStepEnvironment(
		inheritedEnvironment,
		"otter-core",
		map[string]string{"CRAFTMAKE_TEMP": "/runtime/temp"},
		map[string]string{"JAVA_HOME": "/workflow/java", "PRESERVED_VALUE": "workflow"},
	)
	environmentValues := environmentEntriesByName(commandEnvironment)

	for _, removedVariable := range []string{"CLASSPATH", "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "JRE_HOME", "LD_LIBRARY_PATH", "_JAVA_OPTIONS"} {
		if _, exists := environmentValues[removedVariable]; exists {
			t.Fatalf("managed environment inherited contaminating variable %q", removedVariable)
		}
	}
	if environmentValues["JAVA_HOME"] != "/workflow/java" {
		t.Fatalf("explicit step JAVA_HOME should be preserved, got %q", environmentValues["JAVA_HOME"])
	}
	if environmentValues["PATH"] != "/host/bin" {
		t.Fatalf("PATH should remain available to the environment launcher, got %q", environmentValues["PATH"])
	}
	if environmentValues["CRAFTMAKE_TEMP"] != "/runtime/temp" {
		t.Fatalf("runtime variable was not applied: %#v", environmentValues)
	}
	if environmentValues["PRESERVED_VALUE"] != "workflow" {
		t.Fatalf("step variable should override inherited value, got %q", environmentValues["PRESERVED_VALUE"])
	}
}

func TestBuildStepEnvironmentPreservesHostJavaWithoutManagedEnvironment(t *testing.T) {
	commandEnvironment := buildStepEnvironment(
		[]string{"JAVA_HOME=/host/java", "LD_LIBRARY_PATH=/host/lib"},
		"",
		nil,
		nil,
	)
	environmentValues := environmentEntriesByName(commandEnvironment)

	if environmentValues["JAVA_HOME"] != "/host/java" || environmentValues["LD_LIBRARY_PATH"] != "/host/lib" {
		t.Fatalf("unmanaged step should inherit host environment: %#v", environmentValues)
	}
}

func TestRunUsesQuietEnvaAndCleansHostJavaBeforeStartingEnvironmentLauncher(t *testing.T) {
	temporaryDirectory := t.TempDir()
	toolDirectory := filepath.Join(temporaryDirectory, "tools")
	if err := os.MkdirAll(toolDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	envaPath := filepath.Join(toolDirectory, "enva")
	envaScript := `#!/bin/bash
set -euo pipefail
if [ "${1:-}" != "--quiet" ]; then
  printf 'enva launcher banner on stdout\n'
  printf 'enva launcher banner on stderr\n' >&2
else
  shift
fi
if [ "${JAVA_HOME+x}" = "x" ] || [ "${LD_LIBRARY_PATH+x}" = "x" ]; then
  printf 'environment launcher inherited host Java variables\n' >&2
  exit 41
fi
if [ "$1" != "run" ]; then
  exit 42
fi
shift 2
if [ "${1:-}" = "--" ]; then
  shift
fi
exec "$@"
`
	if err := os.WriteFile(envaPath, []byte(envaScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("JAVA_HOME", "/host/java")
	t.Setenv("LD_LIBRARY_PATH", "/host/lib")

	manifest := &protocol.TaskManifest{
		ProtocolVersion:  protocol.Version,
		RunID:            "run-managed-environment",
		TaskID:           "task-managed-environment",
		Attempt:          1,
		WorkDirectory:    temporaryDirectory,
		TempDirectory:    filepath.Join(temporaryDirectory, "tmp"),
		RuntimeDirectory: filepath.Join(temporaryDirectory, "runtime"),
		ResultPath:       filepath.Join(temporaryDirectory, "result.json"),
		Steps: []protocol.StepManifest{{
			Index:       1,
			Name:        "inspect environment",
			Shell:       "bash",
			Environment: "otter-core",
			Command:     "printf 'clean environment\\n'",
		}},
	}

	result, err := Run(context.Background(), manifest)
	if err != nil {
		t.Fatalf("run managed environment step: %v\nstderr: %s", err, readPlainLog(t, result.Steps[0].StderrPath))
	}
	if output := readPlainLog(t, result.Steps[0].StdoutPath); output != "clean environment\n" {
		t.Fatalf("unexpected managed environment output %q", output)
	}
	if output := readPlainLog(t, result.Steps[0].StderrPath); output != "" {
		t.Fatalf("environment launcher output contaminated step stderr %q", output)
	}
}

func TestCompressLogIfPresentReportsMissingPathWithoutFailingTask(t *testing.T) {
	result := &protocol.TaskResult{Status: "succeeded", Steps: []protocol.StepResult{{StdoutPath: filepath.Join(t.TempDir(), "missing.log")}}}
	compressSuccessfulStepLogs(result)
	if len(result.ObservabilityErrors) != 0 {
		t.Fatalf("missing optional log should not be an observability error: %#v", result.ObservabilityErrors)
	}
}

func environmentEntriesByName(environmentEntries []string) map[string]string {
	environmentValues := make(map[string]string, len(environmentEntries))
	for _, environmentEntry := range environmentEntries {
		variableName, variableValue, found := strings.Cut(environmentEntry, "=")
		if found {
			environmentValues[variableName] = variableValue
		}
	}
	return environmentValues
}

func readCompressedLog(t *testing.T, path string) string {
	t.Helper()
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	reader, err := gzip.NewReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readPlainLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
