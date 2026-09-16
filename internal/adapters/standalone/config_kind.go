package standalone

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type ConfigKind string

const (
	ConfigKindOtterRun   ConfigKind = "otter-run"
	ConfigKindLegacy     ConfigKind = "legacy-otter"
	ConfigKindStandalone ConfigKind = "standalone"
)

// DetectConfigKind chooses a loader without accepting ambiguous configurations.
func DetectConfigKind(path string) (ConfigKind, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read configuration %q: %w", path, err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return "", fmt.Errorf("parse configuration %q: %w", path, err)
	}
	if document == nil {
		return "", fmt.Errorf("configuration %q must be a YAML mapping", path)
	}
	if schemaVersion := mappingString(document, "schema_version"); schemaVersion != "" {
		switch schemaVersion {
		case "otter.run/v1":
			return ConfigKindOtterRun, nil
		case SchemaVersion:
			return ConfigKindStandalone, nil
		default:
			return "", fmt.Errorf("configuration %q has unsupported schema_version %q; expected %q or %q", path, schemaVersion, "otter.run/v1", SchemaVersion)
		}
	}
	if hasCompleteLegacyOtterShape(document) {
		return ConfigKindLegacy, nil
	}
	return "", fmt.Errorf("configuration %q is neither an explicit %q configuration nor a complete legacy Otter configuration; declare schema_version or provide legacy mode, samples, species, and input/reference fields", path, SchemaVersion)
}

func hasCompleteLegacyOtterShape(document map[string]any) bool {
	mode := firstNonEmpty(mappingString(document, "mode"), nestedString(document, "workflow", "mode"))
	species := firstNonEmpty(
		mappingString(document, "species"),
		nestedString(document, "workflow", "species", "primary"),
		nestedString(document, "species", "primary"),
	)
	hasSamples := len(stringSlice(document["SIDs"])) > 0 ||
		len(stringSlice(document["samples"])) > 0 ||
		len(stringSlice(nestedValue(document, "metadata", "sample_ids"))) > 0 ||
		len(stringSlice(nestedValue(document, "metadata", "SIDs"))) > 0
	hasInputOrOutput := nestedString(document, "input", "fastq_dir") != "" ||
		nestedString(document, "output", "raw_dir") != "" ||
		nestedString(document, "output", "workflow_dir") != "" ||
		nestedString(document, "output", "analysis_dir") != "" ||
		nestedString(document, "directories", "work") != "" ||
		nestedString(document, "directories", "workflow") != ""
	hasReference := len(stringSlice(nestedValue(document, "reference", "genome_fasta"))) > 0 ||
		len(stringSlice(nestedValue(document, "reference", "files", "fasta"))) > 0 ||
		len(stringSlice(nestedValue(document, "reference", "genome_index"))) > 0 ||
		len(stringSlice(nestedValue(document, "reference", "indices", "genome"))) > 0
	hasWorkflowContract := nestedString(document, "workflow", "jobid") != "" ||
		nestedString(document, "workflow", "userid") != ""
	return mode != "" && species != "" && hasSamples && hasInputOrOutput && hasReference && hasWorkflowContract
}

func mappingString(document map[string]any, key string) string {
	return stringValue(document[key])
}

func nestedString(document map[string]any, keys ...string) string {
	return stringValue(nestedValue(document, keys...))
}

func nestedValue(document map[string]any, keys ...string) any {
	var current any = document
	for _, key := range keys {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = mapping[key]
	}
	return current
}

func stringSlice(value any) []string {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, item := range values {
		if stringItem := stringValue(item); stringItem != "" {
			result = append(result, stringItem)
		}
	}
	return result
}

func stringValue(value any) string {
	stringValue, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(stringValue)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
