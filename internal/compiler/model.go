package compiler

import (
	"github.com/otterlab-bio/craftmake/internal/spec"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type Context struct {
	Raw       map[string]any    `json:"raw"`
	Workflow  WorkflowContext   `json:"workflow"`
	Execution ExecutionContext  `json:"execution"`
	Samples   []SampleContext   `json:"samples"`
	Species   []SpeciesContext  `json:"species"`
	Paths     map[string]string `json:"paths"`
}

type ExecutionContext struct {
	Slurm          SlurmExecutionContext               `json:"slurm,omitempty"`
	PhaseResources map[string]protocol.ResourceRequest `json:"phase_resources,omitempty"`
}

type SlurmExecutionContext struct {
	Partition   string `json:"partition,omitempty"`
	Account     string `json:"account,omitempty"`
	QOS         string `json:"qos,omitempty"`
	MaxJobs     int    `json:"max_jobs,omitempty"`
	DefaultTime string `json:"default_time,omitempty"`
	ScratchRoot string `json:"scratch_root,omitempty"`
}

type WorkflowContext struct {
	Mode             string   `json:"mode"`
	WorkflowName     string   `json:"workflow_name"`
	JobID            string   `json:"job_id"`
	UserID           string   `json:"user_id"`
	Executor         string   `json:"executor,omitempty"`
	Backend          string   `json:"backend,omitempty"`
	Toolchain        string   `json:"toolchain"`
	LegacyExtensions []string `json:"legacy_extensions,omitempty"`
	PDXMode          bool     `json:"pdx_mode"`
}

type SampleContext struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Read1    string `json:"read1"`
	Read2    string `json:"read2"`
	Adapter1 string `json:"adapter1"`
	Adapter2 string `json:"adapter2"`
}

type SpeciesContext struct {
	Name            string `json:"name"`
	Index           int    `json:"index"`
	Role            string `json:"role"`
	GenomeFasta     string `json:"genome_fasta"`
	GenomeIndex     string `json:"genome_index"`
	RNASeqGTF       string `json:"rnaseq_gtf"`
	RNASeqReference string `json:"rnaseq_reference"`
}

type Task struct {
	ID                  string                   `json:"id"`
	JobID               string                   `json:"job_id"`
	JobName             string                   `json:"job_name"`
	Workflow            string                   `json:"workflow"`
	Phase               string                   `json:"phase"`
	Scope               string                   `json:"scope"`
	Dimensions          map[string]string        `json:"dimensions"`
	Inputs              map[string][]string      `json:"inputs"`
	Outputs             map[string]string        `json:"outputs"`
	Resources           protocol.ResourceRequest `json:"resources"`
	Worker              *WorkerPlan              `json:"worker,omitempty"`
	Environment         string                   `json:"environment,omitempty"`
	Env                 map[string]string        `json:"env,omitempty"`
	Steps               []protocol.StepManifest  `json:"steps"`
	Dependencies        []string                 `json:"dependencies"`
	MaxAttempts         int                      `json:"max_attempts"`
	CompressSuccessLogs bool                     `json:"compress_success_logs"`
	Fingerprint         string                   `json:"fingerprint"`
}

type WorkerPlan struct {
	Resources   protocol.ResourceRequest `json:"resources"`
	MaxParallel int                      `json:"max_parallel"`
}

type SubmissionGroup struct {
	ID        string                   `json:"id"`
	Scope     string                   `json:"scope"`
	GroupKey  string                   `json:"group_key"`
	TaskIDs   []string                 `json:"task_ids"`
	Resources protocol.ResourceRequest `json:"resources"`
	Worker    *WorkerPlan              `json:"worker,omitempty"`
}

type Plan struct {
	Workflow    string             `json:"workflow"`
	Phase       string             `json:"phase"`
	Name        string             `json:"name"`
	Tasks       []*Task            `json:"tasks"`
	TaskByID    map[string]*Task   `json:"task_by_id"`
	Order       []string           `json:"order"`
	Submissions []SubmissionGroup  `json:"submissions"`
	Source      *spec.WorkflowSpec `json:"source"`
}
