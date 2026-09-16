package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGitHubAutomationWorkflowYAMLParses(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	workflowPaths := []string{
		filepath.Join(repositoryRoot, ".github", "workflows", "ci.yaml"),
		filepath.Join(repositoryRoot, ".github", "workflows", "release.yaml"),
	}
	for _, workflowPath := range workflowPaths {
		t.Run(filepath.Base(workflowPath), func(t *testing.T) {
			workflowContents, err := os.ReadFile(workflowPath)
			if err != nil {
				t.Fatal(err)
			}
			var workflowDocument yaml.Node
			if err := yaml.Unmarshal(workflowContents, &workflowDocument); err != nil {
				t.Fatalf("parse GitHub workflow %q: %v", workflowPath, err)
			}
			if len(workflowDocument.Content) != 1 || workflowDocument.Content[0].Kind != yaml.MappingNode {
				t.Fatalf("GitHub workflow %q must contain one mapping document", workflowPath)
			}
			rootMapping := workflowDocument.Content[0]
			for _, requiredKey := range []string{"name", "on", "permissions", "jobs"} {
				if !yamlMappingContainsKey(rootMapping, requiredKey) {
					t.Fatalf("GitHub workflow %q is missing top-level key %q", workflowPath, requiredKey)
				}
			}
		})
	}
}

func yamlMappingContainsKey(mapping *yaml.Node, expectedKey string) bool {
	for mappingIndex := 0; mappingIndex+1 < len(mapping.Content); mappingIndex += 2 {
		if mapping.Content[mappingIndex].Value == expectedKey {
			return true
		}
	}
	return false
}
