package spec

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestJobSpecAcceleratorField(t *testing.T) {
	var job JobSpec
	if err := yaml.Unmarshal([]byte("accelerator: gpu\n"), &job); err != nil {
		t.Fatal(err)
	}
	if job.Accelerator != "gpu" {
		t.Fatalf("accelerator = %q, want gpu", job.Accelerator)
	}
}

func TestJobSpecAcceleratorDefault(t *testing.T) {
	var job JobSpec
	if err := yaml.Unmarshal([]byte("name: x\n"), &job); err != nil {
		t.Fatal(err)
	}
	if job.Accelerator != "" {
		t.Fatalf("accelerator = %q, want empty default", job.Accelerator)
	}
}

func TestJobSpecAcceleratorValidation(t *testing.T) {
	valid := []string{"", "cpu", "gpu", "tpu"}
	for _, accel := range valid {
		job := JobSpec{Name: "j", Scope: "global", Accelerator: accel, Steps: []StepSpec{{Run: "echo hi"}}}
		if err := job.validate("j"); err != nil {
			t.Fatalf("accelerator %q should be valid: %v", accel, err)
		}
	}
	invalid := []string{"quantum", "TPU", "GPU", "Cpu"}
	for _, accel := range invalid {
		job := JobSpec{Name: "j", Scope: "global", Accelerator: accel, Steps: []StepSpec{{Run: "echo hi"}}}
		if err := job.validate("j"); err == nil {
			t.Fatalf("accelerator %q should be invalid", accel)
		}
	}
}
