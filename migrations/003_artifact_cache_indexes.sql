CREATE INDEX IF NOT EXISTS idx_attempts_task_status_finished ON task_attempts(run_id, task_id, status, finished_at);
CREATE INDEX IF NOT EXISTS idx_artifacts_attempt_role ON artifacts(attempt_id, role);
