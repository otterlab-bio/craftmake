package spec

import "fmt"

const CurrentVersion = 1

type WorkflowSpec struct {
	Name     string             `yaml:"name"`
	Version  int                `yaml:"version"`
	On       TriggerSpec        `yaml:"on"`
	Defaults DefaultsSpec       `yaml:"defaults"`
	Jobs     map[string]JobSpec `yaml:"jobs"`
}

type TriggerSpec struct {
	Otter OtterTrigger `yaml:"otter"`
}

type OtterTrigger struct {
	Workflow string   `yaml:"workflow"`
	Phase    string   `yaml:"phase"`
	Modes    []string `yaml:"modes"`
}

type DefaultsSpec struct {
	Shell         string            `yaml:"shell"`
	Environment   string            `yaml:"environment"`
	Env           map[string]string `yaml:"env"`
	Observability ObservabilitySpec `yaml:"observability"`
}

type ObservabilitySpec struct {
	Metrics             string `yaml:"metrics"`
	CaptureStdout       *bool  `yaml:"capture_stdout"`
	CaptureStderr       *bool  `yaml:"capture_stderr"`
	CompressSuccessLogs *bool  `yaml:"compress_success_logs"`
}

type JobSpec struct {
	Name          string            `yaml:"name"`
	Scope         string            `yaml:"scope"`
	Accelerator   string            `yaml:"accelerator,omitempty"`
	Dimensions    []string          `yaml:"dimensions"`
	GroupBy       []string          `yaml:"group_by"`
	Needs         []string          `yaml:"needs"`
	Inputs        map[string]string `yaml:"inputs"`
	Outputs       map[string]string `yaml:"outputs"`
	Resources     ResourceSpec      `yaml:"resources"`
	Worker        *WorkerSpec       `yaml:"worker"`
	Environment   string            `yaml:"environment"`
	Env           map[string]string `yaml:"env"`
	Steps         []StepSpec        `yaml:"steps"`
	MaxAttempts   int               `yaml:"max_attempts"`
	Observability ObservabilitySpec `yaml:"observability"`
}

type WorkerSpec struct {
	Resources   ResourceSpec `yaml:"resources"`
	MaxParallel int          `yaml:"max_parallel"`
}

type ResourceSpec struct {
	Cores     int    `yaml:"cores"`
	Memory    string `yaml:"memory"`
	Partition string `yaml:"partition"`
	Time      string `yaml:"time"`
}

type StepSpec struct {
	ID          string            `yaml:"id"`
	Name        string            `yaml:"name"`
	Run         string            `yaml:"run"`
	Shell       string            `yaml:"shell"`
	Environment string            `yaml:"environment"`
	Env         map[string]string `yaml:"env"`
	Logs        map[string]string `yaml:"logs"`
}

func (workflow WorkflowSpec) Validate() error {
	if workflow.Version != CurrentVersion {
		return fmt.Errorf("unsupported workflow version %d, expected %d", workflow.Version, CurrentVersion)
	}
	if workflow.Name == "" {
		return fmt.Errorf("workflow name is required")
	}
	if workflow.On.Otter.Workflow == "" || workflow.On.Otter.Phase == "" {
		return fmt.Errorf("on.otter.workflow and on.otter.phase are required")
	}
	if len(workflow.Jobs) == 0 {
		return fmt.Errorf("workflow must define at least one job")
	}
	for jobID, job := range workflow.Jobs {
		if err := job.validate(jobID); err != nil {
			return err
		}
	}
	return nil
}

func (job JobSpec) validate(jobID string) error {
	switch job.Accelerator {
	case "", "cpu", "gpu", "tpu":
	default:
		return fmt.Errorf("job %q has invalid accelerator %q (want cpu, gpu, or tpu)", jobID, job.Accelerator)
	}
	switch job.Scope {
	case "global", "sample", "batch":
	default:
		return fmt.Errorf("job %q has invalid scope %q", jobID, job.Scope)
	}
	knownDimensions := map[string]bool{"sample": true, "species": true}
	for _, dimension := range job.Dimensions {
		if !knownDimensions[dimension] {
			return fmt.Errorf("job %q has unsupported dimension %q", jobID, dimension)
		}
	}
	for _, groupDimension := range job.GroupBy {
		if !contains(job.Dimensions, groupDimension) {
			return fmt.Errorf("job %q group_by dimension %q is not in dimensions", jobID, groupDimension)
		}
	}
	if job.Scope == "global" && len(job.Dimensions) != 0 {
		return fmt.Errorf("global job %q cannot declare dimensions", jobID)
	}
	if job.Scope != "global" && len(job.Dimensions) == 0 {
		return fmt.Errorf("job %q must declare dimensions", jobID)
	}
	if job.Scope == "batch" {
		if job.Worker == nil {
			return fmt.Errorf("batch job %q must declare worker resources", jobID)
		}
		if job.Worker.Resources.Cores <= 0 || job.Worker.Resources.Memory == "" {
			return fmt.Errorf("batch job %q must declare positive worker cores and memory", jobID)
		}
	}
	if len(job.Steps) == 0 {
		return fmt.Errorf("job %q must contain at least one step", jobID)
	}
	for stepIndex, step := range job.Steps {
		if step.Run == "" {
			return fmt.Errorf("job %q step %d is missing run", jobID, stepIndex+1)
		}
	}
	return nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
