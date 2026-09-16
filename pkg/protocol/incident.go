package protocol

import (
	"strings"
	"time"
)

const IncidentSchemaVersion = "otter.runtime-incident/v1"

type IncidentCategory string

const (
	IncidentInputReferenceDigest IncidentCategory = "input_reference_digest"
	IncidentEnvironmentTool      IncidentCategory = "environment_tool"
	IncidentSchedulerSubmission  IncidentCategory = "scheduler_submission"
	IncidentQueueTimeout         IncidentCategory = "queue_timeout"
	IncidentResourceExhaustion   IncidentCategory = "resource_exhaustion"
	IncidentFilesystemIO         IncidentCategory = "filesystem_io"
	IncidentNetworkAcquisition   IncidentCategory = "network_acquisition"
	IncidentToolInvocation       IncidentCategory = "tool_invocation"
	IncidentScientificQC         IncidentCategory = "scientific_qc"
	IncidentArtifactIntegrity    IncidentCategory = "artifact_integrity"
	IncidentCancellationRecovery IncidentCategory = "cancellation_recovery"
	IncidentInternalUnknown      IncidentCategory = "internal_unknown"
)

type IncidentScope string

const (
	IncidentScopeRun        IncidentScope = "run"
	IncidentScopeSubmission IncidentScope = "submission"
	IncidentScopeTask       IncidentScope = "task"
	IncidentScopeStep       IncidentScope = "step"
)

type IncidentRemediationStatus string

const (
	IncidentRemediationOpen      IncidentRemediationStatus = "open"
	IncidentRemediationMitigated IncidentRemediationStatus = "mitigated"
	IncidentRemediationResolved  IncidentRemediationStatus = "resolved"
	IncidentRemediationWaived    IncidentRemediationStatus = "waived"
)

type IncidentRetryPolicy string

const (
	IncidentRetryNever     IncidentRetryPolicy = "never"
	IncidentRetryAutomatic IncidentRetryPolicy = "automatic"
	IncidentRetryManual    IncidentRetryPolicy = "manual"
)

// Incident records a classified runtime failure without treating human-readable
// stderr as a stable machine interface. Diagnostic paths remain evidence only.
type Incident struct {
	SchemaVersion     string                    `json:"schema_version"`
	Category          IncidentCategory          `json:"category"`
	Scope             IncidentScope             `json:"scope"`
	RetrySafe         bool                      `json:"retry_safe"`
	RetryPolicy       IncidentRetryPolicy       `json:"retry_policy"`
	Owner             string                    `json:"owner"`
	Escalation        string                    `json:"escalation"`
	RemediationStatus IncidentRemediationStatus `json:"remediation_status"`
	Summary           string                    `json:"summary"`
	FirstObservedAt   time.Time                 `json:"first_observed_at"`
	Executor          string                    `json:"executor,omitempty"`
	Backend           string                    `json:"backend,omitempty"`
	BackendJobID      string                    `json:"backend_job_id,omitempty"`
	ExitCode          int                       `json:"exit_code,omitempty"`
	Signal            string                    `json:"signal,omitempty"`
	DiagnosticPaths   []string                  `json:"diagnostic_paths,omitempty"`
	EvidencePaths     []string                  `json:"evidence_paths,omitempty"`
}

type incidentPolicy struct {
	retrySafe bool
	retry     IncidentRetryPolicy
	owner     string
	escalate  string
}

func ClassifyTaskIncident(result TaskResult, backend string) *Incident {
	if result.Status == "succeeded" {
		return nil
	}

	category := classifyIncidentCategory(result)
	policy := incidentPolicyFor(category)
	firstObservedAt := result.FinishedAt
	if firstObservedAt.IsZero() {
		firstObservedAt = result.StartedAt
	}
	if firstObservedAt.IsZero() {
		firstObservedAt = time.Now().UTC()
	}

	return &Incident{
		SchemaVersion:     IncidentSchemaVersion,
		Category:          category,
		Scope:             incidentScopeFor(result),
		RetrySafe:         policy.retrySafe,
		RetryPolicy:       policy.retry,
		Owner:             policy.owner,
		Escalation:        policy.escalate,
		RemediationStatus: IncidentRemediationOpen,
		Summary:           incidentSummary(result),
		FirstObservedAt:   firstObservedAt.UTC(),
		Backend:           backend,
		ExitCode:          result.ExitCode,
		Signal:            result.Signal,
		DiagnosticPaths:   diagnosticPaths(result),
	}
}

