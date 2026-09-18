package scheduler

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/store"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type lifecycleBackend struct {
	recordingBackend
	mutex  sync.Mutex
	begins int
	ends   int
}

func (b *lifecycleBackend) BeginRun(context.Context, backend.RunContext) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.begins++
	return nil
}
func (b *lifecycleBackend) EndRun(context.Context, backend.RunOutcome) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.ends++
	return nil
}

func TestSchedulerInvokesRunLifecycleOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	state, err := store.Open(ctx, filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	task := &compiler.Task{ID: "workflow/main/job", JobID: "job", Workflow: "workflow", Phase: "main", Scope: "global", Dimensions: map[string]string{}, Inputs: map[string][]string{}, Outputs: map[string]string{}, Resources: protocol.ResourceRequest{Cores: 1}, MaxAttempts: 1, Fingerprint: "job"}
	plan := &compiler.Plan{Workflow: "workflow", Phase: "main", Tasks: []*compiler.Task{task}, TaskByID: map[string]*compiler.Task{task.ID: task}, Order: []string{task.ID}, Submissions: []compiler.SubmissionGroup{{ID: task.ID, Scope: "global", TaskIDs: []string{task.ID}, Resources: protocol.ResourceRequest{Cores: 1}}}}
	selected := &lifecycleBackend{}
	runner, err := New(plan, state, Options{ProjectDirectory: dir, StateDirectory: dir, Backend: selected, MaxParallel: 1, RunID: "lifecycle-run"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	selected.mutex.Lock()
	defer selected.mutex.Unlock()
	if selected.begins != 1 || selected.ends != 1 {
		t.Fatalf("expected one lifecycle begin/end, got %d/%d", selected.begins, selected.ends)
	}
}

var _ backend.RunLifecycle = (*lifecycleBackend)(nil)
