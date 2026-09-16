package processgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitCancelsEntireProcessGroup(t *testing.T) {
	temporaryDirectory := t.TempDir()
	startedPath := filepath.Join(temporaryDirectory, "started")
	completedPath := filepath.Join(temporaryDirectory, "completed")
	command := exec.Command("bash", "-c", "touch \"$1\"; (sleep 1; touch \"$2\") & wait", "bash", startedPath, completedPath)
	Configure(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, startedPath)

	commandContext, cancelCommand := context.WithCancel(context.Background())
	cancelCommand()
	waitErr := Wait(commandContext, command)
	if !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", waitErr)
	}

	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(completedPath); !os.IsNotExist(err) {
		t.Fatalf("process-group child survived cancellation: stat error=%v", err)
	}
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q", path)
}
