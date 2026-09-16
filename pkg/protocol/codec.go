package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const MinimumCompatibleVersion = 1

var ErrUnsupportedVersion = errors.New("unsupported task protocol version")

func DecodeTaskManifest(data []byte) (*TaskManifest, error) {
	var manifest TaskManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode task manifest JSON: %w", err)
	}
	if err := validateProtocolVersion("task manifest", manifest.ProtocolVersion); err != nil {
		return nil, err
	}
	if strings.TrimSpace(manifest.RunID) == "" {
		return nil, fmt.Errorf("task manifest run_id is required")
	}
	if strings.TrimSpace(manifest.TaskID) == "" {
		return nil, fmt.Errorf("task manifest task_id is required")
	}
	if manifest.Attempt <= 0 {
		return nil, fmt.Errorf("task manifest attempt must be positive")
	}
	if strings.TrimSpace(manifest.WorkDirectory) == "" {
		return nil, fmt.Errorf("task manifest work_directory is required")
	}
	if strings.TrimSpace(manifest.RuntimeDirectory) == "" {
		return nil, fmt.Errorf("task manifest runtime_directory is required")
	}
	if strings.TrimSpace(manifest.ResultPath) == "" {
		return nil, fmt.Errorf("task manifest result_path is required")
	}
	return &manifest, nil
}

func DecodeTaskResult(data []byte) (*TaskResult, error) {
	var result TaskResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode task result JSON: %w", err)
	}
	if err := validateProtocolVersion("task result", result.ProtocolVersion); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.RunID) == "" {
		return nil, fmt.Errorf("task result run_id is required")
	}
	if strings.TrimSpace(result.TaskID) == "" {
		return nil, fmt.Errorf("task result task_id is required")
	}
	if result.Attempt <= 0 {
		return nil, fmt.Errorf("task result attempt must be positive")
	}
	if !isKnownTaskStatus(result.Status) {
		return nil, fmt.Errorf("task result status %q is not recognized", result.Status)
	}
	return &result, nil
}

func DecodeTaskEvent(data []byte) (*TaskEvent, error) {
	var event TaskEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, fmt.Errorf("decode task event JSON: %w", err)
	}
	if err := validateProtocolVersion("task event", event.ProtocolVersion); err != nil {
		return nil, err
	}
	if strings.TrimSpace(event.RunID) == "" {
		return nil, fmt.Errorf("task event run_id is required")
	}
	if strings.TrimSpace(event.Type) == "" {
		return nil, fmt.Errorf("task event type is required")
	}
	if event.Timestamp.IsZero() {
		return nil, fmt.Errorf("task event timestamp is required")
	}
	return &event, nil
}

func DecodeTaskResultForManifest(data []byte, manifest *TaskManifest, requireTerminal bool) (*TaskResult, error) {
	result, err := DecodeTaskResult(data)
	if err != nil {
		return nil, err
	}
	if err := ValidateTaskResultForManifest(result, manifest, requireTerminal); err != nil {
		return nil, err
	}
	return result, nil
}

func ValidateTaskResultForManifest(result *TaskResult, manifest *TaskManifest, requireTerminal bool) error {
	if result == nil {
		return fmt.Errorf("task result is required")
	}
	if manifest == nil {
		return fmt.Errorf("task manifest is required to validate task result identity")
	}
	if err := validateProtocolVersion("task result", result.ProtocolVersion); err != nil {
		return err
	}
	if result.RunID != manifest.RunID || result.TaskID != manifest.TaskID || result.Attempt != manifest.Attempt {
		return fmt.Errorf(
			"task result identity does not match manifest: got run=%q task=%q attempt=%d, expected run=%q task=%q attempt=%d",
			result.RunID,
			result.TaskID,
			result.Attempt,
			manifest.RunID,
			manifest.TaskID,
			manifest.Attempt,
		)
	}
	if !isKnownTaskStatus(result.Status) {
		return fmt.Errorf("task result status %q is not recognized", result.Status)
	}
	if result.StartedAt.IsZero() {
		return fmt.Errorf("task result started_at is required")
	}
	if IsTerminalTaskStatus(result.Status) && result.FinishedAt.IsZero() {
		return fmt.Errorf("task result finished_at is required for terminal status %q", result.Status)
	}
	if !result.FinishedAt.IsZero() && result.FinishedAt.Before(result.StartedAt) {
		return fmt.Errorf("task result finished_at must not be before started_at")
	}
	if requireTerminal && !IsTerminalTaskStatus(result.Status) {
		return fmt.Errorf("task result status %q is not terminal", result.Status)
	}
	return nil
}

func IsTerminalTaskStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func validateProtocolVersion(payloadName string, version int) error {
	switch {
	case version < MinimumCompatibleVersion:
		return fmt.Errorf(
			"%s protocol version %d is older than minimum compatible version %d: %w",
			payloadName,
			version,
			MinimumCompatibleVersion,
			ErrUnsupportedVersion,
		)
	case version > Version:
		return fmt.Errorf(
			"%s protocol version %d is newer than supported version %d: %w",
			payloadName,
			version,
			Version,
			ErrUnsupportedVersion,
		)
	default:
		return nil
	}
}

func isKnownTaskStatus(status string) bool {
	return status == "running" || IsTerminalTaskStatus(status)
}
