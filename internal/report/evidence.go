package report

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/otterlab-bio/craftmake/internal/store"
)

type EvidenceBundle struct {
	SchemaVersion      string                  `json:"schema_version"`
	GeneratedAt        time.Time               `json:"generated_at"`
	Run                store.Run               `json:"run"`
	TaskStatusCounts   map[string]int          `json:"task_status_counts"`
	ControllerLogPath  string                  `json:"controller_log_path"`
	MetricsCSVPath     string                  `json:"metrics_csv_path"`
	StepTimingsCSVPath string                  `json:"step_timings_csv_path"`
	AllocationsCSVPath string                  `json:"allocations_csv_path"`
	Incidents          []store.RuntimeIncident `json:"incidents"`
}

func ExportEvidenceBundle(
	ctx context.Context,
	stateStore *store.Store,
	runID string,
	outputDirectory string,
	controllerLogPath string,
) (string, error) {
	run, taskStatusCounts, err := stateStore.RunSummary(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load evidence run summary: %w", err)
	}
	incidents, err := stateStore.ListRuntimeIncidents(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("load evidence runtime incidents: %w", err)
	}

	bundle := EvidenceBundle{
		SchemaVersion:      "craftmake.run-evidence/v1",
		GeneratedAt:        time.Now().UTC(),
		Run:                run,
		TaskStatusCounts:   taskStatusCounts,
		ControllerLogPath:  controllerLogPath,
		MetricsCSVPath:     filepath.Join(outputDirectory, "task_metrics.csv"),
		StepTimingsCSVPath: filepath.Join(outputDirectory, "step_timings.csv"),
		AllocationsCSVPath: filepath.Join(outputDirectory, "allocations.csv"),
		Incidents:          incidents,
	}
	encodedBundle, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode run evidence bundle: %w", err)
	}
	outputPath := filepath.Join(outputDirectory, "run-evidence.json")
	if err := os.WriteFile(outputPath, append(encodedBundle, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("write run evidence bundle: %w", err)
	}
	return outputPath, nil
}