func classifyIncidentCategory(result TaskResult) IncidentCategory {
	if len(result.MissingOutputs) > 0 {
		return IncidentArtifactIntegrity
	}

	diagnosticText := strings.ToLower(strings.Join(append(stepErrors(result.Steps), result.Error), "\n"))
	switch {
	case containsAny(diagnosticText, "digest", "checksum", "reference", "input missing", "no such input"):
		return IncidentInputReferenceDigest
	case containsAny(diagnosticText, "out_of_memory", "out of memory", "oom", "memory limit"):
		return IncidentResourceExhaustion
	case containsAny(diagnosticText, "pending timeout", "queue timeout", "time limit", "timed out"):
		return IncidentQueueTimeout
	case containsAny(diagnosticText, "qos", "partition", "account", "sbatch", "slurm controller", "submit job"):
		return IncidentSchedulerSubmission
	case containsAny(diagnosticText, "permission denied", "read-only file system", "no space left", "input/output error", "filesystem"):
		return IncidentFilesystemIO
	case containsAny(diagnosticText, "network", "connection", "http", "download", "resolve host"):
		return IncidentNetworkAcquisition
	case containsAny(diagnosticText, "cancelled", "canceled", "interrupted", "signal: terminated", "context canceled"):
		return IncidentCancellationRecovery
	case containsAny(diagnosticText, "environment", "enva", "conda", "executable file not found", "command not found"):
		return IncidentEnvironmentTool
	case containsAny(diagnosticText, "missing output", "artifact", "manifest", "checksum mismatch"):
		return IncidentArtifactIntegrity
	case containsAny(diagnosticText, "validation", "quality control", "qc", "scientific"):
		return IncidentScientificQC
	case diagnosticText != "":
		return IncidentToolInvocation
	default:
		return IncidentInternalUnknown
	}
}

func incidentPolicyFor(category IncidentCategory) incidentPolicy {
	switch category {
	case IncidentSchedulerSubmission, IncidentNetworkAcquisition:
		return incidentPolicy{retrySafe: true, retry: IncidentRetryAutomatic, owner: "platform", escalate: "Escalate after bounded automatic retries are exhausted."}
	case IncidentCancellationRecovery:
		return incidentPolicy{retrySafe: true, retry: IncidentRetryManual, owner: "workflow", escalate: "Require an explicit resume decision and verify cache/recovery evidence."}
	case IncidentToolInvocation:
		return incidentPolicy{retrySafe: true, retry: IncidentRetryManual, owner: "workflow", escalate: "Reproduce with retained logs before retrying after a configuration or tool change."}
	case IncidentResourceExhaustion:
		return incidentPolicy{retrySafe: false, retry: IncidentRetryManual, owner: "workflow", escalate: "Review accounting evidence and increase or rebalance resources in a new immutable run."}
	case IncidentInputReferenceDigest, IncidentEnvironmentTool, IncidentFilesystemIO, IncidentScientificQC, IncidentArtifactIntegrity:
		return incidentPolicy{retrySafe: false, retry: IncidentRetryNever, owner: "workflow", escalate: "Correct the contract or scientific validation failure, then resolve a new run."}
	default:
		return incidentPolicy{retrySafe: false, retry: IncidentRetryNever, owner: "platform", escalate: "Classify the incident before any retry or promotion decision."}
	}
}

func incidentScopeFor(result TaskResult) IncidentScope {
	if len(result.Steps) > 0 {
		return IncidentScopeStep
	}
	return IncidentScopeTask
}

func incidentSummary(result TaskResult) string {
	if result.Error != "" {
		return result.Error
	}
	if len(result.MissingOutputs) > 0 {
		return "declared outputs are missing"
	}
	return "task did not reach a successful terminal state"
}

func diagnosticPaths(result TaskResult) []string {
	paths := make([]string, 0, len(result.Steps)*2)
	for _, step := range result.Steps {
		if step.StdoutPath != "" {
			paths = append(paths, step.StdoutPath)
		}
		if step.StderrPath != "" {
			paths = append(paths, step.StderrPath)
		}
	}
	return paths
}

func stepErrors(steps []StepResult) []string {
	errors := make([]string, 0, len(steps))
	for _, step := range steps {
		if step.Error != "" {
			errors = append(errors, step.Error)
		}
	}
	return errors
}

func containsAny(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
