package action

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/spec"
	"gopkg.in/yaml.v3"
)

const SchemaVersion = "craftmake.action/v1"

type InputSpec struct {
	Description string `yaml:"description,omitempty"`
	Default     string `yaml:"default,omitempty"`
	Required    bool   `yaml:"required,omitempty"`
}

type ActionSpec struct {
	SchemaVersion string                  `yaml:"schema_version"`
	Name          string                  `yaml:"name"`
	Description   string                  `yaml:"description,omitempty"`
	Backend       string                  `yaml:"backend,omitempty"`
	Inputs        map[string]InputSpec    `yaml:"inputs,omitempty"`
	Env           map[string]string       `yaml:"env,omitempty"`
	Colab         *ColabSpec              `yaml:"colab,omitempty"`
	Jobs          map[string]spec.JobSpec `yaml:"jobs"`
}

type ColabSpec struct {
	Session            string            `yaml:"session,omitempty"`
	AuthConfig         string            `yaml:"auth_config,omitempty"`
	DriveRoot          string            `yaml:"drive_root,omitempty"`
	RemoteRoot         string            `yaml:"remote_root,omitempty"`
	ScratchRoot        string            `yaml:"scratch_root,omitempty"`
	DefaultAccelerator string            `yaml:"default_accelerator,omitempty"`
	SyncIn             bool              `yaml:"sync_in,omitempty"`
	SyncOut            bool              `yaml:"sync_out,omitempty"`
	Excludes           []string          `yaml:"excludes,omitempty"`
	PathMap            map[string]string `yaml:"path_map,omitempty"`
}

type Loaded struct {
	Action   ActionSpec
	Workflow *spec.WorkflowSpec
	Context  *compiler.Context
}

func Load(path string, overrides map[string]string, projectDir, stateDir string) (*Loaded, error) {
	return LoadWithSources(path, overrides, currentEnvironment(), projectDir, stateDir)
}

func LoadWithSources(path string, overrides, environment map[string]string, projectDir, stateDir string) (*Loaded, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve action path: %w", err)
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("read action %q: %w", absolutePath, err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var action ActionSpec
	if err := decoder.Decode(&action); err != nil {
		return nil, fmt.Errorf("parse action %q: %w", absolutePath, err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("action %q contains multiple YAML documents", absolutePath)
		}
		return nil, fmt.Errorf("parse action %q: %w", absolutePath, err)
	}
	if err := validate(action); err != nil {
		return nil, fmt.Errorf("validate action %q: %w", absolutePath, err)
	}
	values, err := resolveInputs(action.Inputs, overrides)
	if err != nil {
		return nil, err
	}
	env, err := renderMap(action.Env, parameterValues(values, environment))
	if err != nil {
		return nil, fmt.Errorf("action env: %w", err)
	}
	effectiveEnvironment := cloneMap(environment)
	for key, value := range env {
		effectiveEnvironment[key] = value
	}
	if action.Colab != nil {
		renderedColab, renderErr := renderColab(*action.Colab, parameterValues(values, effectiveEnvironment))
		if renderErr != nil {
			return nil, fmt.Errorf("colab config: %w", renderErr)
		}
		action.Colab = &renderedColab
	}
	workflow, err := normalizeWorkflow(action, values, env, effectiveEnvironment)
	if err != nil {
		return nil, err
	}
	if projectDir == "" {
		projectDir = filepath.Dir(filepath.Dir(absolutePath))
	}
	if projectDir, err = filepath.Abs(projectDir); err != nil {
		return nil, fmt.Errorf("resolve project directory: %w", err)
	}
	if stateDir == "" {
		stateDir = filepath.Join(projectDir, ".craftmake", "state")
	}
	if stateDir, err = filepath.Abs(stateDir); err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	backendName := action.Backend
	if backendName == "" {
		backendName = "local"
	}
	raw := map[string]any{"action": action.Name, "args": stringMapAny(values), "inputs": stringMapAny(values), "env": stringMapAny(effectiveEnvironment)}
	if action.Colab != nil {
		raw["colab"] = colabContext(*action.Colab)
	}
	context := &compiler.Context{Raw: raw, Workflow: compiler.WorkflowContext{Mode: "STANDALONE", WorkflowName: action.Name, Executor: "craftmake", Backend: backendName}, Paths: map[string]string{"config": absolutePath, "project": projectDir, "state": stateDir, "work": filepath.Join(projectDir, "work"), "results": filepath.Join(projectDir, "results"), "logs": filepath.Join(projectDir, "logs")}}
	return &Loaded{Action: action, Workflow: workflow, Context: context}, nil
}

func validate(action ActionSpec) error {
	if action.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %q", SchemaVersion)
	}
	if !safeComponent(action.Name) {
		return fmt.Errorf("name must be a non-empty path component")
	}
	if action.Backend != "" && action.Backend != "local" && action.Backend != "slurm" && action.Backend != "colab" {
		return fmt.Errorf("backend %q is unsupported", action.Backend)
	}
	if len(action.Jobs) == 0 {
		return fmt.Errorf("jobs must define at least one job")
	}
	if action.Colab != nil {
		if action.Backend != "colab" {
			return fmt.Errorf("colab config requires backend colab")
		}
		if action.Colab.Session != "" && !safeComponent(action.Colab.Session) {
			return fmt.Errorf("colab session must be a non-empty path component")
		}
		for _, exclude := range action.Colab.Excludes {
			normalized := filepath.ToSlash(strings.TrimSpace(exclude))
			if normalized == "" || normalized == ".." || strings.HasPrefix(normalized, "../") {
				return fmt.Errorf("invalid colab exclude %q", exclude)
			}
		}
		for host, remote := range action.Colab.PathMap {
			if strings.TrimSpace(host) == "" || strings.TrimSpace(remote) == "" {
				return fmt.Errorf("colab path_map entries must be non-empty")
			}
			if !filepath.IsAbs(host) {
				return fmt.Errorf("colab path_map host %q must be absolute", host)
			}
			if !filepath.IsAbs(remote) {
				return fmt.Errorf("colab path_map remote %q must be absolute", remote)
			}
		}
	}
	for key := range action.Inputs {
		if !safeComponent(key) {
			return fmt.Errorf("input %q is not a safe identifier", key)
		}
	}
	for jobID, job := range action.Jobs {
		if !safeComponent(jobID) {
			return fmt.Errorf("job %q is not a safe identifier", jobID)
		}
		if job.Scope != "" && job.Scope != "global" {
			return fmt.Errorf("job %q must use global scope in action/v1", jobID)
		}
		if len(job.Steps) == 0 {
			return fmt.Errorf("job %q must contain at least one step", jobID)
		}
		for index, step := range job.Steps {
			if step.Run == "" {
				return fmt.Errorf("job %q step %d is missing run", jobID, index+1)
			}
		}
	}
	return nil
}

