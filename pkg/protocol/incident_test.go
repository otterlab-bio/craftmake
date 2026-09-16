package protocol

import (
	"testing"
	"time"
)

func TestClassifyTaskIncidentUsesStableCategoriesAndPolicies(t *testing.T) {
	finishedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	testCases := []struct {
		name             string
		result           TaskResult
		expectedCategory IncidentCategory
		expectedRetry    IncidentRetryPolicy
		expectedSafe     bool
	}{
		{
			name:             "resource exhaustion",
			result:           TaskResult{Status: "failed", Error: "slurm job OUT_OF_MEMORY", FinishedAt: finishedAt, ExitCode: 137},
			expectedCategory: IncidentResourceExhaustion,
			expectedRetry:    IncidentRetryManual,
		},
		{
			name:             "transient scheduler submission",
			result:           TaskResult{Status: "failed", Error: "unable to contact slurm controller", FinishedAt: finishedAt},
			expectedCategory: IncidentSchedulerSubmission,
			expectedRetry:    IncidentRetryAutomatic,
			expectedSafe:     true,
		},
		{
			name:             "missing declared output",
			result:           TaskResult{Status: "failed", MissingOutputs: []string{"matrix=results/matrix.h5"}, FinishedAt: finishedAt},
			expectedCategory: IncidentArtifactIntegrity,
			expectedRetry:    IncidentRetryNever,
		},
		{
			name:             "unclassified failure is safe by default",
			result:           TaskResult{Status: "failed", FinishedAt: finishedAt},
			expectedCategory: IncidentInternalUnknown,
			expectedRetry:    IncidentRetryNever,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			incident := ClassifyTaskIncident(testCase.result, "slurm")
			if incident == nil {
				t.Fatal("expected an incident")
			}
			if incident.SchemaVersion != IncidentSchemaVersion || incident.Category != testCase.expectedCategory {
				t.Fatalf("unexpected incident: %#v", incident)
			}
			if incident.RetryPolicy != testCase.expectedRetry || incident.RetrySafe != testCase.expectedSafe {
				t.Fatalf("unexpected retry policy: %#v", incident)
			}
			if incident.Backend != "slurm" || incident.FirstObservedAt != finishedAt {
				t.Fatalf("incident provenance was not retained: %#v", incident)
			}
		})
	}
}

func TestClassifyTaskIncidentDoesNotCreateSuccessIncident(t *testing.T) {
	if incident := ClassifyTaskIncident(TaskResult{Status: "succeeded"}, "slurm"); incident != nil {
		t.Fatalf("successful task has incident: %#v", incident)
	}
}
