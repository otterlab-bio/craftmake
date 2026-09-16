package otter

const runSchemaVersion = "otter.run/v1"

type runSnapshot struct {
	SchemaVersion string              `yaml:"schema_version"`
	Run           runMetadata         `yaml:"run"`
	Project       resolvedProject     `yaml:"project"`
	Workflow      resolvedWorkflow    `yaml:"workflow"`
	Execution     resolvedExecution   `yaml:"execution"`
	Samples       []sampleRecord      `yaml:"samples"`
	References    resolvedReferences  `yaml:"references"`
	Paths         runPaths            `yaml:"paths"`
	Digests       runDigests          `yaml:"digests"`
	Observability observabilityConfig `yaml:"observability,omitempty"`
	Parity        parityConfig        `yaml:"parity,omitempty"`
}

type runMetadata struct {
	ID          string `yaml:"id"`
	CreatedAt   string `yaml:"created_at"`
	Immutable   bool   `yaml:"immutable"`
	ParentRunID string `yaml:"parent_run_id,omitempty"`
}

type resolvedProject struct {
	ID   string `yaml:"id"`
	Root string `yaml:"root"`
}

type resolvedWorkflow struct {
	Scenario         string   `yaml:"scenario"`
	Toolchain        string   `yaml:"toolchain"`
	LegacyExtensions []string `yaml:"legacy_extensions,omitempty"`
	AssetRoot        string   `yaml:"asset_root"`
}

type resolvedExecution struct {
	Executor  resolvedExecutor       `yaml:"executor"`
	Backend   resolvedBackend        `yaml:"backend"`
	Site      resolvedString         `yaml:"site"`
	Resources projectResources       `yaml:"resources,omitempty"`
	Slurm     resolvedSlurmResources `yaml:"slurm,omitempty"`
}

type resolvedExecutor struct {
	Value  string `yaml:"value"`
	Source string `yaml:"source"`
}

type resolvedBackend struct {
	Value    string          `yaml:"value"`
	Source   string          `yaml:"source"`
	Evidence backendEvidence `yaml:"evidence,omitempty"`
}

type backendEvidence struct {
	Cluster  string   `yaml:"cluster,omitempty"`
	Commands []string `yaml:"commands,omitempty"`
	Reason   string   `yaml:"reason,omitempty"`
}

type resolvedString struct {
	Value  string `yaml:"value"`
	Source string `yaml:"source"`
}

type resolvedInt struct {
	Value  int    `yaml:"value"`
	Source string `yaml:"source"`
}

type resolvedSlurmResources struct {
	Partition   resolvedString `yaml:"partition"`
	Account     resolvedString `yaml:"account"`
	QOS         resolvedString `yaml:"qos"`
	MaxJobs     resolvedInt    `yaml:"max_jobs"`
	DefaultTime resolvedString `yaml:"default_time"`
	ScratchRoot resolvedString `yaml:"scratch_root"`
}

type projectResources struct {
	Defaults resourceSpec            `yaml:"defaults,omitempty"`
	Phases   map[string]resourceSpec `yaml:"phases,omitempty"`
}

type resourceSpec struct {
	Cores     int    `yaml:"cores,omitempty"`
	Memory    string `yaml:"memory,omitempty"`
	Time      string `yaml:"time,omitempty"`
	Partition string `yaml:"partition,omitempty"`
}

type sampleRecord struct {
	ID        string `yaml:"id"`
	R1        string `yaml:"r1"`
	R2        string `yaml:"r2"`
	Group     string `yaml:"group,omitempty"`
	Batch     string `yaml:"batch,omitempty"`
	AdapterR1 string `yaml:"adapter_r1,omitempty"`
	AdapterR2 string `yaml:"adapter_r2,omitempty"`
	R1SHA256  string `yaml:"r1_sha256,omitempty"`
	R1Size    int64  `yaml:"r1_size_bytes,omitempty"`
	R2SHA256  string `yaml:"r2_sha256,omitempty"`
	R2Size    int64  `yaml:"r2_size_bytes,omitempty"`
}

type resolvedReferences struct {
	ProjectSelection   referenceSelections `yaml:"project_selection,omitempty"`
	EffectiveSelection referenceSelections `yaml:"effective_selection,omitempty"`
	Override           bool                `yaml:"override,omitempty"`
	OverrideSource     string              `yaml:"override_source,omitempty"`
	Resolved           []resolvedReference `yaml:"resolved"`
}

type referenceSelections struct {
	Primary   string `yaml:"primary,omitempty"`
	Secondary string `yaml:"secondary,omitempty"`
	Graft     string `yaml:"graft,omitempty"`
	Host      string `yaml:"host,omitempty"`
}

type resolvedReference struct {
	Role           string          `yaml:"role"`
	ID             string          `yaml:"id"`
	Release        string          `yaml:"release"`
	Organism       string          `yaml:"organism,omitempty"`
	RegistryRoot   string          `yaml:"registry_root"`
	ManifestDigest string          `yaml:"manifest_digest"`
	Fasta          resolvedAsset   `yaml:"fasta"`
	Annotations    []resolvedAsset `yaml:"annotations,omitempty"`
	Indexes        []resolvedAsset `yaml:"indexes"`
}

type resolvedAsset struct {
	Type   string `yaml:"type"`
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

type runPaths struct {
	RunRoot         string   `yaml:"run_root"`
	Work            string   `yaml:"work"`
	Results         string   `yaml:"results"`
	Logs            string   `yaml:"logs"`
	State           string   `yaml:"state"`
	Metrics         string   `yaml:"metrics"`
	ProjectConfig   string   `yaml:"project_config,omitempty"`
	SamplesManifest string   `yaml:"samples_manifest,omitempty"`
	ReferencesLock  string   `yaml:"references_lock,omitempty"`
	WorkflowAssets  []string `yaml:"workflow_assets,omitempty"`
}

type runDigests struct {
	Project        string `yaml:"project"`
	Samples        string `yaml:"samples"`
	WorkflowAssets string `yaml:"workflow_assets"`
	References     string `yaml:"references,omitempty"`
}

type observabilityConfig struct {
	Metrics    bool `yaml:"metrics,omitempty"`
	RetainLogs bool `yaml:"retain_logs,omitempty"`
}

type parityConfig struct {
	Policy string `yaml:"policy,omitempty"`
}
