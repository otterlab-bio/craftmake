ALTER TABLE runs ADD COLUMN resumed_from_run_id TEXT REFERENCES runs(run_id);

CREATE INDEX IF NOT EXISTS idx_runs_resumed_from
ON runs(resumed_from_run_id);
