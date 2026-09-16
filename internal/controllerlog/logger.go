package controllerlog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const FileName = "controller.jsonl"

type Event struct {
	Timestamp            time.Time      `json:"timestamp"`
	Level                string         `json:"level"`
	Name                 string         `json:"event"`
	RunID                string         `json:"run_id,omitempty"`
	SubmissionID         string         `json:"submission_id,omitempty"`
	TaskID               string         `json:"task_id,omitempty"`
	AttemptID            string         `json:"attempt_id,omitempty"`
	AttemptNumber        int            `json:"attempt,omitempty"`
	Backend              string         `json:"backend,omitempty"`
	BackendJobID         string         `json:"backend_job_id,omitempty"`
	Status               string         `json:"status,omitempty"`
	DurationMilliseconds int64          `json:"duration_ms,omitempty"`
	Error                string         `json:"error,omitempty"`
	Details              map[string]any `json:"details,omitempty"`
}

type EventRecorder interface {
	RecordEvent(
		context.Context,
		string,
		string,
		time.Time,
		string,
		json.RawMessage,
	) error
}

type Logger struct {
	mutex    sync.Mutex
	writer   io.Writer
	closer   io.Closer
	recorder EventRecorder
	clock    func() time.Time
	errors   []error
}

func DefaultPath(stateDirectory string, runID string) string {
	return filepath.Join(stateDirectory, "runs", runID, FileName)
}

func Open(path string, recorder EventRecorder) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create controller log directory: %w", err)
	}
	logFile, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open controller log: %w", err)
	}
	return &Logger{
		writer:   logFile,
		closer:   logFile,
		recorder: recorder,
		clock:    func() time.Time { return time.Now().UTC() },
	}, nil
}

func New(writer io.Writer, recorder EventRecorder) *Logger {
	if writer == nil {
		writer = io.Discard
	}
	return &Logger{
		writer:   writer,
		recorder: recorder,
		clock:    func() time.Time { return time.Now().UTC() },
	}
}

func Discard() *Logger {
	return New(io.Discard, nil)
}

func (logger *Logger) Log(ctx context.Context, event Event) {
	if logger == nil {
		return
	}
	logger.mutex.Lock()
	defer logger.mutex.Unlock()

	if event.Timestamp.IsZero() {
		event.Timestamp = logger.clock().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	if event.Level == "" {
		event.Level = "info"
	}
	encodedEvent, err := json.Marshal(event)
	if err != nil {
		logger.errors = append(logger.errors, fmt.Errorf("encode controller event %q: %w", event.Name, err))
		return
	}
	if _, err := logger.writer.Write(append(encodedEvent, '\n')); err != nil {
		logger.errors = append(logger.errors, fmt.Errorf("write controller event %q: %w", event.Name, err))
	}
	if logger.recorder != nil && event.RunID != "" {
		if err := logger.recorder.RecordEvent(ctx, event.RunID, event.TaskID, event.Timestamp, event.Name, encodedEvent); err != nil {
			logger.errors = append(logger.errors, fmt.Errorf("record controller event %q: %w", event.Name, err))
		}
	}
}

func (logger *Logger) Errors() []error {
	if logger == nil {
		return nil
	}
	logger.mutex.Lock()
	defer logger.mutex.Unlock()
	return append([]error(nil), logger.errors...)
}

func (logger *Logger) Close() error {
	if logger == nil {
		return nil
	}
	logger.mutex.Lock()
	defer logger.mutex.Unlock()
	if logger.closer == nil {
		return nil
	}
	err := logger.closer.Close()
	logger.closer = nil
	if err != nil {
		logger.errors = append(logger.errors, fmt.Errorf("close controller log: %w", err))
	}
	return err
}
