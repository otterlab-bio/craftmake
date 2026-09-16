package backend

import (
	"context"

	"github.com/otterlab-bio/craftmake/internal/metrics"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type Result struct {
	TaskResult *protocol.TaskResult
	Metrics    *metrics.TaskMetrics
	BackendID  string
	Raw        map[string]any
}

type TaskOutcome struct {
	Result *Result
	Err    error
}

type SubmissionState struct {
	State   string
	Reason  string
	Details map[string]any
}

type SubmissionRequest struct {
	SubmissionID      string
	Scope             string
	GroupKey          string
	RuntimeDirectory  string
	Resources         protocol.ResourceRequest
	WorkerResources   protocol.ResourceRequest
	WorkerMaxParallel int
	Manifests         []*protocol.TaskManifest
	OnStarted         func(string, map[string]any) error
	OnStateChanged    func(SubmissionState) error
}

type RecoveryRequest struct {
	SubmissionID     string
	BackendJobID     string
	RuntimeDirectory string
	Metadata         map[string]any
	Manifests        []*protocol.TaskManifest
}

type SubmissionResult struct {
	BackendID string
	Tasks     map[string]TaskOutcome
	Raw       map[string]any
}

type MetricsRefreshRequest struct {
	AttemptID        string
	RuntimeDirectory string
}

type MetricsRefresher interface {
	RefreshMetrics(context.Context, MetricsRefreshRequest) (*metrics.TaskMetrics, error)
}

type Recoverable interface {
	RecoverSubmission(context.Context, RecoveryRequest) (*SubmissionResult, error)
}

type Backend interface {
	Name() string
	RunSubmission(context.Context, string, SubmissionRequest) (*SubmissionResult, error)
	CancelSubmission(context.Context, string, map[string]any) error
	Cancel(context.Context) error
}
