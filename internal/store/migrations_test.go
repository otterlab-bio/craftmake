package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/migrations"
	_ "modernc.org/sqlite"
)

func TestOpenInitializesCurrentSchemaVersion(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	stateStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	assertMigrationVersions(t, stateStore.database, []int{1, 2, 3, 4, 5, 6, 7})
	for _, indexName := range []string{"idx_submissions_run_status", "idx_attempts_submission", "idx_attempts_task_status_finished", "idx_artifacts_attempt_role", "idx_runs_resumed_from", "idx_tasks_cache_decision", "idx_runtime_incidents_run", "idx_runtime_incidents_category"} {
		if !databaseObjectExists(t, stateStore.database, "index", indexName) {
			t.Fatalf("expected current schema index %q", indexName)
		}
	}
}

func TestOpenUsesRollbackJournalWithoutMemoryMapping(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	stateStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	var journalMode string
	if err := stateStore.database.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "delete" {
		t.Fatalf("expected rollback journal mode, got %q", journalMode)
	}

	var memoryMapSize int64
	if err := stateStore.database.QueryRow(`PRAGMA mmap_size`).Scan(&memoryMapSize); err != nil {
		t.Fatal(err)
	}
	if memoryMapSize != 0 {
		t.Fatalf("expected SQLite memory mapping to be disabled, got %d bytes", memoryMapSize)
	}
}

