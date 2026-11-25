package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const interruptedTransactionHelperEnv = "CRAFTMAKE_STORE_INTERRUPTED_TX_HELPER"

func TestStoreSerializesConcurrentWritersAcrossHandles(t *testing.T) {
	const (
		writerCount       = 8
		transactionsEach  = 32
		totalTransactions = writerCount * transactionsEach
	)

	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	stores := make([]*Store, writerCount)
	for index := range stores {
		stateStore, err := Open(ctx, databasePath)
		if err != nil {
			t.Fatalf("open writer store %d: %v", index, err)
		}
		stores[index] = stateStore
		if index == 0 {
			if _, err := stateStore.database.ExecContext(ctx, `CREATE TABLE concurrency_probe (id TEXT PRIMARY KEY NOT NULL, writer INTEGER NOT NULL, sequence INTEGER NOT NULL)`); err != nil {
				t.Fatalf("create concurrency probe: %v", err)
			}
		}
	}
	t.Cleanup(func() {
		for _, stateStore := range stores {
			if stateStore != nil {
				_ = stateStore.Close()
			}
		}
	})

	start := make(chan struct{})
	errors := make(chan error, writerCount)
	var workers sync.WaitGroup
	for writerIndex, stateStore := range stores {
		workers.Add(1)
		go func(writerIndex int, stateStore *Store) {
			defer workers.Done()
			<-start
			for sequence := 0; sequence < transactionsEach; sequence++ {
				id := fmt.Sprintf("writer-%02d-transaction-%03d", writerIndex, sequence)
				err := stateStore.WithTransaction(ctx, func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, `INSERT INTO concurrency_probe(id, writer, sequence) VALUES(?, ?, ?)`, id, writerIndex, sequence); err != nil {
						return err
					}
					time.Sleep(5 * time.Millisecond)
					return nil
				})
				if err != nil {
					errors <- fmt.Errorf("writer %d transaction %d: %w", writerIndex, sequence, err)
					return
				}
			}
		}(writerIndex, stateStore)
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}

	if err := stores[0].Close(); err != nil {
		t.Fatalf("close writer store: %v", err)
	}
	stores[0] = nil

	var rowCount, distinctCount int
	if err := stores[1].database.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT id) FROM concurrency_probe`).Scan(&rowCount, &distinctCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != totalTransactions || distinctCount != totalTransactions {
		t.Fatalf("concurrent writes lost or duplicated rows: rows=%d distinct=%d want=%d", rowCount, distinctCount, totalTransactions)
	}
}

func TestStoreRecoversAfterWriterProcessIsKilled(t *testing.T) {
	if os.Getenv(interruptedTransactionHelperEnv) == "1" {
		runInterruptedTransactionHelper(t)
		return
	}

	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	readyPath := filepath.Join(t.TempDir(), "writer-ready")
	stateStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.database.ExecContext(t.Context(), `CREATE TABLE recovery_probe (id TEXT PRIMARY KEY NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.database.ExecContext(t.Context(), `INSERT INTO recovery_probe(id) VALUES('committed-before-interruption')`); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.Close(); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestStoreRecoversAfterWriterProcessIsKilled$")
	command.Env = append(os.Environ(),
		interruptedTransactionHelperEnv+"=1",
		"CRAFTMAKE_STORE_DATABASE="+databasePath,
		"CRAFTMAKE_STORE_READY="+readyPath,
	)
	outputPath := filepath.Join(t.TempDir(), "writer.log")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		output.Close()
		t.Fatal(err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			_ = output.Close()
			log, _ := os.ReadFile(outputPath)
			t.Fatalf("writer did not reach the open transaction: %s", log)
		}
		if _, err := os.Stat(outputPath); err == nil {
			if info, statErr := output.Stat(); statErr == nil && info.Size() > 0 {
				_ = command.Process.Kill()
				_ = command.Wait()
				_ = output.Close()
				log, _ := os.ReadFile(outputPath)
				t.Fatalf("writer exited before opening its transaction: %s", log)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := os.Stat(databasePath + "-journal"); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = output.Close()
		t.Fatalf("active transaction did not leave a rollback journal: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("writer process unexpectedly exited cleanly instead of being interrupted")
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatalf("reopen state database after interrupted transaction: %v", err)
	}
	defer recovered.Close()

	var journalMode string
	if err := recovered.database.QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "delete" {
		t.Fatalf("recovery changed journal mode: got %q", journalMode)
	}
	var integrity string
	if err := recovered.database.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("database integrity check failed after recovery: %q", integrity)
	}
	var committedCount, interruptedCount int
	if err := recovered.database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM recovery_probe WHERE id='committed-before-interruption'`).Scan(&committedCount); err != nil {
		t.Fatal(err)
	}
	if err := recovered.database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM recovery_probe WHERE id='uncommitted-before-interruption'`).Scan(&interruptedCount); err != nil {
		t.Fatal(err)
	}
	if committedCount != 1 || interruptedCount != 0 {
		t.Fatalf("unexpected post-recovery rows: committed=%d interrupted=%d", committedCount, interruptedCount)
	}
}

func runInterruptedTransactionHelper(t *testing.T) {
	databasePath := os.Getenv("CRAFTMAKE_STORE_DATABASE")
	readyPath := os.Getenv("CRAFTMAKE_STORE_READY")
	if databasePath == "" || readyPath == "" {
		t.Fatal("interrupted transaction helper requires database and ready paths")
	}
	stateStore, err := Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	err = stateStore.WithTransaction(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO recovery_probe(id) VALUES('uncommitted-before-interruption')`); err != nil {
			return err
		}
		if _, err := os.Stat(databasePath + "-journal"); err != nil {
			return fmt.Errorf("rollback journal not present during open transaction: %w", err)
		}
		if err := os.WriteFile(readyPath, []byte("transaction-open\n"), 0o600); err != nil {
			return err
		}
		select {}
	})
	if err != nil {
		t.Fatal(err)
	}
}
