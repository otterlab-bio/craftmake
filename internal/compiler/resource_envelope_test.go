package compiler

import (
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/spec"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func TestCompileRejectsSubmissionOutsidePhaseResourceEnvelope(t *testing.T) {
	workflow := batchWorkflow(
		spec.ResourceSpec{Cores: 4, Memory: "8G", Partition: "amd_512"},
		spec.ResourceSpec{Cores: 2, Memory: "4G", Partition: "amd_512"},
		2,
	)
	context := batchContext(2)
	context.Execution = ExecutionContext{
		PhaseResources: map[string]protocol.ResourceRequest{
			"batch": {Cores: 2, MemoryByte: 8 << 30, Partition: "amd_512", Time: "02:00:00"},
		},
	}

	_, err := Compile(workflow, context)
	if err == nil || !strings.Contains(err.Error(), "has 2 cores") {
		t.Fatalf("expected phase envelope core failure, got %v", err)
	}
}

func TestCompileUsesPhaseEnvelopeTimeForUnspecifiedTaskTime(t *testing.T) {
	workflow := batchWorkflow(
		spec.ResourceSpec{Cores: 4, Memory: "8G", Partition: "amd_512"},
		spec.ResourceSpec{Cores: 2, Memory: "4G", Partition: "amd_512"},
		2,
	)
	context := batchContext(2)
	context.Execution = ExecutionContext{
		PhaseResources: map[string]protocol.ResourceRequest{
			"batch": {Cores: 4, MemoryByte: 8 << 30, Partition: "amd_512", Time: "02:00:00"},
		},
	}

	plan, err := Compile(workflow, context)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range plan.Tasks {
		if task.Resources.Time != "02:00:00" {
			t.Fatalf("task %q expected phase time, got %q", task.ID, task.Resources.Time)
		}
		if task.Worker != nil && task.Worker.Resources.Time != "02:00:00" {
			t.Fatalf("task worker %q expected phase time, got %q", task.ID, task.Worker.Resources.Time)
		}
	}
}

func TestCompileAcceptsBatchWorkerInsidePhaseResourceEnvelope(t *testing.T) {
	workflow := batchWorkflow(
		spec.ResourceSpec{Cores: 4, Memory: "8G", Partition: "amd_512"},
		spec.ResourceSpec{Cores: 2, Memory: "4G", Partition: "amd_512"},
		2,
	)
	context := batchContext(2)
	context.Execution = ExecutionContext{
		PhaseResources: map[string]protocol.ResourceRequest{
			"batch": {Cores: 4, MemoryByte: 8 << 30, Partition: "amd_512", Time: "02:00:00"},
		},
	}

	if _, err := Compile(workflow, context); err != nil {
		t.Fatal(err)
	}
}
