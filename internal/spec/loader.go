package spec

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

func Load(path string) (*WorkflowSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow %q: %w", path, err)
	}

	var workflow WorkflowSpec
	decoderErr := yaml.Unmarshal(data, &workflow)
	if decoderErr != nil {
		return nil, fmt.Errorf("parse workflow %q: %w", path, decoderErr)
	}
	if err := workflow.Validate(); err != nil {
		return nil, fmt.Errorf("validate workflow %q: %w", path, err)
	}
	return &workflow, nil
}
