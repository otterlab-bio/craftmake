package otter

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestLoadRunV1BuildsCompilerContext(t *testing.T) {
	configurationPath := writeRunSnapshot(t, runSnapshotYAML("rna-pdx", "craftmake", "slurm", "true", true))
	context, err := Load(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	if context.Workflow.WorkflowName != "BeaverRNASEQPDX" || context.Workflow.Mode != "RNASEQ" || !context.Workflow.PDXMode {
		t.Fatalf("unexpected workflow context: %#v", context.Workflow)
	}
	if context.Workflow.Toolchain != "modern" || len(context.Workflow.LegacyExtensions) != 0 {
		t.Fatalf("unexpected workflow toolchain context: %#v", context.Workflow)
	}
	if context.Execution.Slurm.Partition != "compute" || context.Execution.Slurm.Account != "genomics" || context.Execution.Slurm.QOS != "normal" || context.Execution.Slurm.MaxJobs != 4 || context.Execution.Slurm.DefaultTime != "2-00:00:00" || context.Execution.Slurm.ScratchRoot != "/scratch/otter" {
		t.Fatalf("unexpected resolved Slurm execution context: %#v", context.Execution.Slurm)
	}
	if len(context.Samples) != 1 || context.Samples[0].Read1 != "/project/data/S01_R1.fastq.gz" || context.Samples[0].Adapter1 != "AUTO" {
		t.Fatalf("unexpected sample context: %#v", context.Samples)
	}
	metadata := context.Raw["metadata"].(map[string]any)
	if metadata["group_levels"] != 0 {
		t.Fatalf("single sample without a group should expose zero group levels: %#v", metadata)
	}
	workflow := context.Raw["workflow"].(map[string]any)
	species := workflow["species"].(map[string]any)
	if species["expression"] != "human" {
		t.Fatalf("expected human expression species, got %#v", species)
	}
	output := context.Raw["output"].(map[string]any)
	if output["raw_dir"] != "/project/data" || output["trim_dir"] != "/project/runs/example/work/trim" || output["analysis_dir"] != "/project/runs/example/results" {
		t.Fatalf("unexpected derived output paths: %#v", output)
	}
	directories := context.Raw["directories"].(map[string]any)
	if directories["sid_log"] != "/project/runs/example/logs" ||
		directories["methylation_call"] != "/project/runs/example/work/expression" ||
		directories["selfconfig"] != "/project" ||
		directories["qctb_config"] != "/project/runs/example/state/config.yaml" {
		t.Fatalf("unexpected derived directories: %#v", directories)
	}
	reference := context.Raw["reference"].(map[string]any)
	if reference["graft_fasta"] != "/refs/hg38.fa" || reference["host_fasta"] != "/refs/mm39.fa" {
		t.Fatalf("unexpected PDX references: %#v", reference)
	}
	rnaseq := reference["rnaseq"].(map[string]any)
	if rnaseq["graft_gtf"] != "/refs/hg38.gtf" || rnaseq["primary_reference"] != "/refs/hg38-star" {
		t.Fatalf("unexpected RNA references: %#v", rnaseq)
	}
}

func TestSeq2matSpeciesForReference(t *testing.T) {
	testCases := []struct {
		name      string
		reference resolvedReference
		want      string
		wantError string
	}{
		{name: "human", reference: resolvedReference{ID: "hg38", Organism: "Homo sapiens"}, want: "human"},
		{name: "mouse", reference: resolvedReference{ID: "mm10", Organism: "Mus musculus"}, want: "mouse"},
		{name: "unsupported", reference: resolvedReference{ID: "unknown", Organism: "Danio rerio"}, wantError: "unsupported organism"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := seq2matSpeciesForReference(testCase.reference)
			if testCase.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
					t.Fatalf("expected error containing %q, got %v", testCase.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("seq2matSpeciesForReference() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestSampleGroupLevelCount(t *testing.T) {
	testCases := []struct {
		name    string
		samples []sampleRecord
		want    int
	}{
		{name: "no groups", samples: []sampleRecord{{ID: "S01"}}, want: 0},
		{name: "one group", samples: []sampleRecord{{ID: "S01", Group: "case"}, {ID: "S02", Group: "case"}}, want: 1},
		{name: "two groups", samples: []sampleRecord{{ID: "S01", Group: "case"}, {ID: "S02", Group: "control"}}, want: 2},
		{name: "trim whitespace", samples: []sampleRecord{{ID: "S01", Group: " case "}, {ID: "S02", Group: "case"}}, want: 1},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := sampleGroupLevelCount(testCase.samples); got != testCase.want {
				t.Fatalf("sampleGroupLevelCount() = %d, want %d", got, testCase.want)
			}
		})
	}
}

func TestLoadRunV1CompilesRRBSQCTBInputFromCanonicalRun(t *testing.T) {
	repositoryRoot := repositoryRootForRunLoaderTest(t)
	workflow, err := spec.Load(filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step3-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	context, err := Load(writeRunSnapshot(t, runSnapshotYAML("rrbs", "craftmake", "local", "true", false)))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := compiler.Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	qualityControlTask := plan.TaskByID["BeaverBS/step3-check/qc_summary"]
	if qualityControlTask == nil {
		t.Fatal("compiled RRBS plan is missing the QCTB summary task")
	}
	qualityControlInput := qualityControlTask.Inputs["run_config"]
	if len(qualityControlInput) != 1 || !strings.HasSuffix(qualityControlInput[0], "/run.yaml") {
		t.Fatalf("QCTB run_config input = %#v, want the immutable run snapshot", qualityControlInput)
	}
	if !strings.Contains(qualityControlTask.Steps[0].Command, "qctb --config '"+qualityControlInput[0]+"'") {
		t.Fatalf("QCTB command does not consume its declared input: %s", qualityControlTask.Steps[0].Command)
	}
}

func repositoryRootForRunLoaderTest(t *testing.T) string {
	t.Helper()
	_, currentFile, _, available := runtime.Caller(0)
	if !available {
		t.Fatal("resolve run loader test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
}

func TestLoadRunV1PreservesLegacyEquivalentToolchainContract(t *testing.T) {
	configuration := strings.Replace(
		runSnapshotYAML("rrbs", "craftmake", "local", "true", false),
		"  toolchain: modern",
		"  toolchain: legacy-equivalent\n  legacy_extensions: [ccgg, insert-length]",
		1,
	)
	context, err := Load(writeRunSnapshot(t, configuration))
	if err != nil {
		t.Fatal(err)
	}
	if context.Workflow.Toolchain != "legacy-equivalent" ||
		!slices.Equal(context.Workflow.LegacyExtensions, []string{"ccgg", "insert-length"}) {
		t.Fatalf("unexpected legacy-equivalent workflow context: %#v", context.Workflow)
	}
}

func TestLoadRunV1MapsCatalogScenarios(t *testing.T) {
	testCases := []struct {
		scenario string
		workflow string
		mode     string
	}{
		{scenario: "rrbs", workflow: "BeaverBS", mode: "RRBS"},
		{scenario: "wgbs", workflow: "BeaverBS", mode: "WGBS"},
		{scenario: "rnaseq", workflow: "BeaverRNA", mode: "RNASEQ"},
		{scenario: "bs-pdx", workflow: "BeaverPDX", mode: "RRBS"},
		{scenario: "rna-pdx", workflow: "BeaverRNASEQPDX", mode: "RNASEQ"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.scenario, func(t *testing.T) {
			pdx := strings.Contains(testCase.scenario, "pdx")
			context, err := Load(writeRunSnapshot(t, runSnapshotYAML(testCase.scenario, "craftmake", "local", "true", pdx)))
			if err != nil {
				t.Fatal(err)
			}
			if context.Workflow.WorkflowName != testCase.workflow || context.Workflow.Mode != testCase.mode {
				t.Fatalf("scenario %q resolved to workflow=%q mode=%q", testCase.scenario, context.Workflow.WorkflowName, context.Workflow.Mode)
			}
		})
	}
}

func TestLoadRunV1RejectsInvalidContracts(t *testing.T) {
	base := runSnapshotYAML("rrbs", "craftmake", "local", "true", false)
	testCases := []struct {
		name          string
		configuration string
		want          string
	}{
		{name: "schema", configuration: strings.Replace(base, "otter.run/v1", "otter.project/v1", 1), want: "schema_version"},
		{name: "run id", configuration: strings.Replace(base, "id: run-20260726T000000Z-abcdef", "id: ''", 1), want: "run.id"},
		{name: "mutable", configuration: strings.Replace(base, "immutable: true", "immutable: false", 1), want: "immutable"},
		{name: "executor", configuration: strings.Replace(base, "value: craftmake", "value: snakemake", 1), want: "executor"},
		{name: "auto backend", configuration: strings.Replace(base, "value: local", "value: auto", 1), want: "backend"},
		{name: "samples", configuration: strings.Replace(base, "samples:\n  - id: S01\n    r1: /project/data/S01_R1.fastq.gz\n    r2: /project/data/S01_R2.fastq.gz\n    adapter_r1: AUTO\n    adapter_r2: AUTO", "samples: []", 1), want: "samples"},
		{name: "unknown field", configuration: base + "unexpected: true\n", want: "field unexpected not found"},
		{name: "invalid legacy extension", configuration: strings.Replace(base, "  asset_root: /project/workflows", "  asset_root: /project/workflows\n  legacy_extensions: [unsupported]", 1), want: "legacy extension"},
		{name: "duplicated legacy extension", configuration: strings.Replace(base, "  asset_root: /project/workflows", "  asset_root: /project/workflows\n  legacy_extensions: [ccgg, ccgg]", 1), want: "duplicated"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Load(writeRunSnapshot(t, testCase.configuration))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("expected error containing %q, got %v", testCase.want, err)
			}
		})
	}
}

func writeRunSnapshot(t *testing.T, configuration string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.yaml")
	if err := os.WriteFile(path, []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runSnapshotYAML(scenario, executor, backend, immutable string, includeHost bool) string {
	hostReference := ""
	if includeHost {
		hostReference = `
    - role: host
      id: mm39
      release: GRCm39
      registry_root: /refs
      manifest_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      fasta:
        type: fasta
        path: /refs/mm39.fa
        sha256: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
      annotations:
        - type: gtf
          path: /refs/mm39.gtf
          sha256: sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
      indexes:
        - type: star
          path: /refs/mm39-star
          sha256: sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
`
	}
	primaryRole := "primary"
	if includeHost {
		primaryRole = "graft"
	}
	slurmResources := ""
	if backend == "slurm" {
		slurmResources = "  slurm:\n" +
			"    partition:\n      value: compute\n      source: profile\n" +
			"    account:\n      value: genomics\n      source: profile\n" +
			"    qos:\n      value: normal\n      source: profile\n" +
			"    max_jobs:\n      value: 4\n      source: profile\n" +
			"    default_time:\n      value: 2-00:00:00\n      source: profile\n" +
			"    scratch_root:\n      value: /scratch/otter\n      source: profile\n"
	}
	return fmt.Sprintf(`schema_version: otter.run/v1
run:
  id: run-20260726T000000Z-abcdef
  created_at: "2026-07-26T00:00:00Z"
  immutable: %s
project:
  id: project-a
  root: /project
workflow:
  scenario: %s
  toolchain: modern
  asset_root: /project/workflows
execution:
  executor:
    value: %s
    source: project
  backend:
    value: %s
    source: project
    evidence: {}
  site:
    value: local
    source: project
  resources: {}
%ssamples:
  - id: S01
    r1: /project/data/S01_R1.fastq.gz
    r2: /project/data/S01_R2.fastq.gz
    adapter_r1: AUTO
    adapter_r2: AUTO
references:
  project_selection: {}
  effective_selection: {}
  override: false
  override_source: none
  resolved:
    - role: %s
      id: hg38
      release: GRCh38
      organism: Homo sapiens
      registry_root: /refs
      manifest_digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
      fasta:
        type: fasta
        path: /refs/hg38.fa
        sha256: sha256:2222222222222222222222222222222222222222222222222222222222222222
      annotations:
        - type: gtf
          path: /refs/hg38.gtf
          sha256: sha256:3333333333333333333333333333333333333333333333333333333333333333
      indexes:
        - type: bismark
          path: /refs/hg38-bismark
          sha256: sha256:4444444444444444444444444444444444444444444444444444444444444444
        - type: star
          path: /refs/hg38-star
          sha256: sha256:5555555555555555555555555555555555555555555555555555555555555555
%s
paths:
  run_root: /project/runs/example
  work: /project/runs/example/work
  results: /project/runs/example/results
  logs: /project/runs/example/logs
  state: /project/runs/example/state
  metrics: /project/runs/example/metrics
digests:
  project: sha256:6666666666666666666666666666666666666666666666666666666666666666
  samples: sha256:7777777777777777777777777777777777777777777777777777777777777777
  workflow_assets: sha256:8888888888888888888888888888888888888888888888888888888888888888
observability: {}
parity: {}
`, immutable, scenario, executor, backend, slurmResources, primaryRole, hostReference)
}
