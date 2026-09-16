package standalone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBuildsStandaloneContext(t *testing.T) {
	projectDirectory := t.TempDir()
	configurationPath := filepath.Join(t.TempDir(), "config.yaml")
	configuration := `schema_version: craftmake.standalone/v1
workflow:
  name: Smoke
  phase: step1
  backend: slurm
run:
  id: smoke-run
  project_dir: ` + projectDirectory + `
  state_dir: ` + filepath.Join(projectDirectory, ".craftmake") + `
execution:
  slurm:
    partition: compute
    max_jobs: 2
    default_time: 01:00:00
    scratch_root: ` + filepath.Join(projectDirectory, "scratch") + `
samples:
  - id: sample-a
  - id: sample-b
`
	writeFile(t, configurationPath, configuration)
	context, err := Load(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	if context.Workflow.Mode != "STANDALONE" || context.Workflow.WorkflowName != "Smoke" || context.Workflow.Backend != "slurm" {
		t.Fatalf("unexpected workflow context: %#v", context.Workflow)
	}
	if context.Paths["project"] != projectDirectory || context.Execution.Slurm.Partition != "compute" || context.Execution.Slurm.MaxJobs != 2 {
		t.Fatalf("unexpected execution context: paths=%#v slurm=%#v", context.Paths, context.Execution.Slurm)
	}
	if len(context.Samples) != 2 || context.Samples[1].ID != "sample-b" {
		t.Fatalf("unexpected standalone samples: %#v", context.Samples)
	}
}

func TestLoadRejectsInvalidStandaloneConfigurations(t *testing.T) {
	projectDirectory := t.TempDir()
	testCases := []struct {
		name          string
		configuration string
		expectedError string
	}{
		{name: "unknown field", configuration: baseConfiguration(projectDirectory) + "unknown: value\n", expectedError: "field unknown"},
		{name: "invalid backend", configuration: strings.Replace(baseConfiguration(projectDirectory), "backend: local", "backend: invalid", 1), expectedError: "workflow.backend"},
		{name: "relative project", configuration: strings.Replace(baseConfiguration(projectDirectory), "project_dir: "+projectDirectory, "project_dir: relative", 1), expectedError: "run.project_dir"},
		{name: "slurm on local", configuration: baseConfiguration(projectDirectory) + "execution:\n  slurm:\n    partition: compute\n", expectedError: "execution.slurm"},
		{name: "invalid slurm time", configuration: `schema_version: craftmake.standalone/v1
workflow:
  name: Smoke
  phase: step1
  backend: slurm
run:
  project_dir: ` + projectDirectory + `
execution:
  slurm:
    default_time: tomorrow
`, expectedError: "default_time"},
		{name: "duplicate sample", configuration: baseConfiguration(projectDirectory) + "samples:\n  - id: sample-a\n  - id: sample-a\n", expectedError: "duplicated"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			configurationPath := filepath.Join(t.TempDir(), "config.yaml")
			writeFile(t, configurationPath, testCase.configuration)
			_, err := Load(configurationPath)
			if err == nil || !strings.Contains(err.Error(), testCase.expectedError) {
				t.Fatalf("expected %q, got %v", testCase.expectedError, err)
			}
		})
	}
}

func TestDetectConfigKindRoutesSchemasAndCompleteLegacyConfig(t *testing.T) {
	projectDirectory := t.TempDir()
	testCases := []struct {
		name     string
		contents string
		expected ConfigKind
	}{
		{name: "standalone", contents: baseConfiguration(projectDirectory), expected: ConfigKindStandalone},
		{name: "otter snapshot", contents: "schema_version: otter.run/v1\n", expected: ConfigKindOtterRun},
		{name: "legacy", contents: `mode: RRBS
species: human
SIDs: [sample-a]
input:
  fastq_dir: /data
workflow:
  jobid: legacy-run
reference:
  files:
    fasta: [/references/human.fasta]
`, expected: ConfigKindLegacy},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			configurationPath := filepath.Join(t.TempDir(), "config.yaml")
			writeFile(t, configurationPath, testCase.contents)
			actual, err := DetectConfigKind(configurationPath)
			if err != nil {
				t.Fatal(err)
			}
			if actual != testCase.expected {
				t.Fatalf("expected %q, got %q", testCase.expected, actual)
			}
		})
	}
}

func TestDetectConfigKindRejectsIncompleteLegacyShape(t *testing.T) {
	configurationPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, configurationPath, "workflow:\n  mode: RRBS\n")
	_, err := DetectConfigKind(configurationPath)
	if err == nil || !strings.Contains(err.Error(), "neither an explicit") {
		t.Fatalf("expected actionable routing guidance, got %v", err)
	}
}

func baseConfiguration(projectDirectory string) string {
	return `schema_version: craftmake.standalone/v1
workflow:
  name: Smoke
  phase: step1
  backend: local
run:
  project_dir: ` + projectDirectory + `
`
}

func writeFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