func resolveInputs(declarations map[string]InputSpec, overrides map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(declarations))
	for name, declaration := range declarations {
		if declaration.Default != "" {
			result[name] = declaration.Default
		}
	}
	for name, value := range overrides {
		if _, ok := declarations[name]; !ok {
			return nil, fmt.Errorf("unknown input %q", name)
		}
		result[name] = value
	}
	for name, declaration := range declarations {
		if declaration.Required && result[name] == "" {
			return nil, fmt.Errorf("input %s is required", name)
		}
	}
	return result, nil
}

func normalizeWorkflow(action ActionSpec, inputs, env, environment map[string]string) (*spec.WorkflowSpec, error) {
	values := parameterValues(inputs, environment)
	workflow := &spec.WorkflowSpec{Name: action.Name, Version: spec.CurrentVersion, On: spec.TriggerSpec{Otter: spec.OtterTrigger{Workflow: action.Name, Phase: "main", Modes: []string{"STANDALONE"}}}, Defaults: spec.DefaultsSpec{Env: cloneMap(env)}, Jobs: map[string]spec.JobSpec{}}
	defaultAccelerator := ""
	if action.Colab != nil {
		defaultAccelerator = action.Colab.DefaultAccelerator
	}
	for jobID, original := range action.Jobs {
		job := original
		job.Scope = "global"
		if job.Accelerator == "" {
			job.Accelerator = defaultAccelerator
		}
		var err error
		if job.Env, err = renderMap(job.Env, values); err != nil {
			return nil, fmt.Errorf("job %s env: %w", jobID, err)
		}
		if job.Inputs, err = renderMap(job.Inputs, values); err != nil {
			return nil, fmt.Errorf("job %s inputs: %w", jobID, err)
		}
		if job.Outputs, err = renderMap(job.Outputs, values); err != nil {
			return nil, fmt.Errorf("job %s outputs: %w", jobID, err)
		}
		for index := range job.Steps {
			step := &job.Steps[index]
			if step.Run, err = compiler.Render(step.Run, values); err != nil {
				return nil, fmt.Errorf("job %s step %d command: %w", jobID, index+1, err)
			}
			if step.Env, err = renderMap(step.Env, values); err != nil {
				return nil, fmt.Errorf("job %s step %d env: %w", jobID, index+1, err)
			}
			if step.Logs, err = renderMap(step.Logs, values); err != nil {
				return nil, fmt.Errorf("job %s step %d logs: %w", jobID, index+1, err)
			}
		}
		workflow.Jobs[jobID] = job
	}
	return workflow, workflow.Validate()
}

func renderMap(values map[string]string, context map[string]any) (map[string]string, error) {
	if values == nil {
		return nil, nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string]string, len(values))
	for _, key := range keys {
		rendered, err := compiler.Render(values[key], context)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		result[key] = rendered
	}
	return result, nil
}

func renderColab(spec ColabSpec, context map[string]any) (ColabSpec, error) {
	var err error
	fields := []*string{&spec.Session, &spec.AuthConfig, &spec.DriveRoot, &spec.RemoteRoot, &spec.ScratchRoot}
	for _, field := range fields {
		if *field, err = compiler.Render(*field, context); err != nil {
			return ColabSpec{}, err
		}
	}
	for index, exclude := range spec.Excludes {
		if spec.Excludes[index], err = compiler.Render(exclude, context); err != nil {
			return ColabSpec{}, err
		}
	}
	if spec.PathMap != nil {
		rendered := make(map[string]string, len(spec.PathMap))
		for host, remote := range spec.PathMap {
			if rendered[host], err = compiler.Render(remote, context); err != nil {
				return ColabSpec{}, err
			}
		}
		spec.PathMap = rendered
	}
	return spec, nil
}

func colabContext(spec ColabSpec) map[string]any {
	return map[string]any{"session": spec.Session, "auth_config": spec.AuthConfig, "drive_root": spec.DriveRoot, "remote_root": spec.RemoteRoot, "scratch_root": spec.ScratchRoot, "sync_in": spec.SyncIn, "sync_out": spec.SyncOut, "excludes": spec.Excludes, "path_map": spec.PathMap}
}

func parameterValues(args, environment map[string]string) map[string]any {
	argValues := stringMapAny(args)
	envValues := stringMapAny(environment)
	return map[string]any{"args": argValues, "inputs": argValues, "env": envValues}
}

func currentEnvironment() map[string]string {
	result := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func stringMapAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
func cloneMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
func safeComponent(value string) bool {
	return strings.TrimSpace(value) != "" && value != "." && value != ".." && !strings.ContainsAny(value, string([]byte{47, 92}))
}
