ALTER TABLE task_instances ADD COLUMN definition_fingerprint TEXT;
ALTER TABLE task_instances ADD COLUMN dependency_fingerprint TEXT;
ALTER TABLE task_instances ADD COLUMN cache_decision TEXT;
ALTER TABLE task_instances ADD COLUMN cache_reason_code TEXT;
ALTER TABLE task_instances ADD COLUMN cache_reason_detail TEXT;

CREATE INDEX IF NOT EXISTS idx_tasks_cache_decision
ON task_instances(run_id, cache_decision, cache_reason_code);
