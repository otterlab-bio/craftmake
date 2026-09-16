package cli

import (
	"fmt"

	"github.com/otterlab-bio/craftmake/internal/scheduler"
	"github.com/otterlab-bio/craftmake/internal/store"
)

type planDigests struct {
	Config   string
	Workflow string
}

func calculatePlanDigests(configPath string, workflowPath string) (planDigests, error) {
	configDigest, err := scheduler.DigestFile(configPath)
	if err != nil {
		return planDigests{}, err
	}
	workflowDigest, err := scheduler.DigestFile(workflowPath)
	if err != nil {
		return planDigests{}, err
	}
	return planDigests{Config: configDigest, Workflow: workflowDigest}, nil
}

func validatePersistedRunDigests(run store.Run) (planDigests, error) {
	digests, err := calculatePlanDigests(run.ConfigPath, run.WorkflowPath)
	if err != nil {
		return planDigests{}, err
	}
	if run.ConfigDigest != "" && run.ConfigDigest != digests.Config {
		return planDigests{}, fmt.Errorf("run %s config digest drift detected", run.ID)
	}
	if run.WorkflowDigest != "" && run.WorkflowDigest != digests.Workflow {
		return planDigests{}, fmt.Errorf("run %s workflow digest drift detected", run.ID)
	}
	return digests, nil
}
