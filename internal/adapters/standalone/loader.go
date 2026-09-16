package standalone

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/otterlab-bio/craftmake/internal/compiler"
	"gopkg.in/yaml.v3"
)

const SchemaVersion = "craftmake.standalone/v1"

var slurmTimePattern = regexp.MustCompile("^(?:[0-9]+-)?[0-9]{1,2}:[0-5][0-9]:[0-5][0-9]\\z")

type configuration struct {
	SchemaVersion string            `yaml:"schema_version"`
	Workflow      workflowConfig    `yaml:"workflow"`
	Run           runConfig         `yaml:"run"`
	Execution     executionConfig   `yaml:"execution"`
	Samples       []sampleConfig    `yaml:"samples"`
	Acquisition   acquisitionConfig `yaml:"acquisition"`
}

type workflowConfig struct {
	Name    string `yaml:"name"`
	Phase   string `yaml:"phase"`
	Backend string `yaml:"backend"`
}

type runConfig struct {
	ID         string `yaml:"id"`
	ProjectDir string `yaml:"project_dir"`
	StateDir   string `yaml:"state_dir"`
}

type executionConfig struct {
	Slurm slurmConfig `yaml:"slurm"`
}

type slurmConfig struct {
	Partition   string `yaml:"partition"`
	Account     string `yaml:"account"`
	QOS         string `yaml:"qos"`
	MaxJobs     int    `yaml:"max_jobs"`
	DefaultTime string `yaml:"default_time"`
	ScratchRoot string `yaml:"scratch_root"`
}

type sampleConfig struct {
	ID string `yaml:"id"`
}

type acquisitionConfig struct {
	Accession           string `yaml:"accession"`
	ArchivePath         string `yaml:"archive_path"`
	ArchiveChecksumPath string `yaml:"archive_checksum_path"`
	ArchiveSHA256       string `yaml:"archive_sha256"`
	ArchiveMD5          string `yaml:"archive_md5"`
	ArchiveBytes        int64  `yaml:"archive_bytes"`
	OutputRoot          string `yaml:"output_root"`
	FasterqDump         string `yaml:"fasterq_dump"`
	Pigz                string `yaml:"pigz"`
}

// Load parses a strict, Otter-free Craftmake configuration.
func Load(path string) (*compiler.Context, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve standalone config path: %w", err)
	}
	fileHandle, err := os.Open(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("read standalone config %q: %w", absolutePath, err)
	}
	defer fileHandle.Close()

	decoder := yaml.NewDecoder(fileHandle)
	decoder.KnownFields(true)
	var parsed configuration
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse standalone config %q: %w", absolutePath, err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse standalone config %q: multiple YAML documents are not supported", absolutePath)
		}
		return nil, fmt.Errorf("parse standalone config %q: %w", absolutePath, err)
	}
	if err := validate(parsed); err != nil {
		return nil, fmt.Errorf("validate standalone config %q: %w", absolutePath, err)
	}

	rawBytes, err := os.ReadFile(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("read standalone config values %q: %w", absolutePath, err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(rawBytes, &raw); err != nil {
		return nil, fmt.Errorf("parse standalone config values %q: %w", absolutePath, err)
	}

	stateDirectory := parsed.Run.StateDir
	if stateDirectory == "" {
		stateDirectory = filepath.Join(parsed.Run.ProjectDir, "workflow", ".craftmake")
	}
	samples := make([]compiler.SampleContext, 0, len(parsed.Samples))
	for sampleIndex, sample := range parsed.Samples {
		samples = append(samples, compiler.SampleContext{ID: sample.ID, Index: sampleIndex})
	}
	return &compiler.Context{
		Raw: raw,
		Workflow: compiler.WorkflowContext{
			Mode:         "STANDALONE",
			WorkflowName: parsed.Workflow.Name,
			JobID:        parsed.Run.ID,
			UserID:       "standalone",
			Executor:     "craftmake",
			Backend:      parsed.Workflow.Backend,
		},
		Execution: compiler.ExecutionContext{Slurm: compiler.SlurmExecutionContext{
			Partition:   parsed.Execution.Slurm.Partition,
			Account:     parsed.Execution.Slurm.Account,
			QOS:         parsed.Execution.Slurm.QOS,
			MaxJobs:     parsed.Execution.Slurm.MaxJobs,
			DefaultTime: parsed.Execution.Slurm.DefaultTime,
			ScratchRoot: parsed.Execution.Slurm.ScratchRoot,
		}},
		Samples: samples,
		Paths: map[string]string{
			"config":  absolutePath,
			"project": parsed.Run.ProjectDir,
			"state":   stateDirectory,
			"work":    filepath.Join(parsed.Run.ProjectDir, "work"),
			"results": filepath.Join(parsed.Run.ProjectDir, "results"),
			"logs":    filepath.Join(parsed.Run.ProjectDir, "logs"),
		},
	}, nil
}

func validate(parsed configuration) error {
	if parsed.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %q", SchemaVersion)
	}
	if !safeComponent(parsed.Workflow.Name) {
		return fmt.Errorf("workflow.name must be a non-empty path component")
	}
	if !safeComponent(parsed.Workflow.Phase) {
		return fmt.Errorf("workflow.phase must be a non-empty path component")
	}
	if parsed.Workflow.Backend != "local" && parsed.Workflow.Backend != "slurm" {
		return fmt.Errorf("workflow.backend must be local or slurm")
	}
	if !filepath.IsAbs(parsed.Run.ProjectDir) {
		return fmt.Errorf("run.project_dir must be an absolute path")
	}
	if parsed.Run.StateDir != "" && !filepath.IsAbs(parsed.Run.StateDir) {
		return fmt.Errorf("run.state_dir must be an absolute path")
	}
	if parsed.Run.ID != "" && !safeComponent(parsed.Run.ID) {
		return fmt.Errorf("run.id must be a path-safe identifier")
	}
	if parsed.Execution.Slurm.MaxJobs < 0 {
		return fmt.Errorf("execution.slurm.max_jobs must be zero or positive")
	}
	if parsed.Workflow.Backend != "slurm" && !isEmptySlurmConfig(parsed.Execution.Slurm) {
		return fmt.Errorf("execution.slurm is only valid when workflow.backend is slurm")
	}
	if parsed.Execution.Slurm.DefaultTime != "" && !slurmTimePattern.MatchString(parsed.Execution.Slurm.DefaultTime) {
		return fmt.Errorf("execution.slurm.default_time must use D-HH:MM:SS or HH:MM:SS format")
	}
	if parsed.Execution.Slurm.ScratchRoot != "" && !filepath.IsAbs(parsed.Execution.Slurm.ScratchRoot) {
		return fmt.Errorf("execution.slurm.scratch_root must be an absolute path")
	}
	seenSampleIDs := make(map[string]bool, len(parsed.Samples))
	for sampleIndex, sample := range parsed.Samples {
		if !safeComponent(sample.ID) {
			return fmt.Errorf("samples[%d].id must be a non-empty path component", sampleIndex)
		}
		if seenSampleIDs[sample.ID] {
			return fmt.Errorf("samples[%d].id %q is duplicated", sampleIndex, sample.ID)
		}
		seenSampleIDs[sample.ID] = true
	}
	return nil
}

func isEmptySlurmConfig(config slurmConfig) bool {
	return config.Partition == "" && config.Account == "" && config.QOS == "" && config.MaxJobs == 0 && config.DefaultTime == "" && config.ScratchRoot == ""
}

func safeComponent(value string) bool {
	return strings.TrimSpace(value) != "" && value != "." && value != ".." && !strings.ContainsAny(value, `/\\`)
}
