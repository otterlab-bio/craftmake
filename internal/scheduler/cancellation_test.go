package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/compiler"
	"github.com/otterlab-bio/craftmake/internal/store"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type blockingBackend struct {
	started chan struct{}
	once    sync.Once
}

func (blockingBackend *blockingBackend) Name() string { return "blocking" }

func (blockingBackend *blockingBackend) RunSubmission(ctx context.Context, _ string, _ backend.SubmissionRequest) (*backend.SubmissionResult, error) {
	blockingBackend.once.Do(func() { close(blockingBackend.started) })
	<-ctx.Done()
	return &backend.SubmissionResult{BackendID: "blocking-job", Tasks: map[string]backend.TaskOutcome{}}, ctx.Err()
}

func (blockingBackend *blockingBackend) CancelSubmission(context.Context, string, map[string]any) error {
	return nil
}

func (blockingBackend *blockingBackend) Cancel(context.Context) error { return nil }

func TestSchedulerPersistsCancellationAfterContextCancellation(t *testing.T) {
	stateDirectory := t.TempDir()
	stateStore, err := store.Open(context.Background(), filepath.Join(stateDirectory, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	task := &compiler.Task{
		ID:          "workflow/phase/job",
		JobID:       "job",
		Workflow:    "workflow",
		Phase:       "phase",
		Scope:       "global",
		Dimensions:  map[string]string{},
		Inputs:      map[string][]string{},
		Outputs:     map[string]string{},
		Resources:   protocol.ResourceRequest{Cores: 1, MemoryByte: 64 << 20},
		Steps:       []protocol.StepManifest{{Index: 1, Name: "wait", Shell: "bash", Command: "sleep 30"}},
		MaxAttempts: 1,
	}
	plan := &compiler.Plan{
		Workflow: "workflow",
		Phase:    "phase",
		Tasks:    []*compiler.Task{task},
		TaskByID: map[string]*compiler.Task{task.ID: task},
		Order:    []string{task.ID},
		Submissions: []compiler.SubmissionGroup{{
			ID: "global-job", Scope: "global", TaskIDs: []string{task.ID}, Resources: task.Resources,
		}},
	}
	selectedBackend := &blockingBackend{started: make(chan struct{})}
	taskScheduler, err := New(plan, stateStore, Options{
		ProjectDirectory: stateDirectory,
		StateDirectory:   stateDirectory,
		ConfigPath:       filepath.Join(stateDirectory, "config.yaml"),
		WorkflowPath:     filepath.Join(stateDirectory, "workflow.yaml"),
		Backend:          selectedBackend,
		MaxParallel:      1,
		MaxCores:         1,
		MaxMemoryBytes:   64 << 20,
		RunID:            "cancelled-run",
	})
	if err != nil {
		t.Fatal(err)
	}

	runContext, cancelRun := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		_, runErr := taskScheduler.Run(runContext)
		runResult <- runErr
	}()
	select {
	case <-selectedBackend.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend submission")
	}
	cancelRun()

	select {
	case runErr := <-runResult:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not finish after cancellation")
	}

	run, counts, err := stateStore.RunSummary(context.Background(), "cancelled-run")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "cancelled" {
		t.Fatalf("expected cancelled run, got %q", run.Status)
	}
	if counts["cancelled"] != 1 {
		t.Fatalf("expected one cancelled task, got %#v", counts)
	}
	assertStoredStatus(t, stateStore, "physical_submissions", "cancelled", "cancelled-run")
	assertStoredStatus(t, stateStore, "task_attempts", "cancelled", "cancelled-run")
}

func assertStoredStatus(t *testing.T, stateStore *store.Store, tableName, expectedStatus, runID string) {
	t.Helper()
	query := "SELECT COUNT(*) FROM " + tableName + " WHERE run_id = ? AND status = ?"
	rows, err := stateStore.QueryRows(context.Background(), query, runID, expectedStatus)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no result for %s status query", tableName)
	}
	var count int
	if err := rows.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one cancelled %s, got %d", tableName, count)
	}
}
