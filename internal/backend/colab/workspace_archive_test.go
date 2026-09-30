package colab

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWorkspaceFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceArchiveRoundTrip(t *testing.T) {
	localRoot := t.TempDir()
	writeWorkspaceFile(t, filepath.Join(localRoot, "main.go"), "package main\n")
	writeWorkspaceFile(t, filepath.Join(localRoot, "data", "input.txt"), "hello\n")
	writeWorkspaceFile(t, filepath.Join(localRoot, ".craftmake", "state", "state.sqlite"), "stale\n")

	remoteRoot := "/content/drive/MyDrive/work"
	mapper := PathMapper{HostRoot: localRoot, RemoteRoot: remoteRoot}
	archive, err := BuildWorkspaceArchive(localRoot, mapper, []string{".craftmake/state", ".git"})
	if err != nil {
		t.Fatal(err)
	}
	names := archiveMemberNames(t, archive)
	if !containsString(names, "content/drive/MyDrive/work/data/input.txt") {
		t.Fatalf("sync-in member names must be absolute remote paths: %#v", names)
	}
	if containsString(names, "content/drive/MyDrive/work/.craftmake/state/state.sqlite") {
		t.Fatalf("excluded state directory must not be archived: %#v", names)
	}

	// Sync-out archives use names relative to the remote workspace root, so the
	// round trip is expressed the same way the runtime reports them.
	restore := t.TempDir()
	if err := ExtractWorkspaceArchive(archiveWithMember(t, "data/input.txt", "hello\n"), restore, remoteRoot, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(restore, "data", "input.txt"))
	if err != nil {
		t.Fatalf("nested file was not restored: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("unexpected content %q", data)
	}
}

// TestWorkspaceArchiveUsesPathMap verifies the action's path_map is applied in
// both directions for destinations inside the sync root: a file under a mapped
// host prefix is uploaded to its mapped remote location and returns to the host
// prefix on sync-out.
func TestWorkspaceArchiveUsesPathMap(t *testing.T) {
	localRoot := t.TempDir()
	hostData := filepath.Join(localRoot, "external")
	writeWorkspaceFile(t, filepath.Join(hostData, "reads.fastq"), "ACGT\n")
	writeWorkspaceFile(t, filepath.Join(localRoot, "notes.md"), "notes\n")

	remoteRoot := "/content/work"
	pathMap := map[string]string{hostData: remoteRoot + "/data"}
	mapper := PathMapper{HostRoot: localRoot, RemoteRoot: remoteRoot, PathMap: pathMap}
	archive, err := BuildWorkspaceArchive(localRoot, mapper, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := archiveMemberNames(t, archive)
	if !containsString(names, "content/work/data/reads.fastq") {
		t.Fatalf("path_map entry was not honored: %#v", names)
	}
	if !containsString(names, "content/work/notes.md") {
		t.Fatalf("unmapped file should follow the remote root: %#v", names)
	}

	restore := t.TempDir()
	outArchive := archiveWithMembers(t, map[string]string{"data/reads.fastq": "ACGT\n", "notes.md": "notes\n"})
	if err := ExtractWorkspaceArchive(outArchive, restore, remoteRoot, pathMap, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hostData, "reads.fastq")); err != nil {
		t.Fatalf("mapped file did not return to its host prefix: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restore, "notes.md")); err != nil {
		t.Fatalf("unmapped file did not return under the project root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restore, "data", "reads.fastq")); err == nil {
		t.Fatal("mapped file must not be duplicated under the project root")
	}
}

func TestWorkspaceArchiveSkipsSymlinksAndRespectsLimit(t *testing.T) {
	localRoot := t.TempDir()
	writeWorkspaceFile(t, filepath.Join(localRoot, "real.txt"), "real\n")
	if err := os.Symlink(filepath.Join(localRoot, "real.txt"), filepath.Join(localRoot, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	mapper := PathMapper{HostRoot: localRoot, RemoteRoot: "/content/work"}
	archive, err := BuildWorkspaceArchive(localRoot, mapper, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := archiveMemberNames(t, archive)
	if containsString(names, "content/work/link.txt") {
		t.Fatalf("symlinks must not be archived: %#v", names)
	}
	if !containsString(names, "content/work/real.txt") {
		t.Fatalf("regular file missing: %#v", names)
	}

	t.Setenv("CRAFTMAKE_COLAB_SYNC_MAX_BYTES", "64")
	writeWorkspaceFile(t, filepath.Join(localRoot, "big.bin"), strings.Repeat("x", 4096))
	if _, err := BuildWorkspaceArchive(localRoot, mapper, nil); err == nil {
		t.Fatal("expected the archive size limit to be enforced")
	}
}

func TestExtractWorkspaceArchiveRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"content/work/../../../escape.txt", "../escape.txt"} {
		archive := archiveWithMember(t, name, "pwned\n")
		if err := ExtractWorkspaceArchive(archive, root, "/content/work", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "escape.txt" {
			t.Fatal("archive entry escaped the workspace root")
		}
	}
}

func archiveMemberNames(t *testing.T, archive []byte) []string {
	t.Helper()
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	var names []string
	for {
		header, err := tarReader.Next()
		if err != nil {
			break
		}
		names = append(names, header.Name)
	}
	return names
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
