package report

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/internal/store"
)

type MetricRefreshFailure struct {
	AttemptID string
	Err       error
}

type MetricRefreshSummary struct {
	Candidates int
	Refreshed  int
	Failures   []MetricRefreshFailure
}

func RefreshMetrics(
	ctx context.Context,
	stateStore *store.Store,
	runID string,
	metricsRefresher backend.MetricsRefresher,
) (MetricRefreshSummary, error) {
	summary := MetricRefreshSummary{}
	if metricsRefresher == nil {
		return summary, fmt.Errorf("metrics refresher is required")
	}
	candidates, err := stateStore.RefreshableMetrics(ctx, runID)
	if err != nil {
		return summary, err
	}
	summary.Candidates = len(candidates)

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if strings.TrimSpace(candidate.ResultPath) == "" {
			summary.Failures = append(summary.Failures, MetricRefreshFailure{
				AttemptID: candidate.AttemptID,
				Err:       fmt.Errorf("task result path is unavailable"),
			})
			continue
		}
		collected, refreshErr := metricsRefresher.RefreshMetrics(ctx, backend.MetricsRefreshRequest{
			AttemptID:        candidate.AttemptID,
			RuntimeDirectory: filepath.Dir(candidate.ResultPath),
		})
		if refreshErr != nil {
			summary.Failures = append(summary.Failures, MetricRefreshFailure{AttemptID: candidate.AttemptID, Err: refreshErr})
			continue
		}
		if err := stateStore.SaveRefreshedMetrics(ctx, candidate.AttemptID, collected); err != nil {
			return summary, err
		}
		summary.Refreshed++
	}
	return summary, nil
}
