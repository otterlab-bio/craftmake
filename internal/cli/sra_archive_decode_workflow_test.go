package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSRAArchiveDecodeWorkflowPublishesFinalManifestPaths(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	workflowPath := filepath.Join(repositoryRoot, "workflows", "SRAArchiveDecode", "decode.yaml")
	workflowContents, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	workflowText := string(workflowContents)
	for _, requiredFragment := range []string{
		`python3 - "${publication_root}" "${output_root}"`,
		`'path':str(output_root / 'R1.fastq.gz')`,
		`'path':str(output_root / 'R2.fastq.gz')`,
		`mv "${publication_root}" "${output_root}"`,
		`sha256sum -c checksums.sha256`,
	} {
		if !strings.Contains(workflowText, requiredFragment) {
			t.Fatalf("SRA archive decode workflow is missing final-publication contract %q", requiredFragment)
		}
	}
	for _, forbiddenFragment := range []string{
		`'path':str(r1)`,
		`'path':str(r2)`,
	} {
		if strings.Contains(workflowText, forbiddenFragment) {
			t.Fatalf("SRA archive decode workflow retains temporary publication path %q", forbiddenFragment)
		}
	}
}