func TestOpenUpgradesVersionOneDatabaseWithoutLosingData(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	legacyDatabase := openRawSQLite(t, databasePath)
	if _, err := legacyDatabase.Exec(migrations.InitialSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDatabase.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(1, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDatabase.Exec(`
		INSERT INTO runs(run_id, workflow, phase, backend, status, started_at)
		VALUES('legacy-run', 'BeaverBS', 'step1', 'local', 'succeeded', '2026-01-01T00:00:00Z')
	`); err != nil {
		t.Fatal(err)
	}
	if err := legacyDatabase.Close(); err != nil {
		t.Fatal(err)
	}

	stateStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()

	assertMigrationVersions(t, stateStore.database, []int{1, 2, 3, 4, 5, 6, 7})
	var workflowName string
	if err := stateStore.database.QueryRow(`SELECT workflow FROM runs WHERE run_id='legacy-run'`).Scan(&workflowName); err != nil {
		t.Fatal(err)
	}
	if workflowName != "BeaverBS" {
		t.Fatalf("unexpected preserved legacy run workflow %q", workflowName)
	}
}

func TestOpenUpgradesVersionTwoDatabaseWithoutLosingArtifactData(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	legacyDatabase := openRawSQLite(t, databasePath)
	if _, err := legacyDatabase.Exec(migrations.InitialSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDatabase.Exec(`CREATE INDEX IF NOT EXISTS idx_submissions_run_status ON physical_submissions(run_id, status)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDatabase.Exec(`CREATE INDEX IF NOT EXISTS idx_attempts_submission ON task_attempts(submission_id)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDatabase.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(1, '2026-01-01T00:00:00Z'), (2, '2026-01-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDatabase.Exec(`
		INSERT INTO runs(run_id, workflow, phase, backend, status, started_at)
		VALUES('artifact-run', 'BeaverRNA', 'step2', 'local', 'succeeded', '2026-01-01T00:00:00Z');
		INSERT INTO task_instances(task_id, run_id, job_id, dimensions_json, status)
		VALUES('task-a', 'artifact-run', 'job-a', '{}', 'succeeded');
		INSERT INTO task_attempts(attempt_id, run_id, task_id, attempt_number, status, started_at, finished_at)
		VALUES('attempt-a', 'artifact-run', 'task-a', 1, 'succeeded', '2026-01-01T00:00:00Z', '2026-01-01T00:00:01Z');
		INSERT INTO artifacts(artifact_id, attempt_id, role, name, path, size_bytes, mtime_ns, validation_status)
		VALUES('artifact-a', 'attempt-a', 'output', 'result', '/tmp/result.txt', 42, 123456789, 'valid');
	`); err != nil {
		t.Fatal(err)
	}
	if err := legacyDatabase.Close(); err != nil {
		t.Fatal(err)
	}

	stateStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	assertMigrationVersions(t, stateStore.database, []int{1, 2, 3, 4, 5, 6, 7})

	var sizeBytes, modificationTime int64
	var validationStatus string
	if err := stateStore.database.QueryRow(`SELECT size_bytes, mtime_ns, validation_status FROM artifacts WHERE artifact_id='artifact-a'`).Scan(&sizeBytes, &modificationTime, &validationStatus); err != nil {
		t.Fatal(err)
	}
	if sizeBytes != 42 || modificationTime != 123456789 || validationStatus != "valid" {
		t.Fatalf("unexpected preserved artifact: size=%d mtime=%d status=%q", sizeBytes, modificationTime, validationStatus)
	}
	for _, indexName := range []string{"idx_attempts_task_status_finished", "idx_artifacts_attempt_role"} {
		if !databaseObjectExists(t, stateStore.database, "index", indexName) {
			t.Fatalf("expected migrated index %q", indexName)
		}
	}
}

func TestOpenUpgradesVersionThreeDatabaseAndSupportsResumeLineage(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	legacyDatabase := openRawSQLite(t, databasePath)
	if _, err := legacyDatabase.Exec(migrations.InitialSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDatabase.Exec(`
		CREATE INDEX IF NOT EXISTS idx_submissions_run_status ON physical_submissions(run_id, status);
		CREATE INDEX IF NOT EXISTS idx_attempts_submission ON task_attempts(submission_id);
		CREATE INDEX IF NOT EXISTS idx_attempts_task_status_finished ON task_attempts(run_id, task_id, status, finished_at);
		CREATE INDEX IF NOT EXISTS idx_artifacts_attempt_role ON artifacts(attempt_id, role);
		INSERT INTO schema_migrations(version, applied_at)
		VALUES
			(1, '2026-01-01T00:00:00Z'),
			(2, '2026-01-02T00:00:00Z'),
			(3, '2026-01-03T00:00:00Z');
		INSERT INTO runs(run_id, workflow, phase, backend, status, started_at)
		VALUES('source-run', 'BeaverBS', 'step1', 'local', 'failed', '2026-01-01T00:00:00Z');
	`); err != nil {
		t.Fatal(err)
	}
	if err := legacyDatabase.Close(); err != nil {
		t.Fatal(err)
	}

	stateStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	assertMigrationVersions(t, stateStore.database, []int{1, 2, 3, 4, 5, 6, 7})

	if err := stateStore.CreateRun(t.Context(), Run{
		ID:               "resumed-run",
		Workflow:         "BeaverBS",
		Phase:            "step1",
		Backend:          "local",
		CraftmakeVersion: "test",
		ResumedFromRunID: "source-run",
		Status:           "running",
		StartedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	resumedRun, _, err := stateStore.RunSummary(t.Context(), "resumed-run")
	if err != nil {
		t.Fatal(err)
	}
	if resumedRun.ResumedFromRunID != "source-run" {
		t.Fatalf("unexpected resume lineage %q", resumedRun.ResumedFromRunID)
	}
	if !databaseObjectExists(t, stateStore.database, "index", "idx_runs_resumed_from") {
		t.Fatal("expected resume lineage index")
	}
}

func TestOpenUpgradesVersionFourDatabaseWithCacheDecisionColumns(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	legacyDatabase := openRawSQLite(t, databasePath)
	if _, err := legacyDatabase.Exec(migrations.InitialSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDatabase.Exec(`
		CREATE INDEX IF NOT EXISTS idx_submissions_run_status ON physical_submissions(run_id, status);
		CREATE INDEX IF NOT EXISTS idx_attempts_submission ON task_attempts(submission_id);
		CREATE INDEX IF NOT EXISTS idx_attempts_task_status_finished ON task_attempts(run_id, task_id, status, finished_at);
		CREATE INDEX IF NOT EXISTS idx_artifacts_attempt_role ON artifacts(attempt_id, role);
		ALTER TABLE runs ADD COLUMN resumed_from_run_id TEXT REFERENCES runs(run_id);
		CREATE INDEX IF NOT EXISTS idx_runs_resumed_from ON runs(resumed_from_run_id);
		INSERT INTO schema_migrations(version, applied_at)
		VALUES
			(1, '2026-01-01T00:00:00Z'),
			(2, '2026-01-02T00:00:00Z'),
			(3, '2026-01-03T00:00:00Z'),
			(4, '2026-01-04T00:00:00Z');
		INSERT INTO runs(run_id, workflow, phase, backend, status, started_at)
		VALUES('cache-run', 'BeaverBS', 'step1', 'local', 'running', '2026-01-04T00:00:00Z');
		INSERT INTO task_instances(task_id, run_id, job_id, dimensions_json, status)
		VALUES('cache-task', 'cache-run', 'cache-job', '{}', 'pending');
	`); err != nil {
		t.Fatal(err)
	}
	if err := legacyDatabase.Close(); err != nil {
		t.Fatal(err)
	}

	stateStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	assertMigrationVersions(t, stateStore.database, []int{1, 2, 3, 4, 5, 6, 7})

	if err := stateStore.UpsertTask(t.Context(), TaskInstance{
		RunID:                 "cache-run",
		TaskID:                "cache-task",
		JobID:                 "cache-job",
		Dimensions:            map[string]string{},
		Inputs:                map[string][]string{},
		Outputs:               map[string]string{},
		Fingerprint:           "runtime-fingerprint",
		DefinitionFingerprint: "definition-fingerprint",
		DependencyFingerprint: "dependency-fingerprint",
		CacheDecision:         "miss",
		CacheReasonCode:       "no_prior_success",
		CacheReasonDetail:     "no prior successful attempt exists for this task",
		Status:                "pending",
	}); err != nil {
		t.Fatal(err)
	}

	var cacheDecision string
	var reasonCode string
	if err := stateStore.database.QueryRow(`
		SELECT cache_decision, cache_reason_code
		FROM task_instances
		WHERE run_id='cache-run' AND task_id='cache-task'
	`).Scan(&cacheDecision, &reasonCode); err != nil {
		t.Fatal(err)
	}
	if cacheDecision != "miss" || reasonCode != "no_prior_success" {
		t.Fatalf("unexpected migrated cache decision: decision=%q reason=%q", cacheDecision, reasonCode)
	}
	if !databaseObjectExists(t, stateStore.database, "index", "idx_tasks_cache_decision") {
		t.Fatal("expected cache decision index")
	}
}

func TestOpenIsIdempotentAtCurrentSchemaVersion(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	firstStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}

	secondStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	assertMigrationVersions(t, secondStore.database, []int{1, 2, 3, 4, 5, 6, 7})

	var migrationCount int
	if err := secondStore.database.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != migrations.CurrentVersion {
		t.Fatalf("expected %d migration records after repeated open, got %d", migrations.CurrentVersion, migrationCount)
	}
}

func TestOpenSerializesConcurrentInitialMigration(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	const concurrentOpenCount = 8
	startOpening := make(chan struct{})
	openErrors := make(chan error, concurrentOpenCount)
	var openWorkers sync.WaitGroup

	for workerIndex := 0; workerIndex < concurrentOpenCount; workerIndex++ {
		openWorkers.Add(1)
		go func() {
			defer openWorkers.Done()
			<-startOpening
			stateStore, err := Open(context.Background(), databasePath)
			if err != nil {
				openErrors <- err
				return
			}
			openErrors <- stateStore.Close()
		}()
	}
	close(startOpening)
	openWorkers.Wait()
	close(openErrors)

	for openErr := range openErrors {
		if openErr != nil {
			t.Fatalf("concurrent state database open failed: %v", openErr)
		}
	}
	stateStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	assertMigrationVersions(t, stateStore.database, []int{1, 2, 3, 4, 5, 6, 7})
}

func TestOpenRejectsDatabaseFromNewerCraftmakeVersion(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	database := openRawSQLite(t, databasePath)
	if _, err := database.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= migrations.CurrentVersion+1; version++ {
		if _, err := database.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, '2026-01-01T00:00:00Z')`, version); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := Open(t.Context(), databasePath)
	if err == nil || !strings.Contains(err.Error(), "newer than supported version") {
		t.Fatalf("expected future schema rejection, got %v", err)
	}
}

func TestOpenRejectsMigrationVersionGap(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	database := openRawSQLite(t, databasePath)
	if _, err := database.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(2, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := Open(t.Context(), databasePath)
	if err == nil || !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("expected migration gap rejection, got %v", err)
	}
}

func openRawSQLite(t *testing.T, databasePath string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+databasePath+"?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.PingContext(context.Background()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	return database
}

func assertMigrationVersions(t *testing.T, database *sql.DB, expectedVersions []int) {
	t.Helper()
	rows, err := database.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actualVersions := []int{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		actualVersions = append(actualVersions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(actualVersions) != len(expectedVersions) {
		t.Fatalf("unexpected migration versions: got %v, expected %v", actualVersions, expectedVersions)
	}
	for versionIndex, expectedVersion := range expectedVersions {
		if actualVersions[versionIndex] != expectedVersion {
			t.Fatalf("unexpected migration versions: got %v, expected %v", actualVersions, expectedVersions)
		}
	}
}

func databaseObjectExists(t *testing.T, database *sql.DB, objectType, objectName string) bool {
	t.Helper()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type=? AND name=?`, objectType, objectName).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}
