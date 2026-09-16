package protocol

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDecodeTaskManifestCompatibilityPolicy(t *testing.T) {
	testCases := []struct {
		name             string
		protocolVersion  int
		extraJSON        string
		expectedError    string
		unsupportedError bool
	}{
		{name: "minimum compatible", protocolVersion: MinimumCompatibleVersion},
		{name: "current", protocolVersion: Version},
		{name: "unknown additive field", protocolVersion: Version, extraJSON: `,"future_optional_field":{"enabled":true}`},
		{name: "missing version", protocolVersion: 0, expectedError: "older than minimum compatible", unsupportedError: true},
		{name: "future version", protocolVersion: Version + 1, expectedError: "newer than supported", unsupportedError: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`{
				"protocol_version":%d,
				"run_id":"run-a",
				"task_id":"task-a",
				"attempt":1,
				"work_directory":"/work",
				"runtime_directory":"/runtime",
				"result_path":"/runtime/result.json",
				"resources":{"cores":1,"memory_bytes":1024},
				"steps":[]%s
			}`, testCase.protocolVersion, testCase.extraJSON))

			manifest, err := DecodeTaskManifest(payload)
			if testCase.expectedError == "" {
				if err != nil {
					t.Fatal(err)
				}
				if manifest.ProtocolVersion != testCase.protocolVersion {
					t.Fatalf("unexpected decoded version %d", manifest.ProtocolVersion)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.expectedError) {
				t.Fatalf("expected error containing %q, got %v", testCase.expectedError, err)
			}
			if testCase.unsupportedError && !errors.Is(err, ErrUnsupportedVersion) {
				t.Fatalf("expected unsupported-version classification, got %v", err)
			}
		})
	}
}

func TestDecodeTaskManifestRejectsCorruptAndIncompletePayloads(t *testing.T) {
	testCases := []struct {
		name          string
		payload       string
		expectedError string
	}{
		{name: "corrupt JSON", payload: `{"protocol_version":2`, expectedError: "decode task manifest JSON"},
		{name: "trailing JSON", payload: validManifestJSON(Version) + ` {}`, expectedError: "decode task manifest JSON"},
		{name: "missing run ID", payload: strings.Replace(validManifestJSON(Version), `"run_id":"run-a"`, `"run_id":""`, 1), expectedError: "run_id is required"},
		{name: "nonpositive attempt", payload: strings.Replace(validManifestJSON(Version), `"attempt":1`, `"attempt":0`, 1), expectedError: "attempt must be positive"},
		{name: "missing result path", payload: strings.Replace(validManifestJSON(Version), `"result_path":"/runtime/result.json"`, `"result_path":""`, 1), expectedError: "result_path is required"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := DecodeTaskManifest([]byte(testCase.payload))
			if err == nil || !strings.Contains(err.Error(), testCase.expectedError) {
				t.Fatalf("expected error containing %q, got %v", testCase.expectedError, err)
			}
		})
	}
}

func TestDecodeTaskResultCompatibilityAndIdentityPolicy(t *testing.T) {
	manifest := &TaskManifest{RunID: "run-a", TaskID: "task-a", Attempt: 2}
	testCases := []struct {
		name             string
		payload          string
		requireTerminal  bool
		expectedError    string
		unsupportedError bool
	}{
		{name: "minimum compatible", payload: validResultJSON(MinimumCompatibleVersion, "run-a", "task-a", 2, "succeeded"), requireTerminal: true},
		{name: "current with unknown field", payload: strings.TrimSuffix(validResultJSON(Version, "run-a", "task-a", 2, "failed"), "}") + `,"future_diagnostics":{"code":"x"}}`, requireTerminal: true},
		{name: "running allowed during recovery polling", payload: validResultJSON(Version, "run-a", "task-a", 2, "running")},
		{name: "running rejected as terminal", payload: validResultJSON(Version, "run-a", "task-a", 2, "running"), requireTerminal: true, expectedError: "not terminal"},
		{name: "future version", payload: validResultJSON(Version+1, "run-a", "task-a", 2, "succeeded"), requireTerminal: true, expectedError: "newer than supported", unsupportedError: true},
		{name: "mismatched identity", payload: validResultJSON(Version, "run-a", "different-task", 2, "succeeded"), requireTerminal: true, expectedError: "identity does not match manifest"},
		{name: "unknown status", payload: validResultJSON(Version, "run-a", "task-a", 2, "mystery"), requireTerminal: true, expectedError: "not recognized"},
		{name: "corrupt JSON", payload: `{"protocol_version":2`, expectedError: "decode task result JSON"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := DecodeTaskResultForManifest([]byte(testCase.payload), manifest, testCase.requireTerminal)
			if testCase.expectedError == "" {
				if err != nil {
					t.Fatal(err)
				}
				if result.RunID != manifest.RunID || result.TaskID != manifest.TaskID || result.Attempt != manifest.Attempt {
					t.Fatalf("unexpected decoded result identity: %#v", result)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.expectedError) {
				t.Fatalf("expected error containing %q, got %v", testCase.expectedError, err)
			}
			if testCase.unsupportedError && !errors.Is(err, ErrUnsupportedVersion) {
				t.Fatalf("expected unsupported-version classification, got %v", err)
			}
		})
	}
}

