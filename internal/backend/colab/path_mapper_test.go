package colab

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPathMapperRelativeUnderHostRoot(t *testing.T) {
	root := t.TempDir()
	m := PathMapper{HostRoot: root, RemoteRoot: "/content/drive/MyDrive/project"}
	got, err := m.Map(filepath.Join(root, "work", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/content/drive/MyDrive/project", "work", "a.txt")
	if got != want {
		t.Fatalf("Map = %q, want %q", got, want)
	}
}

func TestPathMapperExplicitPathMapAllowsAbsoluteOutsideRoot(t *testing.T) {
	root := t.TempDir()
	m := PathMapper{HostRoot: root, RemoteRoot: "/content/drive/MyDrive/project", PathMap: map[string]string{"/analysis": "/content/drive/MyDrive/analysis"}}
	got, err := m.Map("/analysis/sample.bam")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/content/drive/MyDrive/analysis/sample.bam" {
		t.Fatalf("Map = %q", got)
	}
}

func TestPathMapperRejectsUnmappedAbsoluteOutsideRoot(t *testing.T) {
	root := t.TempDir()
	m := PathMapper{HostRoot: root, RemoteRoot: "/content/drive/MyDrive/project"}
	_, err := m.Map("/etc/passwd")
	if err == nil || !strings.Contains(err.Error(), "not mapped") {
		t.Fatalf("expected unmapped rejection, got %v", err)
	}
}

func TestPathMapperRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	m := PathMapper{HostRoot: root, RemoteRoot: "/content/drive/MyDrive/project"}
	_, err := m.Map(filepath.Join(root, "..", "escape.txt"))
	if err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestPathMapperLongestPathMapWins(t *testing.T) {
	m := PathMapper{HostRoot: "/project", RemoteRoot: "/content/drive/MyDrive/project", PathMap: map[string]string{"/analysis": "/content/drive/MyDrive/analysis", "/analysis/deep": "/content/drive/MyDrive/deep"}}
	got, err := m.Map("/analysis/deep/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/content/drive/MyDrive/deep/x.txt" {
		t.Fatalf("Map = %q", got)
	}
}
