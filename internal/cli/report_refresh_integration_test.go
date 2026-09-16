package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otterlab-bio/craftmake/internal/metrics"
	"github.com/otterlab-bio/craftmake/internal/store"
)

func TestReportRefreshMetricsUpdatesAvailableSlurmAccountingAndWarnsForUnavailableAttempts(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	binaryPath := filepath.Join(temporaryDirectory, "craftmake")
	buildCraftmakeBinary(t, repositoryRoot, binaryPath)

	stateDirectory := filepath.Join(temporaryDirectory, "state")
	if err := os.MkdirAll(stateDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(stateDirectory, "state.sqlite")
	stateStore, err := store.Open(t.Context(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "slurm-metrics-refresh-run"
	startedAt := time.Now().UTC().Add(-time.Minute)
	finishedAt := time.Now().UTC()
	if err := stateStore.CreateRun(t.Context(), store.Run{
		ID:               runID,
		Workflow:         "BeaverBS",
		Phase:            "step1",
		Backend:          "slurm",
		CraftmakeVersion: "test",
		Status:           "succeeded",
		StartedAt:        startedAt,
	}); err != nil {
		t.Fatal(err)
	}

	for _, attemptFixture := range []struct {
		taskID      string
		attemptID   string
		stepID      string
		writeStepID bool
	}{
		{taskID: "workflow/phase/task-refreshable", attemptID: "attempt-refreshable", stepID: "12345.7", writeStepID: true},
		{taskID: "workflow/phase/task-unavailable", attemptID: "attempt-unavailable", stepID: "12345.8", writeStepID: false},
	} {
		runtimeDirectory := filepath.Join(stateDirectory, "runs", runID, "tasks", attemptFixture.attemptID, "attempt-001")
		if err := os.MkdirAll(runtimeDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		resultPath := filepath.Join(runtimeDirectory, "result.json")
		if attemptFixture.writeStepID {
			if err := os.WriteFile(filepath.Join(runtimeDirectory, "slurm-step-id"), []byte(attemptFixture.stepID+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := stateStore.UpsertTask(t.Context(), store.TaskInstance{
			RunID:       runID,
			TaskID:      attemptFixture.taskID,
			JobID:       filepath.Base(attemptFixture.taskID),
			Dimensions:  map[string]string{},
			Inputs:      map[string][]string{},
			Outputs:     map[string]string{},
			Fingerprint: "fingerprint-" + attemptFixture.attemptID,
			Status:      "succeeded",
		}); err != nil {
			t.Fatal(err)
		}
		if err := stateStore.CreateAttempt(t.Context(), store.TaskAttempt{
			ID:            attemptFixture.attemptID,
			RunID:         runID,
			TaskID:        attemptFixture.taskID,
			AttemptNumber: 1,
			Status:        "succeeded",
			StartedAt:     &startedAt,
			FinishedAt:    &finishedAt,
			ResultPath:    resultPath,
		}); err != nil {
			t.Fatal(err)
		}
		inputArtifactBytes := int64(111)
		outputArtifactBytes := int64(222)
		if err := stateStore.SaveMetrics(t.Context(), attemptFixture.attemptID, &metrics.TaskMetrics{
			Source:              "slurm_sacct",
			Quality:             "unavailable",
			InputArtifactBytes:  &inputArtifactBytes,
			OutputArtifactBytes: &outputArtifactBytes,
			Raw:                 map[string]string{"JobID": attemptFixture.stepID},
		}, 2, 64<<20); err != nil {
			t.Fatal(err)
		}
	}
	if err := stateStore.Close(); err != nil {
		t.Fatal(err)
	}

	fakeCommandDirectory := t.TempDir()
	writeExecutable(t, filepath.Join(fakeCommandDirectory, "sacct"), "#!/bin/sh\nprintf '12345.7|COMPLETED|0:0|12|4|00:00:20|00:00:15|00:00:05|1024K|512K|2048K|4M|8M|4096Mc|node-a|2026-07-23T00:00:00|2026-07-23T00:00:12\\n'\n")
	commandEnvironment := append(os.Environ(), "PATH="+fakeCommandDirectory)
	reportDirectory := filepath.Join(temporaryDirectory, "reports")
	reportOutput := runCraftmake(
		t,
		binaryPath,
		commandEnvironment,
		"report",
		"--state", statePath,
		"--run", runID,
		"--output", reportDirectory,
		"--refresh-metrics",
	)
	for _, expectedOutput := range []string{
		"metrics_refresh_candidates: 2",
		"metrics_refreshed: 1",
		"metrics_unavailable: 1",
		"warning: metrics refresh for attempt attempt-unavailable failed",
		reportDirectory,
	} {
		if !strings.Contains(reportOutput, expectedOutput) {
			t.Fatalf("report refresh output does not contain %q:\n%s", expectedOutput, reportOutput)
		}
	}

	stateStore, err = store.Open(t.Context(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	metricRows, err := stateStore.QueryRows(t.Context(), `
		SELECT attempt_id, source, quality, COALESCE(wall_seconds, 0), COALESCE(allocated_cpus, 0),
		       COALESCE(requested_memory_bytes, 0), COALESCE(max_rss_bytes, 0),
		       COALESCE(input_artifact_bytes, 0), COALESCE(output_artifact_bytes, 0)
		FROM task_metrics
		WHERE attempt_id IN ('attempt-refreshable', 'attempt-unavailable')
		ORDER BY attempt_id
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer metricRows.Close()
	type persistedMetrics struct {
		source               string
		quality              string
		wallSeconds          float64
		allocatedCPUs        int64
		requestedMemoryBytes int64
		maxRSSBytes          int64
		inputArtifactBytes   int64
		outputArtifactBytes  int64
	}
	persistedByAttempt := make(map[string]persistedMetrics)
	for metricRows.Next() {
		var attemptID string
		var persisted persistedMetrics
		if err := metricRows.Scan(
			&attemptID,
			&persisted.source,
			&persisted.quality,
			&persisted.wallSeconds,
			&persisted.allocatedCPUs,
			&persisted.requestedMemoryBytes,
			&persisted.maxRSSBytes,
			&persisted.inputArtifactBytes,
			&persisted.outputArtifactBytes,
		); err != nil {
			t.Fatal(err)
		}
		persistedByAttempt[attemptID] = persisted
	}
	if err := metricRows.Err(); err != nil {
		t.Fatal(err)
	}
	refreshedMetrics := persistedByAttempt["attempt-refreshable"]
	if refreshedMetrics.source != "slurm_sacct" || refreshedMetrics.quality != "accounting" || refreshedMetrics.wallSeconds != 12 || refreshedMetrics.allocatedCPUs != 4 || refreshedMetrics.maxRSSBytes != 1<<20 {
		t.Fatalf("unexpected refreshed metrics: %#v", refreshedMetrics)
	}
	if refreshedMetrics.requestedMemoryBytes != 64<<20 || refreshedMetrics.inputArtifactBytes != 111 || refreshedMetrics.outputArtifactBytes != 222 {
		t.Fatalf("refresh did not preserve requested resources and artifact bytes: %#v", refreshedMetrics)
	}
	unavailableMetrics := persistedByAttempt["attempt-unavailable"]
	if unavailableMetrics.quality != "unavailable" {
		t.Fatalf("unavailable metrics were unexpectedly replaced: %#v", unavailableMetrics)
	}

	csvData, err := os.ReadFile(filepath.Join(reportDirectory, "task_metrics.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(csvData), "slurm_sacct,accounting") || !strings.Contains(string(csvData), "slurm_sacct,unavailable") {
		t.Fatalf("exported task metrics do not contain refreshed and unavailable qualities:\n%s", csvData)
	}

	secondReportOutput := runCraftmake(
		t,
		binaryPath,
		commandEnvironment,
		"report",
		"--state", statePath,
		"--run", runID,
		"--output", reportDirectory,
		"--refresh-metrics",
	)
	if !strings.Contains(secondReportOutput, "metrics_refresh_candidates: 1") || !strings.Contains(secondReportOutput, "metrics_refreshed: 0") || !strings.Contains(secondReportOutput, "metrics_unavailable: 1") {
		t.Fatalf("repeated refresh did not skip accounting-quality metrics:\n%s", secondReportOutput)
	}
}
