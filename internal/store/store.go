package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/otterlab-bio/craftmake/migrations"
	_ "modernc.org/sqlite"
)

type Store struct {
	database *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	dataSourceName := path
	if path != ":memory:" {
		absolutePath, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve state database path: %w", err)
		}
		dataSourceName = "file:" + absolutePath
	}
	dataSourceName += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=foreign_keys(ON)&_pragma=mmap_size(0)"

	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	database.SetMaxOpenConns(1)
	if err := retryStateDatabaseBusy(ctx, func() error {
		return database.PingContext(ctx)
	}); err != nil {
		database.Close()
		return nil, fmt.Errorf("connect to state database: %w", err)
	}
	stateStore := &Store{database: database}
	if err := retryStateDatabaseBusy(ctx, func() error {
		return stateStore.initialize(ctx)
	}); err != nil {
		database.Close()
		return nil, err
	}
	return stateStore, nil
}

func retryStateDatabaseBusy(ctx context.Context, operation func() error) error {
	const retryWindow = 5 * time.Second
	const retryInterval = 25 * time.Millisecond

	deadline := time.Now().Add(retryWindow)
	for {
		err := operation()
		if err == nil || !isStateDatabaseBusy(err) || !time.Now().Before(deadline) {
			return err
		}
		retryTimer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			retryTimer.Stop()
			return ctx.Err()
		case <-retryTimer.C:
		}
	}
}

func isStateDatabaseBusy(err error) bool {
	if err == nil {
		return false
	}
	errorMessage := strings.ToLower(err.Error())
	return strings.Contains(errorMessage, "sqlite_busy") || strings.Contains(errorMessage, "database is locked")
}

func (stateStore *Store) Close() error {
	return stateStore.database.Close()
}

func (stateStore *Store) initialize(ctx context.Context) error {
	connection, err := stateStore.database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire schema migration connection: %w", err)
	}
	defer connection.Close()

	if _, err := connection.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin schema migration transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = connection.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()

	if _, err := connection.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("initialize schema migration table: %w", err)
	}

	appliedVersions, err := appliedMigrationVersions(ctx, connection)
	if err != nil {
		return err
	}
	if err := validateAppliedMigrationVersions(appliedVersions); err != nil {
		return err
	}

	appliedVersionSet := make(map[int]bool, len(appliedVersions))
	for _, appliedVersion := range appliedVersions {
		appliedVersionSet[appliedVersion] = true
	}
	for _, migration := range migrations.All() {
		if appliedVersionSet[migration.Version] {
			continue
		}
		if err := applyMigration(ctx, connection, migration); err != nil {
			return err
		}
	}
	if _, err := connection.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit schema migrations: %w", err)
	}
	committed = true
	return nil
}

func appliedMigrationVersions(ctx context.Context, connection *sql.Conn) ([]int, error) {
	rows, err := connection.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("list applied schema migrations: %w", err)
	}
	defer rows.Close()

	versions := []int{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan applied schema migration: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read applied schema migrations: %w", err)
	}
	return versions, nil
}

func validateAppliedMigrationVersions(appliedVersions []int) error {
	for versionIndex, appliedVersion := range appliedVersions {
		expectedVersion := versionIndex + 1
		if appliedVersion > migrations.CurrentVersion {
			return fmt.Errorf("state database schema version %d is newer than supported version %d", appliedVersion, migrations.CurrentVersion)
		}
		if appliedVersion != expectedVersion {
			return fmt.Errorf("state database schema migrations are not contiguous: expected version %d, found version %d", expectedVersion, appliedVersion)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, connection *sql.Conn, migration migrations.Migration) error {
	if _, err := connection.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("apply schema migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	if _, err := connection.ExecContext(ctx, `
		INSERT INTO schema_migrations(version, applied_at)
		VALUES(?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	`, migration.Version); err != nil {
		return fmt.Errorf("record schema migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	return nil
}

func (stateStore *Store) WithTransaction(ctx context.Context, operation func(*sql.Tx) error) error {
	transaction, err := stateStore.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer transaction.Rollback()
	if err := operation(transaction); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
