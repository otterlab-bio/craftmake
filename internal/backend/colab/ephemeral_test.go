package colab

import (
	"context"
	"testing"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

// fakeAccelControlPlane records the accelerator requested for each acquire.
type fakeAccelControlPlane struct {
	acquiredAccels []string
	released       int
}

func (f *fakeAccelControlPlane) AcquireRuntime(_ context.Context, req RuntimeRequest) (Runtime, error) {
	f.acquiredAccels = append(f.acquiredAccels, req.Accelerator)
	return Runtime{ID: "runtime-" + req.Accelerator}, nil
}
func (f *fakeAccelControlPlane) ReleaseRuntime(context.Context, Runtime) error {
	f.released++
	return nil
}
func (f *fakeAccelControlPlane) ListAssignments(context.Context) ([]Assignment, error) {
	return nil, nil
}

func TestRunSubmissionRoutesByAccelerator(t *testing.T) {
	control := &fakeAccelControlPlane{}
	b := &Backend{Control: control, Executor: fakeNotebookExecutor{}, Config: Config{RemoteRoot: "/content/craftmake", DefaultAccelerator: "cpu"}}
	if err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	cpuManifest := &protocol.TaskManifest{RunID: "run-1", TaskID: "cpu-task", Resources: protocol.ResourceRequest{Accelerator: "cpu"}, Steps: []protocol.StepManifest{{Index: 0, Name: "s", StdoutPath: "/tmp/o", StderrPath: "/tmp/e"}}}
	gpuManifest := &protocol.TaskManifest{RunID: "run-1", TaskID: "gpu-task", Resources: protocol.ResourceRequest{Accelerator: "gpu"}, Steps: []protocol.StepManifest{{Index: 0, Name: "s", StdoutPath: "/tmp/o", StderrPath: "/tmp/e"}}}
	if _, err := b.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{cpuManifest, gpuManifest}}); err != nil {
		t.Fatal(err)
	}
	if len(control.acquiredAccels) != 2 || control.acquiredAccels[0] != "cpu" || control.acquiredAccels[1] != "gpu" {
		t.Fatalf("expected cpu then gpu acquire, got %#v", control.acquiredAccels)
	}
	if control.released != 2 {
		t.Fatalf("expected 2 releases, got %d", control.released)
	}
}

func TestRunSubmissionDefaultAccelerator(t *testing.T) {
	control := &fakeAccelControlPlane{}
	b := &Backend{Control: control, Executor: fakeNotebookExecutor{}, Config: Config{RemoteRoot: "/content/craftmake", DefaultAccelerator: "gpu"}}
	if err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	manifest := &protocol.TaskManifest{RunID: "run-1", TaskID: "t", Steps: []protocol.StepManifest{{Index: 0, Name: "s", StdoutPath: "/tmp/o", StderrPath: "/tmp/e"}}}
	if _, err := b.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{manifest}}); err != nil {
		t.Fatal(err)
	}
	if len(control.acquiredAccels) != 1 || control.acquiredAccels[0] != "gpu" {
		t.Fatalf("expected default gpu acquire, got %#v", control.acquiredAccels)
	}
}

func TestRunSubmissionReleaseOnFailure(t *testing.T) {
	control := &fakeAccelControlPlane{}
	b := &Backend{Control: control, Executor: fakeNotebookExecutor{}, Config: Config{RemoteRoot: "/content/craftmake", DefaultAccelerator: "cpu"}}
	if err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	// A manifest with no steps will fail to build a notebook, but the runtime
	// must still be released.
	manifest := &protocol.TaskManifest{RunID: "run-1", TaskID: "t"}
	if _, err := b.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{manifest}}); err != nil {
		t.Fatal(err)
	}
	if control.released != 1 {
		t.Fatalf("expected runtime released even on failure, got %d", control.released)
	}
}

func TestBeginRunNoAssignment(t *testing.T) {
	control := &fakeAccelControlPlane{}
	b := &Backend{Control: control, Executor: fakeNotebookExecutor{}, Config: Config{RemoteRoot: "/content/craftmake"}}
	if err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	if len(control.acquiredAccels) != 0 {
		t.Fatalf("BeginRun must not acquire a runtime, got %#v", control.acquiredAccels)
	}
	if b.Config.DefaultAccelerator != "cpu" {
		t.Fatalf("default accelerator should default to cpu, got %q", b.Config.DefaultAccelerator)
	}
}

func TestEndRunDefensiveCleanup(t *testing.T) {
	control := &fakeAccelControlPlane{}
	b := &Backend{Control: control, Executor: fakeNotebookExecutor{}, Config: Config{RemoteRoot: "/content/craftmake"}}
	if err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	if err := b.EndRun(context.Background(), backendpkg.RunOutcome{RunID: "run-1", Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	// fakeAccelControlPlane.ListAssignments returns nil, so no residual release.
	if control.released != 0 {
		t.Fatalf("expected no residual release, got %d", control.released)
	}
}
func TestRunSubmissionConsecutiveSameType(t *testing.T) {
	control := &fakeAccelControlPlane{}
	b := &Backend{Control: control, Executor: fakeNotebookExecutor{}, Config: Config{RemoteRoot: "/content/craftmake", DefaultAccelerator: "cpu"}}
	if err := b.BeginRun(context.Background(), backendpkg.RunContext{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	m1 := &protocol.TaskManifest{RunID: "run-1", TaskID: "t1", Resources: protocol.ResourceRequest{Accelerator: "cpu"}, Steps: []protocol.StepManifest{{Index: 0, Name: "s", StdoutPath: "/tmp/o", StderrPath: "/tmp/e"}}}
	m2 := &protocol.TaskManifest{RunID: "run-1", TaskID: "t2", Resources: protocol.ResourceRequest{Accelerator: "cpu"}, Steps: []protocol.StepManifest{{Index: 0, Name: "s", StdoutPath: "/tmp/o", StderrPath: "/tmp/e"}}}
	if _, err := b.RunSubmission(context.Background(), "sub-1", backendpkg.SubmissionRequest{Manifests: []*protocol.TaskManifest{m1, m2}}); err != nil {
		t.Fatal(err)
	}
	// Two same-type manifests must each get an independent acquire/release
	// (no reuse, because Drive is the durable shared state).
	if len(control.acquiredAccels) != 2 || control.released != 2 {
		t.Fatalf("expected 2 independent acquires/releases, got %d/%d", len(control.acquiredAccels), control.released)
	}
}
