package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/adapters/standalone"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/spf13/cobra"
)

func TestLoadPlanRoutesStandaloneWorkflowWithoutOtterIdentity(t *testing.T) {
	repositoryRoot := cliRepositoryRoot(t)
	projectDirectory := t.TempDir()
	configurationPath := filepath.Join(projectDirectory, "standalone.yaml")
	configuration := `schema_version: craftmake.standalone/v1
workflow:
  name: ArbitraryStandaloneName
  phase: step1
  backend: local
run:
  id: standalone-smoke
  project_dir: ` + projectDirectory + `
  state_dir: ` + filepath.Join(projectDirectory, ".craftmake") + `
samples:
  - id: sample-a
  - id: sample-b
`
	if err := os.WriteFile(configurationPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	options := commonOptions{
		configPath:   configurationPath,
		workflowPath: filepath.Join(repositoryRoot, "testdata", "workflows", "smoke.yaml"),
		phase:        "step1",
	}
	plan, err := loadPlan(&options)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Workflow != "Smoke" || len(plan.Tasks) != 3 {
		t.Fatalf("expected three generic standalone tasks, got workflow=%q tasks=%d", plan.Workflow, len(plan.Tasks))
	}
	if options.configKind != standalone.ConfigKindStandalone {
		t.Fatalf("expected standalone kind, got %q", options.configKind)
	}
}

func TestLoadPlanRejectsStandalonePhaseMismatch(t *testing.T) {
	repositoryRoot := cliRepositoryRoot(t)
	projectDirectory := t.TempDir()
	configurationPath := filepath.Join(projectDirectory, "standalone.yaml")
	configuration := `schema_version: craftmake.standalone/v1
workflow:
  name: ArbitraryStandaloneName
  phase: step1
  backend: local
run:
  project_dir: ` + projectDirectory + `
samples:
  - id: sample-a
`
	if err := os.WriteFile(configurationPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	options := commonOptions{
		configPath:   configurationPath,
		workflowPath: filepath.Join(repositoryRoot, "testdata", "workflows", "smoke.yaml"),
		phase:        "step2",
	}
	_, err := loadPlan(&options)
	if err == nil || !strings.Contains(err.Error(), "declares phase") {
		t.Fatalf("expected phase validation failure, got %v", err)
	}
}

func TestStandaloneSlurmDefaultsResolveWithoutImmutableOverrideRestriction(t *testing.T) {
	command := &cobra.Command{}
	command.Flags().String("partition", "", "")
	command.Flags().String("account", "", "")
	command.Flags().String("qos", "", "")
	command.Flags().String("time", "", "")
	command.Flags().String("scratch-root", "", "")
	standaloneOptions := commonOptions{
		configKind: standalone.ConfigKindStandalone,
		execution: compiler.ExecutionContext{Slurm: compiler.SlurmExecutionContext{
			Partition: "configured", DefaultTime: "01:00:00", ScratchRoot: "/scratch/configured",
		}},
	}
	partition, _, _, defaultTime, scratchRoot, err := resolveSlurmExecutionOptions(command, standaloneOptions, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if partition != "configured" || defaultTime != "01:00:00" || scratchRoot != "/scratch/configured" {
		t.Fatalf("standalone defaults were not resolved: %q %q %q", partition, defaultTime, scratchRoot)
	}
}
