package controllerlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type recordedEvent struct {
	runID      string
	taskID     string
	occurredAt time.Time
	eventType  string
	payload    json.RawMessage
}

type recordingEventStore struct {
	mutex  sync.Mutex
	events []recordedEvent
	err    error
}

func (eventStore *recordingEventStore) RecordEvent(
	_ context.Context,
	runID string,
	taskID string,
	occurredAt time.Time,
	eventType string,
	payload json.RawMessage,
) error {
	eventStore.mutex.Lock()
	defer eventStore.mutex.Unlock()
	eventStore.events = append(eventStore.events, recordedEvent{
		runID:      runID,
		taskID:     taskID,
		occurredAt: occurredAt,
		eventType:  eventType,
		payload:    append(json.RawMessage(nil), payload...),
	})
	return eventStore.err
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("intentional write failure")
}

func TestLoggerWritesStableJSONAndRecordsEvent(t *testing.T) {
	var output bytes.Buffer
	eventStore := &recordingEventStore{}
	logger := New(&output, eventStore)
	fixedTime := time.Date(2026, time.July, 22, 12, 34, 56, 123456789, time.FixedZone("test", 8*60*60))
	logger.clock = func() time.Time { return fixedTime }

	logger.Log(context.Background(), Event{
		Name:          "attempt.finished",
		RunID:         "run-a",
		SubmissionID:  "submission-a",
		TaskID:        "task-a",
		AttemptID:     "attempt-a",
		AttemptNumber: 2,
		Backend:       "slurm",
		BackendJobID:  "12345_7",
		Status:        "failed",
		Error:         "worker exited with status 1",
		Details:       map[string]any{"exit_code": 1},
	})

	var event Event
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatalf("decode controller event: %v", err)
	}
	if event.Timestamp != fixedTime.UTC() {
		t.Fatalf("unexpected timestamp %s", event.Timestamp)
	}
	if event.Level != "info" || event.Name != "attempt.finished" {
		t.Fatalf("unexpected event identity: %#v", event)
	}
	if event.RunID != "run-a" || event.SubmissionID != "submission-a" || event.TaskID != "task-a" || event.AttemptID != "attempt-a" {
		t.Fatalf("unexpected event context: %#v", event)
	}
	if len(eventStore.events) != 1 {
		t.Fatalf("expected one persisted event, got %d", len(eventStore.events))
	}
	persistedEvent := eventStore.events[0]
	if persistedEvent.runID != "run-a" || persistedEvent.taskID != "task-a" || persistedEvent.eventType != "attempt.finished" {
		t.Fatalf("unexpected persisted event: %#v", persistedEvent)
	}
	if len(logger.Errors()) != 0 {
		t.Fatalf("unexpected logger errors: %v", logger.Errors())
	}
}

func TestLoggerSerializesConcurrentEvents(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output, nil)
	logger.clock = func() time.Time {
		return time.Date(2026, time.July, 22, 0, 0, 0, 0, time.UTC)
	}

	const eventCount = 64
	var waitGroup sync.WaitGroup
	for eventIndex := 0; eventIndex < eventCount; eventIndex++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			logger.Log(context.Background(), Event{
				Name:    "task.cache_evaluated",
				RunID:   "run-concurrent",
				TaskID:  "task",
				Details: map[string]any{"index": index},
			})
		}(eventIndex)
	}
	waitGroup.Wait()

	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decodedEvents := 0
	for {
		var event Event
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode event %d: %v", decodedEvents, err)
		}
		decodedEvents++
	}
	if decodedEvents != eventCount {
		t.Fatalf("expected %d events, got %d", eventCount, decodedEvents)
	}
}

func TestLoggerFailuresRemainDiagnosticOnly(t *testing.T) {
	eventStore := &recordingEventStore{err: errors.New("intentional persistence failure")}
	logger := New(failingWriter{}, eventStore)

	logger.Log(context.Background(), Event{Name: "run.started", RunID: "run-a"})

	if len(logger.Errors()) != 2 {
		t.Fatalf("expected file and persistence errors, got %v", logger.Errors())
	}
	if len(eventStore.events) != 1 {
		t.Fatalf("expected persistence attempt despite writer failure, got %d", len(eventStore.events))
	}
}
