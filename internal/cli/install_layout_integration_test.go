package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMakeInstallProvidesPortableWorkflowCatalog(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	buildDirectory := filepath.Join(temporaryDirectory, "build")
	stagingDirectory := filepath.Join(temporaryDirectory, "stage")
	installArguments := []string{
		"install",
		"BUILD_DIR=" + buildDirectory,
		"DESTDIR=" + stagingDirectory,
		"PREFIX=/usr/local",
		"VERSION=9.8.7-test",
		"COMMIT=testcommit",
		"BUILD_DATE=2026-07-20T00:00:00Z",
	}
	installCommand := exec.Command("make", installArguments...)
	installCommand.Dir = repositoryRoot
	if installOutput, err := installCommand.CombinedOutput(); err != nil {
		t.Fatalf("make %s: %v\n%s", strings.Join(installArguments, " "), err, installOutput)
	}

	installedBinary := filepath.Join(stagingDirectory, "usr", "local", "bin", "craftmake")
	installedWorkflow := filepath.Join(stagingDirectory, "usr", "local", "share", "craftmake", "workflows", "BeaverRNASEQPDX", "step3-check.yaml")
	for _, installedPath := range []string{installedBinary, installedWorkflow} {
		if _, err := os.Stat(installedPath); err != nil {
			t.Fatalf("expected installed path %q: %v", installedPath, err)
		}
	}

	versionCommand := exec.Command(installedBinary, "--version")
	versionOutput, err := versionCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("installed version command: %v\n%s", err, versionOutput)
	}
	if !strings.Contains(string(versionOutput), "9.8.7-test+testcommit") {
		t.Fatalf("unexpected installed version output %q", versionOutput)
	}

	unrelatedWorkingDirectory := filepath.Join(temporaryDirectory, "working")
	if err := os.MkdirAll(unrelatedWorkingDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	validateCommand := exec.Command(installedBinary,
		"validate",
		"--legacy-config", "--config", filepath.Join(repositoryRoot, "testdata", "configs", "beaverrnaseqpdx-step3-check.yaml"),
		"--phase", "step3-check",
	)
	validateCommand.Dir = unrelatedWorkingDirectory
	validateCommand.Env = environmentWithoutWorkflowCatalog(os.Environ())
	validateOutput, err := validateCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("installed catalog validation: %v\n%s", err, validateOutput)
	}
	if !strings.Contains(string(validateOutput), "valid: BeaverRNASEQPDX step3-check (8 tasks, 8 submissions)") {
		t.Fatalf("unexpected installed catalog validation output %q", validateOutput)
	}

	uninstallArguments := []string{
		"uninstall",
		"DESTDIR=" + stagingDirectory,
		"PREFIX=/usr/local",
	}
	uninstallCommand := exec.Command("make", uninstallArguments...)
	uninstallCommand.Dir = repositoryRoot
	if uninstallOutput, err := uninstallCommand.CombinedOutput(); err != nil {
		t.Fatalf("make %s: %v\n%s", strings.Join(uninstallArguments, " "), err, uninstallOutput)
	}
	for _, removedPath := range []string{installedBinary, filepath.Dir(filepath.Dir(installedWorkflow))} {
		if _, err := os.Stat(removedPath); !os.IsNotExist(err) {
			t.Fatalf("expected uninstall to remove %q, stat error: %v", removedPath, err)
		}
	}
}

func environmentWithoutWorkflowCatalog(environment []string) []string {
	filteredEnvironment := make([]string, 0, len(environment))
	for _, environmentValue := range environment {
		if !strings.HasPrefix(environmentValue, "CRAFTMAKE_WORKFLOW_CATALOG=") {
			filteredEnvironment = append(filteredEnvironment, environmentValue)
		}
	}
	return filteredEnvironment
}
