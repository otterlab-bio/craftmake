package protocol

import (
	"encoding/json"
	"fmt"
	"io"
)

const CommandProtocolVersion = "otter.craftmake/v1"

type CommandEnvelope struct {
	ProtocolVersion string          `json:"protocol_version"`
	Command         string          `json:"command"`
	OK              bool            `json:"ok"`
	RunID           string          `json:"run_id,omitempty"`
	StatePath       string          `json:"state_path,omitempty"`
	ControllerLog   string          `json:"controller_log,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
}

type ValidatePayload struct {
	Workflow        string `json:"workflow"`
	Phase           string `json:"phase"`
	TaskCount       int    `json:"task_count"`
	SubmissionCount int    `json:"submission_count"`
}

type RunPayload struct {
	Backend string          `json:"backend"`
	Status  string          `json:"status"`
	DryRun  bool            `json:"dry_run,omitempty"`
	Plan    json.RawMessage `json:"plan,omitempty"`
}

type RecoveryPayload struct {
	RunID         string `json:"run_id"`
	FinalStatus   string `json:"final_status"`
	Succeeded     int    `json:"succeeded"`
	Failed        int    `json:"failed"`
	Cancelled     int    `json:"cancelled"`
	Interrupted   int    `json:"interrupted"`
	ControllerLog string `json:"controller_log"`
}

type ResumePayload struct {
	ResumedFrom string           `json:"resumed_from"`
	Backend     string           `json:"backend"`
	Status      string           `json:"status"`
	Recovery    *RecoveryPayload `json:"recovery,omitempty"`
}

type StatusCount struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

type CacheDecisionPayload struct {
	TaskID       string `json:"task_id"`
	Status       string `json:"status"`
	Decision     string `json:"decision"`
	ReasonCode   string `json:"reason_code"`
	ReasonDetail string `json:"reason_detail"`
}

type StatusPayload struct {
	Workflow       string                 `json:"workflow"`
	Phase          string                 `json:"phase"`
	Backend        string                 `json:"backend"`
	Status         string                 `json:"status"`
	Counts         []StatusCount          `json:"counts"`
	CacheDecisions []CacheDecisionPayload `json:"cache_decisions,omitempty"`
}

type LogPayload struct {
	Kind   string `json:"kind"`
	TaskID string `json:"task_id,omitempty"`
	Status string `json:"status,omitempty"`
	Path   string `json:"path"`
}

type LogsPayload struct {
	Logs []LogPayload `json:"logs"`
}

type MetricsRefreshPayload struct {
	Candidates  int `json:"candidates"`
	Refreshed   int `json:"refreshed"`
	Unavailable int `json:"unavailable"`
}

type ReportPayload struct {
	OutputDirectory string                 `json:"output_directory"`
	ReportFormat    string                 `json:"report_format"`
	MetricsRefresh  *MetricsRefreshPayload `json:"metrics_refresh,omitempty"`
}

type CancelFailurePayload struct {
	SubmissionID string `json:"submission_id"`
	Message      string `json:"message"`
}

type CancelPayload struct {
	SubmissionCount int                    `json:"submission_count"`
	CancelledCount  int                    `json:"cancelled_count"`
	FailureCount    int                    `json:"failure_count"`
	Partial         bool                   `json:"partial"`
	Failures        []CancelFailurePayload `json:"failures,omitempty"`
}

func NewCommandEnvelope(command string, ok bool, runID, statePath, controllerLog string, data json.RawMessage) CommandEnvelope {
	return CommandEnvelope{
		ProtocolVersion: CommandProtocolVersion,
		Command:         command,
		OK:              ok,
		RunID:           runID,
		StatePath:       statePath,
		ControllerLog:   controllerLog,
		Data:            data,
	}
}

func MarshalCommandEnvelope(envelope CommandEnvelope) ([]byte, error) {
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal command envelope: %w", err)
	}
	return encoded, nil
}

func WriteCommandEnvelope(writer io.Writer, envelope CommandEnvelope, indent bool) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if indent {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(envelope); err != nil {
		return fmt.Errorf("write command envelope: %w", err)
	}
	return nil
}
