package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMakeReleaseBuildsPortableLinuxArchives(t *testing.T) {
	repositoryRoot := resolveRepositoryRoot(t)
	temporaryDirectory := t.TempDir()
	distributionDirectory := filepath.Join(temporaryDirectory, "dist")
	releaseArguments := []string{
		"release",
		"DIST_DIR=" + distributionDirectory,
		"VERSION=9.8.7-test",
		"COMMIT=testcommit",
		"BUILD_DATE=2026-07-20T00:00:00Z",
	}
	releaseCommand := exec.Command("make", releaseArguments...)
	releaseCommand.Dir = repositoryRoot
	if releaseOutput, err := releaseCommand.CombinedOutput(); err != nil {
		t.Fatalf("make %s: %v\n%s", strings.Join(releaseArguments, " "), err, releaseOutput)
	}

	archiveNames := []string{
		"craftmake_9.8.7-test_linux_amd64.tar.gz",
		"craftmake_9.8.7-test_linux_arm64.tar.gz",
	}
	for _, archiveName := range archiveNames {
		archivePath := filepath.Join(distributionDirectory, archiveName)
		if _, err := os.Stat(archivePath); err != nil {
			t.Fatalf("expected release archive %q: %v", archivePath, err)
		}
		listCommand := exec.Command("tar", "-tzf", archivePath)
		archiveListing, err := listCommand.CombinedOutput()
		if err != nil {
			t.Fatalf("list archive %q: %v\n%s", archivePath, err, archiveListing)
		}
		packageDirectory := strings.TrimSuffix(archiveName, ".tar.gz")
		listing := string(archiveListing)
		for _, expectedPath := range []string{
			packageDirectory + "/bin/craftmake",
			packageDirectory + "/share/craftmake/workflows/BeaverBS/step1.yaml",
			packageDirectory + "/share/craftmake/workflows/BeaverRNASEQPDX/step3-check.yaml",
		} {
			if !strings.Contains(listing, expectedPath) {
				t.Fatalf("archive %q does not contain %q\n%s", archiveName, expectedPath, listing)
			}
		}
	}

	checksumCommand := exec.Command("sha256sum", "-c", "checksums.txt")
	checksumCommand.Dir = distributionDirectory
	checksumOutput, err := checksumCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("verify release checksums: %v\n%s", err, checksumOutput)
	}
	for _, archiveName := range archiveNames {
		if !strings.Contains(string(checksumOutput), archiveName+": OK") {
			t.Fatalf("checksum output missing %q: %s", archiveName, checksumOutput)
		}
	}

	extractionDirectory := filepath.Join(temporaryDirectory, "extracted")
	if err := os.MkdirAll(extractionDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	amd64Archive := filepath.Join(distributionDirectory, archiveNames[0])
	extractCommand := exec.Command("tar", "-xzf", amd64Archive, "-C", extractionDirectory)
	if extractOutput, err := extractCommand.CombinedOutput(); err != nil {
		t.Fatalf("extract amd64 release: %v\n%s", err, extractOutput)
	}
	packageRoot := filepath.Join(extractionDirectory, strings.TrimSuffix(archiveNames[0], ".tar.gz"))
	installedBinary := filepath.Join(packageRoot, "bin", "craftmake")

	versionCommand := exec.Command(installedBinary, "--version")
	versionOutput, err := versionCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("release version command: %v\n%s", err, versionOutput)
	}
	if !strings.Contains(string(versionOutput), "9.8.7-test+testcommit") {
		t.Fatalf("unexpected release version output %q", versionOutput)
	}

	validateCommand := exec.Command(installedBinary,
		"validate",
		"--legacy-config", "--config", filepath.Join(repositoryRoot, "testdata", "configs", "beaverrna-step2-check.yaml"),
		"--phase", "step2-check",
	)
	validateCommand.Dir = temporaryDirectory
	validateCommand.Env = environmentWithoutWorkflowCatalog(os.Environ())
	validateOutput, err := validateCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("release catalog validation: %v\n%s", err, validateOutput)
	}
	if !strings.Contains(string(validateOutput), "valid: BeaverRNA step2-check (5 tasks, 5 submissions)") {
		t.Fatalf("unexpected release validation output %q", validateOutput)
	}

	for _, architecture := range []struct {
		archiveName       string
		expectedFileToken string
	}{
		{archiveName: archiveNames[0], expectedFileToken: "x86-64"},
		{archiveName: archiveNames[1], expectedFileToken: "ARM aarch64"},
	} {
		architectureExtraction := filepath.Join(temporaryDirectory, architecture.expectedFileToken)
		if err := os.MkdirAll(architectureExtraction, 0o755); err != nil {
			t.Fatal(err)
		}
		archivePath := filepath.Join(distributionDirectory, architecture.archiveName)
		command := exec.Command("tar", "-xzf", archivePath, "-C", architectureExtraction)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("extract %q: %v\n%s", architecture.archiveName, err, output)
		}
		binaryPath := filepath.Join(architectureExtraction, strings.TrimSuffix(architecture.archiveName, ".tar.gz"), "bin", "craftmake")
		fileCommand := exec.Command("file", binaryPath)
		fileOutput, err := fileCommand.CombinedOutput()
		if err != nil {
			t.Fatalf("inspect %q: %v\n%s", binaryPath, err, fileOutput)
		}
		if !strings.Contains(string(fileOutput), architecture.expectedFileToken) || !strings.Contains(string(fileOutput), "statically linked") {
			t.Fatalf("unexpected release binary type %q", fileOutput)
		}
	}
}
