package protocol

import "time"

const Version = 2

type ResourceRequest struct {
	Cores      int    `json:"cores"`
	MemoryByte int64  `json:"memory_bytes"`
	Partition  string `json:"partition,omitempty"`
	Time       string `json:"time,omitempty"`
}

type StepManifest struct {
	Index       int               `json:"index"`
	Name        string            `json:"name"`
	Shell       string            `json:"shell"`
	Environment string            `json:"environment,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Command     string            `json:"command"`
	Logs        map[string]string `json:"logs,omitempty"`
	StdoutPath  string            `json:"stdout_path"`
	StderrPath  string            `json:"stderr_path"`
}

type TaskManifest struct {
	ProtocolVersion     int                 `json:"protocol_version"`
	RunID               string              `json:"run_id"`
	TaskID              string              `json:"task_id"`
	JobID               string              `json:"job_id"`
	Attempt             int                 `json:"attempt"`
	Workflow            string              `json:"workflow"`
	Phase               string              `json:"phase"`
	Scope               string              `json:"scope"`
	Dimensions          map[string]string   `json:"dimensions,omitempty"`
	Inputs              map[string][]string `json:"inputs,omitempty"`
	Outputs             map[string]string   `json:"outputs,omitempty"`
	Resources           ResourceRequest     `json:"resources"`
	WorkDirectory       string              `json:"work_directory"`
	TempDirectory       string              `json:"temp_directory"`
	RuntimeDirectory    string              `json:"runtime_directory"`
	ResultPath          string              `json:"result_path"`
	CompressSuccessLogs bool                `json:"compress_success_logs"`
	Steps               []StepManifest      `json:"steps"`
}

type StepResult struct {
	Index       int       `json:"index"`
	Name        string    `json:"name"`
	Environment string    `json:"environment,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	ExitCode    int       `json:"exit_code"`
	StdoutPath  string    `json:"stdout_path"`
	StderrPath  string    `json:"stderr_path"`
	Error       string    `json:"error,omitempty"`
}

type TaskResult struct {
	ProtocolVersion     int          `json:"protocol_version"`
	RunID               string       `json:"run_id"`
	TaskID              string       `json:"task_id"`
	Attempt             int          `json:"attempt"`
	Status              string       `json:"status"`
	StartedAt           time.Time    `json:"started_at"`
	FinishedAt          time.Time    `json:"finished_at"`
	ExitCode            int          `json:"exit_code"`
	Signal              string       `json:"signal,omitempty"`
	Error               string       `json:"error,omitempty"`
	Incident            *Incident    `json:"incident,omitempty"`
	Steps               []StepResult `json:"steps"`
	MissingOutputs      []string     `json:"missing_outputs,omitempty"`
	ObservabilityErrors []string     `json:"observability_errors,omitempty"`
}

type TaskEvent struct {
	ProtocolVersion int            `json:"protocol_version"`
	RunID           string         `json:"run_id"`
	TaskID          string         `json:"task_id,omitempty"`
	Type            string         `json:"type"`
	Timestamp       time.Time      `json:"timestamp"`
	Payload         map[string]any `json:"payload,omitempty"`
}
