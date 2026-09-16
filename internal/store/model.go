package store

import (
	"encoding/json"
	"time"
)

type Run struct {
	ID               string
	Workflow         string
	Phase            string
	ConfigPath       string
	ConfigDigest     string
	WorkflowPath     string
	WorkflowDigest   string
	Backend          string
	CraftmakeVersion string
	ResumedFromRunID string
	LoaderKind       string
	Status           string
	StartedAt        time.Time
	FinishedAt       *time.Time
}

type CacheDecision struct {
	Hit        bool
	Decision   string
	ReasonCode string
	Detail     string
}

type TaskInstance struct {
	RunID                 string
	TaskID                string
	JobID                 string
	Dimensions            map[string]string
	Inputs                map[string][]string
	Outputs               map[string]string
	Fingerprint           string
	DefinitionFingerprint string
	DependencyFingerprint string
	CacheDecision         string
	CacheReasonCode       string
	CacheReasonDetail     string
	Status                string
}

type Dependency struct {
	RunID            string
	UpstreamTaskID   string
	DownstreamTaskID string
	Type             string
}

type Submission struct {
	ID           string
	RunID        string
	Backend      string
	Scope        string
	GroupKey     string
	BackendJobID string
	Resources    json.RawMessage
	Status       string
	StartedAt    *time.Time
	FinishedAt   *time.Time
	RawMetadata  json.RawMessage
}

type TaskAttempt struct {
	ID            string
	RunID         string
	TaskID        string
	AttemptNumber int
	SubmissionID  string
	Status        string
	StartedAt     *time.Time
	FinishedAt    *time.Time
	ExitCode      *int
	Signal        string
	FailureReason string
	StdoutPath    string
	StderrPath    string
	ResultPath    string
}

type StepAttempt struct {
	ID          string
	AttemptID   string
	Index       int
	Name        string
	Environment string
	StartedAt   *time.Time
	FinishedAt  *time.Time
	WallSeconds *float64
	ExitCode    *int
	StdoutPath  string
	StderrPath  string
}

type Artifact struct {
	ID               string
	AttemptID        string
	Role             string
	Name             string
	Path             string
	SizeBytes        *int64
	ModificationTime *time.Time
	Digest           string
	ValidationStatus string
}

type TaskMetrics struct {
	AttemptID                  string
	Source                     string
	Quality                    string
	WallSeconds                *float64
	UserCPUSeconds             *float64
	SystemCPUSeconds           *float64
	AllocatedCPUs              *int64
	RequestedMemoryBytes       *int64
	MaxRSSBytes                *int64
	DiskReadBytes              *int64
	DiskWriteBytes             *int64
	FilesystemInputOperations  *int64
	FilesystemOutputOperations *int64
	InputArtifactBytes         *int64
	OutputArtifactBytes        *int64
	MajorPageFaults            *int64
	MinorPageFaults            *int64
	VoluntaryContextSwitches   *int64
	InvoluntaryContextSwitches *int64
	RawMetrics                 json.RawMessage
}

type RuntimeIncident struct {
	ID                string
	RunID             string
	AttemptID         string
	SchemaVersion     string
	Category          string
	Scope             string
	RetrySafe         bool
	RetryPolicy       string
	Owner             string
	Escalation        string
	RemediationStatus string
	Summary           string
	FirstObservedAt   time.Time
	Executor          string
	Backend           string
	BackendJobID      string
	ExitCode          *int
	Signal            string
	DiagnosticPaths   []string
	EvidencePaths     []string
}

type MetricRefreshCandidate struct {
	AttemptID  string
	ResultPath string
}

type Event struct {
	RunID      string
	TaskID     string
	OccurredAt time.Time
	Type       string
	Payload    json.RawMessage
}
