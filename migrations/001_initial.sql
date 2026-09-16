CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS runs (
  run_id TEXT PRIMARY KEY, workflow TEXT NOT NULL, phase TEXT NOT NULL, config_path TEXT, config_digest TEXT,
  workflow_path TEXT, workflow_digest TEXT, backend TEXT NOT NULL, craftmake_version TEXT, status TEXT NOT NULL,
  started_at TEXT NOT NULL, finished_at TEXT
);
CREATE TABLE IF NOT EXISTS task_instances (
  task_id TEXT NOT NULL, run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE, job_id TEXT NOT NULL,
  dimensions_json TEXT NOT NULL, inputs_json TEXT, outputs_json TEXT, fingerprint TEXT, status TEXT NOT NULL,
  PRIMARY KEY(run_id, task_id)
);
CREATE TABLE IF NOT EXISTS dependencies (
  run_id TEXT NOT NULL, upstream_task_id TEXT NOT NULL, downstream_task_id TEXT NOT NULL, dependency_type TEXT NOT NULL,
  PRIMARY KEY(run_id, upstream_task_id, downstream_task_id), FOREIGN KEY(run_id) REFERENCES runs(run_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS physical_submissions (
  submission_id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE, backend TEXT NOT NULL,
  scope TEXT NOT NULL, group_key TEXT, backend_job_id TEXT, resources_json TEXT, status TEXT NOT NULL,
  started_at TEXT, finished_at TEXT, raw_metadata_json TEXT
);
CREATE TABLE IF NOT EXISTS task_attempts (
  attempt_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, task_id TEXT NOT NULL, attempt_number INTEGER NOT NULL,
  submission_id TEXT, status TEXT NOT NULL, started_at TEXT, finished_at TEXT, exit_code INTEGER, signal TEXT,
  failure_reason TEXT, stdout_path TEXT, stderr_path TEXT, result_path TEXT,
  FOREIGN KEY(run_id, task_id) REFERENCES task_instances(run_id, task_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS step_attempts (
  step_attempt_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL REFERENCES task_attempts(attempt_id) ON DELETE CASCADE,
  step_index INTEGER NOT NULL, step_name TEXT NOT NULL, environment TEXT, started_at TEXT, finished_at TEXT,
  wall_seconds REAL, exit_code INTEGER, stdout_path TEXT, stderr_path TEXT
);
CREATE TABLE IF NOT EXISTS artifacts (
  artifact_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL REFERENCES task_attempts(attempt_id) ON DELETE CASCADE,
  role TEXT NOT NULL, name TEXT NOT NULL, path TEXT NOT NULL, size_bytes INTEGER, mtime_ns INTEGER, digest TEXT,
  validation_status TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS task_metrics (
  attempt_id TEXT PRIMARY KEY REFERENCES task_attempts(attempt_id) ON DELETE CASCADE, source TEXT, quality TEXT,
  wall_seconds REAL, user_cpu_seconds REAL, system_cpu_seconds REAL, allocated_cpus INTEGER,
  requested_memory_bytes INTEGER, max_rss_bytes INTEGER, disk_read_bytes INTEGER, disk_write_bytes INTEGER,
  filesystem_input_operations INTEGER, filesystem_output_operations INTEGER, input_artifact_bytes INTEGER,
  output_artifact_bytes INTEGER, major_page_faults INTEGER, minor_page_faults INTEGER,
  voluntary_context_switches INTEGER, involuntary_context_switches INTEGER, raw_metrics_json TEXT
);
CREATE TABLE IF NOT EXISTS events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  task_id TEXT, occurred_at TEXT NOT NULL, event_type TEXT NOT NULL, payload_json TEXT
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON task_instances(run_id, status);
CREATE INDEX IF NOT EXISTS idx_attempts_task ON task_attempts(run_id, task_id);
CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id, occurred_at);
