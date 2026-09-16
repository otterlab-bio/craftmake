package compiler

import (
	"fmt"
	"strings"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

// validatePhaseResourceEnvelope verifies that an optional phase allocation limit
// can accommodate every Craftmake submission in the compiled workflow.
func validatePhaseResourceEnvelope(plan *Plan, context *Context) error {
	phaseEnvelope, declared := context.Execution.PhaseResources[plan.Phase]
	if !declared {
		return nil
	}
	if phaseEnvelope.Cores <= 0 || phaseEnvelope.MemoryByte <= 0 || strings.TrimSpace(phaseEnvelope.Partition) == "" || strings.TrimSpace(phaseEnvelope.Time) == "" {
		return fmt.Errorf("phase resource envelope %q requires positive cores, memory, partition, and time", plan.Phase)
	}
	if context.Execution.Slurm.Partition != "" && context.Execution.Slurm.Partition != phaseEnvelope.Partition {
		return fmt.Errorf("phase resource envelope %q uses partition %q but the resolved Slurm execution uses %q", plan.Phase, phaseEnvelope.Partition, context.Execution.Slurm.Partition)
	}

	for _, submission := range plan.Submissions {
		if err := validateSubmissionFitsEnvelope(plan.Phase, submission, phaseEnvelope); err != nil {
			return err
		}
	}
	return nil
}

func validateSubmissionFitsEnvelope(phase string, submission SubmissionGroup, envelope protocol.ResourceRequest) error {
	if err := validateRequestFitsEnvelope(phase, submission.ID, "allocation", submission.Resources, envelope); err != nil {
		return err
	}
	if submission.Worker != nil {
		if err := validateRequestFitsEnvelope(phase, submission.ID, "worker", submission.Worker.Resources, envelope); err != nil {
			return err
		}
	}
	return nil
}

func validateRequestFitsEnvelope(phase, submissionID, requestKind string, request, envelope protocol.ResourceRequest) error {
	if request.Cores > envelope.Cores {
		return fmt.Errorf("phase resource envelope %q has %d cores but %s %q requests %d", phase, envelope.Cores, requestKind, submissionID, request.Cores)
	}
	if request.MemoryByte > envelope.MemoryByte {
		return fmt.Errorf("phase resource envelope %q has %d bytes but %s %q requests %d", phase, envelope.MemoryByte, requestKind, submissionID, request.MemoryByte)
	}
	if request.Partition != "" && request.Partition != envelope.Partition {
		return fmt.Errorf("phase resource envelope %q uses partition %q but %s %q requests %q", phase, envelope.Partition, requestKind, submissionID, request.Partition)
	}
	if request.Time != "" && request.Time != envelope.Time {
		return fmt.Errorf("phase resource envelope %q uses time %q but %s %q requests %q", phase, envelope.Time, requestKind, submissionID, request.Time)
	}
	return nil
}
