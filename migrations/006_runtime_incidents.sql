CREATE TABLE runtime_incidents (
  incident_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  attempt_id TEXT NOT NULL REFERENCES task_attempts(attempt_id) ON DELETE CASCADE,
  schema_version TEXT NOT NULL,
  category TEXT NOT NULL,
  scope TEXT NOT NULL,
  retry_safe INTEGER NOT NULL,
  retry_policy TEXT NOT NULL,
  owner TEXT NOT NULL,
  escalation TEXT NOT NULL,
  remediation_status TEXT NOT NULL,
  summary TEXT NOT NULL,
  first_observed_at TEXT NOT NULL,
  executor TEXT,
  backend TEXT,
  backend_job_id TEXT,
  exit_code INTEGER,
  signal TEXT,
  diagnostic_paths_json TEXT,
  evidence_paths_json TEXT
);

CREATE INDEX idx_runtime_incidents_run
ON runtime_incidents(run_id, first_observed_at);

CREATE INDEX idx_runtime_incidents_category
ON runtime_incidents(run_id, category, remediation_status);
