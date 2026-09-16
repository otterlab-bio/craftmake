CREATE INDEX IF NOT EXISTS idx_submissions_run_status ON physical_submissions(run_id, status);
CREATE INDEX IF NOT EXISTS idx_attempts_submission ON task_attempts(submission_id);