func TestDecodeTaskEventCompatibilityAndRequiredFields(t *testing.T) {
	testCases := []struct {
		name             string
		payload          string
		expectedError    string
		unsupportedError bool
	}{
		{
			name:    "minimum compatible with unknown field",
			payload: fmt.Sprintf(`{"protocol_version":%d,"run_id":"run-a","task_id":"task-a","type":"task.started","timestamp":"2026-07-23T00:00:00Z","payload":{"attempt":1},"future_optional":true}`, MinimumCompatibleVersion),
		},
		{
			name:             "future version",
			payload:          fmt.Sprintf(`{"protocol_version":%d,"run_id":"run-a","type":"task.started","timestamp":"2026-07-23T00:00:00Z"}`, Version+1),
			expectedError:    "newer than supported",
			unsupportedError: true,
		},
		{name: "missing run ID", payload: fmt.Sprintf(`{"protocol_version":%d,"type":"task.started","timestamp":"2026-07-23T00:00:00Z"}`, Version), expectedError: "run_id is required"},
		{name: "missing type", payload: fmt.Sprintf(`{"protocol_version":%d,"run_id":"run-a","timestamp":"2026-07-23T00:00:00Z"}`, Version), expectedError: "type is required"},
		{name: "missing timestamp", payload: fmt.Sprintf(`{"protocol_version":%d,"run_id":"run-a","type":"task.started"}`, Version), expectedError: "timestamp is required"},
		{name: "corrupt JSON", payload: `{"protocol_version":2`, expectedError: "decode task event JSON"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			event, err := DecodeTaskEvent([]byte(testCase.payload))
			if testCase.expectedError == "" {
				if err != nil {
					t.Fatal(err)
				}
				if event.RunID != "run-a" || event.Type != "task.started" {
					t.Fatalf("unexpected decoded event: %#v", event)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.expectedError) {
				t.Fatalf("expected error containing %q, got %v", testCase.expectedError, err)
			}
			if testCase.unsupportedError && !errors.Is(err, ErrUnsupportedVersion) {
				t.Fatalf("expected unsupported-version classification, got %v", err)
			}
		})
	}
}

func TestValidateTaskResultTimePolicy(t *testing.T) {
	manifest := &TaskManifest{RunID: "run-a", TaskID: "task-a", Attempt: 1}
	startedAt := time.Date(2026, time.July, 23, 0, 0, 0, 0, time.UTC)
	testCases := []struct {
		name          string
		status        string
		resultStarted time.Time
		resultFinish  time.Time
		requireTerm   bool
		expectedError string
	}{
		{name: "terminal result requires finish time", status: "succeeded", resultStarted: startedAt, requireTerm: true, expectedError: "finished_at is required"},
		{name: "result cannot finish before start", status: "failed", resultStarted: startedAt, resultFinish: startedAt.Add(-time.Second), requireTerm: true, expectedError: "must not be before started_at"},
		{name: "running result may omit finish time", status: "running", resultStarted: startedAt},
		{name: "every result requires start time", status: "running", expectedError: "started_at is required"},
		{name: "terminal result with ordered timestamps", status: "cancelled", resultStarted: startedAt, resultFinish: startedAt.Add(time.Second), requireTerm: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateTaskResultForManifest(&TaskResult{
				ProtocolVersion: Version,
				RunID:           manifest.RunID,
				TaskID:          manifest.TaskID,
				Attempt:         manifest.Attempt,
				Status:          testCase.status,
				StartedAt:       testCase.resultStarted,
				FinishedAt:      testCase.resultFinish,
			}, manifest, testCase.requireTerm)
			if testCase.expectedError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.expectedError) {
				t.Fatalf("expected error containing %q, got %v", testCase.expectedError, err)
			}
		})
	}
}

func validManifestJSON(protocolVersion int) string {
	return fmt.Sprintf(`{"protocol_version":%d,"run_id":"run-a","task_id":"task-a","attempt":1,"work_directory":"/work","runtime_directory":"/runtime","result_path":"/runtime/result.json","resources":{"cores":1,"memory_bytes":1024},"steps":[]}`, protocolVersion)
}

func validResultJSON(protocolVersion int, runID string, taskID string, attempt int, status string) string {
	return fmt.Sprintf(`{"protocol_version":%d,"run_id":%q,"task_id":%q,"attempt":%d,"status":%q,"started_at":"2026-07-23T00:00:00Z","finished_at":"2026-07-23T00:00:01Z","exit_code":0,"steps":[]}`, protocolVersion, runID, taskID, attempt, status)
}
