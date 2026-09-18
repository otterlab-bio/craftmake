package engine

import (
	"context"
	"fmt"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/scheduler"
	"github.com/otterlab-bio/craftmake/internal/store"
)

type RunRequest struct {
	Plan             *compiler.Plan
	DatabasePath     string
	ProjectDirectory string
	StateDirectory   string
	ConfigPath       string
	ConfigDigest     string
	WorkflowPath     string
	WorkflowDigest   string
	Backend          backend.Backend
	MaxParallel      int
	MaxCores         int
	MaxMemoryBytes   int64
	Force            bool
	Version          string
	RunID            string
	LoaderKind       string
}

type RunResult struct {
	RunID               string
	DatabasePath        string
	ControllerLogPath   string
	ControllerLogErrors []error
}

func Run(ctx context.Context, request RunRequest) (RunResult, error) {
	if request.Plan == nil {
		return RunResult{}, fmt.Errorf("run plan is required")
	}
	if request.Backend == nil {
		return RunResult{}, fmt.Errorf("run backend is required")
	}
	state, err := store.Open(ctx, request.DatabasePath)
	if err != nil {
		return RunResult{}, err
	}
	defer state.Close()
	taskScheduler, err := scheduler.New(request.Plan, state, scheduler.Options{ProjectDirectory: request.ProjectDirectory, StateDirectory: request.StateDirectory, ConfigPath: request.ConfigPath, ConfigDigest: request.ConfigDigest, WorkflowPath: request.WorkflowPath, WorkflowDigest: request.WorkflowDigest, Backend: request.Backend, MaxParallel: request.MaxParallel, MaxCores: request.MaxCores, MaxMemoryBytes: request.MaxMemoryBytes, Force: request.Force, Version: request.Version, RunID: request.RunID, LoaderKind: request.LoaderKind})
	if err != nil {
		return RunResult{}, err
	}
	runID, runErr := taskScheduler.Run(ctx)
	return RunResult{RunID: runID, DatabasePath: request.DatabasePath, ControllerLogPath: taskScheduler.ControllerLogPath(), ControllerLogErrors: taskScheduler.ControllerLogErrors()}, runErr
}
