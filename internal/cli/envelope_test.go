package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestValidateCommandDefaultsToTextAndSupportsJSONEnvelope(t *testing.T) {
	repositoryRoot := cliRepositoryRoot(t)
	configurationPath := writeCLIRunSnapshot(t)
	workflowPath := filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step1.yaml")

	textCommand := newValidateCommand()
	var textOutput bytes.Buffer
	textCommand.SetOut(&textOutput)
	textCommand.SetErr(&textOutput)
	textCommand.SetArgs([]string{"--config", configurationPath, "--workflow", workflowPath})
	if err := textCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	if output := textOutput.String(); output != "valid: BeaverBS step1 (3 tasks, 3 submissions)\n" {
		t.Fatalf("unexpected default text output %q", output)
	}

	jsonCommand := newValidateCommand()
	var jsonOutput bytes.Buffer
	jsonCommand.SetOut(&jsonOutput)
	jsonCommand.SetErr(&jsonOutput)
	jsonCommand.SetArgs([]string{"--config", configurationPath, "--workflow", workflowPath, "--format", "json"})
	if err := jsonCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&jsonOutput)
	var envelope protocol.CommandEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.ProtocolVersion != protocol.CommandProtocolVersion || envelope.Command != "validate" || !envelope.OK {
		t.Fatalf("unexpected validate envelope: %#v", envelope)
	}
	if strings.Contains(jsonOutput.String(), "valid:") {
		t.Fatalf("JSON output contains text output: %s", jsonOutput.String())
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("expected exactly one JSON envelope, got trailing data: %v", err)
	}
}

func TestPlanCommandWrapsStableSnakeCaseJSON(t *testing.T) {
	repositoryRoot := cliRepositoryRoot(t)
	command := newPlanCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{
		"--config", writeCLIRunSnapshot(t),
		"--workflow", filepath.Join(repositoryRoot, "workflows", "BeaverBS", "step1.yaml"),
		"--format", "json",
	})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope protocol.CommandEnvelope
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Command != "plan" || !envelope.OK {
		t.Fatalf("unexpected plan envelope: %#v", envelope)
	}
	var shape struct {
		Workflow string          `json:"workflow"`
		TaskByID json.RawMessage `json:"task_by_id"`
	}
	if err := json.Unmarshal(envelope.Data, &shape); err != nil {
		t.Fatal(err)
	}
	if shape.Workflow != "BeaverBS" || len(shape.TaskByID) == 0 || bytes.Contains(envelope.Data, []byte(`"TaskByID"`)) {
		t.Fatalf("plan data does not use stable snake_case fields: %s", envelope.Data)
	}
}

func writeCLIRunSnapshot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	configuration := fmt.Sprintf(`schema_version: otter.run/v1
run:
  id: run-20260726T013245Z-kxqjrm
  created_at: "2026-07-26T01:32:45Z"
  immutable: true
project:
  id: command-test
  root: %[1]s
workflow:
  scenario: rrbs
  toolchain: modern
  asset_root: %[1]s/workflows
execution:
  executor:
    value: craftmake
    source: project
  backend:
    value: local
    source: project
    evidence: {}
  site:
    value: local
    source: project
  resources: {}
samples:
  - id: S01
    r1: %[1]s/data/S01_R1.fastq.gz
    r2: %[1]s/data/S01_R2.fastq.gz
references:
  resolved:
    - role: primary
      id: hg38
      organism: Homo sapiens
      release: GRCh38
      registry_root: %[1]s/references
      manifest_digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
      fasta:
        type: fasta
        path: %[1]s/references/hg38.fa
        sha256: sha256:2222222222222222222222222222222222222222222222222222222222222222
      indexes:
        - type: bismark
          path: %[1]s/references/bismark
          sha256: sha256:3333333333333333333333333333333333333333333333333333333333333333
paths:
  run_root: %[1]s/run
  work: %[1]s/run/work
  results: %[1]s/run/results
  logs: %[1]s/run/logs
  state: %[1]s/run/state
  metrics: %[1]s/run/metrics
digests:
  project: sha256:4444444444444444444444444444444444444444444444444444444444444444
  samples: sha256:5555555555555555555555555555555555555555555555555555555555555555
  workflow_assets: sha256:6666666666666666666666666666666666666666666666666666666666666666
`, root)
	path := filepath.Join(root, "run.yaml")
	if err := os.WriteFile(path, []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
