package backend

import "context"

// RunContext contains run-scoped information needed by backends that allocate resources once per run.
type RunContext struct {
	RunID            string
	ProjectDirectory string
	StateDirectory   string
	Options          map[string]any
}

// RunOutcome describes the terminal state passed to a run-scoped backend cleanup hook.
type RunOutcome struct {
	RunID            string
	Status           string
	Err              error
	ProjectDirectory string
	StateDirectory   string
}

// RunLifecycle is optional. Backends implement it when they need run-scoped setup and teardown.
type RunLifecycle interface {
	BeginRun(context.Context, RunContext) error
	EndRun(context.Context, RunOutcome) error
}
