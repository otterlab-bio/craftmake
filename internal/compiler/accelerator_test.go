package compiler

import (
	"testing"

	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestTaskCarriesAccelerator(t *testing.T) {
	workflow := &spec.WorkflowSpec{
		Name:     "accel",
		Version:  spec.CurrentVersion,
		On:       spec.TriggerSpec{Otter: spec.OtterTrigger{Workflow: "W", Phase: "main", Modes: []string{"STANDALONE"}}},
		Defaults: spec.DefaultsSpec{Shell: "bash"},
		Jobs: map[string]spec.JobSpec{
			"train": {
				Scope:       "global",
				Accelerator: "gpu",
				Resources:   spec.ResourceSpec{Cores: 1, Memory: "1G"},
				Steps:       []spec.StepSpec{{Run: "echo hi"}},
			},
		},
	}
	context := &Context{
		Workflow: WorkflowContext{Mode: "STANDALONE", WorkflowName: "W", Executor: "craftmake", Backend: "colab"},
		Paths:    map[string]string{"results": "/tmp/results"},
	}
	plan, err := Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(plan.Tasks))
	}
	if plan.Tasks[0].Accelerator != "gpu" {
		t.Fatalf("task accelerator = %q, want gpu", plan.Tasks[0].Accelerator)
	}
	// The scheduler reads task.Resources.Accelerator to populate the manifest,
	// so it must carry the same value (TDD 1.6).
	if plan.Tasks[0].Resources.Accelerator != "gpu" {
		t.Fatalf("task resources accelerator = %q, want gpu", plan.Tasks[0].Resources.Accelerator)
	}
}

func TestTaskAcceleratorEmptyByDefault(t *testing.T) {
	workflow := &spec.WorkflowSpec{
		Name:     "accel2",
		Version:  spec.CurrentVersion,
		On:       spec.TriggerSpec{Otter: spec.OtterTrigger{Workflow: "W", Phase: "main", Modes: []string{"STANDALONE"}}},
		Defaults: spec.DefaultsSpec{Shell: "bash"},
		Jobs: map[string]spec.JobSpec{
			"job": {
				Scope:     "global",
				Resources: spec.ResourceSpec{Cores: 1, Memory: "1G"},
				Steps:     []spec.StepSpec{{Run: "echo hi"}},
			},
		},
	}
	context := &Context{
		Workflow: WorkflowContext{Mode: "STANDALONE", WorkflowName: "W", Executor: "craftmake", Backend: "colab"},
		Paths:    map[string]string{"results": "/tmp/results"},
	}
	plan, err := Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tasks[0].Accelerator != "" {
		t.Fatalf("task accelerator = %q, want empty", plan.Tasks[0].Accelerator)
	}
}
