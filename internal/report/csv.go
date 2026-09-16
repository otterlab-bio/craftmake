package report

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"

	"github.com/otterlab-bio/craftmake/internal/store"
)

func ExportCSV(ctx context.Context, stateStore *store.Store, runID, outputDirectory string) error {
	if err := os.MkdirAll(outputDirectory, 0o755); err != nil {
		return err
	}
	if err := exportQuery(ctx, stateStore, filepath.Join(outputDirectory, "task_metrics.csv"), `
		SELECT ti.run_id, ti.job_id, ti.task_id, ta.attempt_number, ti.status,
		COALESCE(ti.cache_decision,''), COALESCE(ti.cache_reason_code,''), COALESCE(ti.cache_reason_detail,''), ti.dimensions_json,
		COALESCE(tm.wall_seconds,''), COALESCE(tm.user_cpu_seconds,''), COALESCE(tm.system_cpu_seconds,''),
		COALESCE(tm.allocated_cpus,''), COALESCE(tm.requested_memory_bytes,''), COALESCE(tm.max_rss_bytes,''),
		COALESCE(tm.disk_read_bytes,''), COALESCE(tm.disk_write_bytes,''), COALESCE(tm.filesystem_input_operations,''),
		COALESCE(tm.filesystem_output_operations,''), COALESCE(tm.input_artifact_bytes,''), COALESCE(tm.output_artifact_bytes,''),
		COALESCE(ta.exit_code,''), COALESCE(tm.source,''), COALESCE(tm.quality,''),
		COALESCE(ta.result_path,'')
		FROM task_instances ti LEFT JOIN task_attempts ta ON ta.run_id=ti.run_id AND ta.task_id=ti.task_id
		LEFT JOIN task_metrics tm ON tm.attempt_id=ta.attempt_id WHERE ti.run_id=? ORDER BY ti.task_id, ta.attempt_number
	`, runID, []string{"run_id", "job_id", "task_id", "attempt", "status", "cache_decision", "cache_reason_code", "cache_reason_detail", "dimensions", "wall_seconds", "user_cpu_seconds", "system_cpu_seconds", "allocated_cpus", "requested_memory_bytes", "max_rss_bytes", "disk_read_bytes", "disk_write_bytes", "filesystem_input_operations", "filesystem_output_operations", "input_artifact_bytes", "output_artifact_bytes", "exit_code", "metric_source", "metric_quality", "result_path"}); err != nil {
		return err
	}
	if err := exportQuery(ctx, stateStore, filepath.Join(outputDirectory, "step_timings.csv"), `
		SELECT ta.run_id, ta.task_id, ta.attempt_number, sa.step_index, sa.step_name, COALESCE(sa.environment,''),
		COALESCE(sa.started_at,''), COALESCE(sa.finished_at,''), COALESCE(sa.wall_seconds,''), COALESCE(sa.exit_code,''),
		COALESCE(sa.stdout_path,''), COALESCE(sa.stderr_path,'')
		FROM step_attempts sa JOIN task_attempts ta ON ta.attempt_id=sa.attempt_id WHERE ta.run_id=?
		ORDER BY ta.task_id, ta.attempt_number, sa.step_index
	`, runID, []string{"run_id", "task_id", "attempt", "step_index", "step_name", "environment", "started_at", "finished_at", "wall_seconds", "exit_code", "stdout_path", "stderr_path"}); err != nil {
		return err
	}
	return exportQuery(ctx, stateStore, filepath.Join(outputDirectory, "allocations.csv"), `
		SELECT run_id, submission_id, scope, COALESCE(group_key,''), backend, COALESCE(backend_job_id,''), status,
		COALESCE(started_at,''), COALESCE(finished_at,''), COALESCE(resources_json,'')
		FROM physical_submissions WHERE run_id=? ORDER BY submission_id
	`, runID, []string{"run_id", "submission_id", "scope", "group_key", "backend", "backend_job_id", "status", "started_at", "finished_at", "resources"})
}

func exportQuery(ctx context.Context, stateStore *store.Store, path, query, runID string, headers []string) error {
	rows, err := stateStore.QueryRows(ctx, query, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	defer writer.Flush()
	if err := writer.Write(headers); err != nil {
		return err
	}
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		record := make([]string, len(values))
		for index, value := range values {
			switch typedValue := value.(type) {
			case []byte:
				record[index] = string(typedValue)
			case nil:
				record[index] = ""
			default:
				record[index] = fmt.Sprint(typedValue)
			}
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}
