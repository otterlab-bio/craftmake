package action

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/otterlab-bio/craftmake/internal/adapters/standalone"
)

type Discovery struct {
	Name string
	Path string
	Kind standalone.ConfigKind
}

func Resolve(projectDir, name string) (string, error) {
	if !safeComponent(name) {
		return "", fmt.Errorf("action name %q is not a safe path component", name)
	}
	root := filepath.Join(projectDir, ".craftmake")
	for _, ext := range []string{".yaml", ".yml"} {
		path := filepath.Join(root, name+ext)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("action %q not found in %s", name, root)
}

func Discover(projectDir string) ([]Discovery, error) {
	if projectDir == "" {
		var err error
		projectDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve current directory: %w", err)
		}
	}
	root := filepath.Join(projectDir, ".craftmake")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read action directory %q: %w", root, err)
	}
	result := make([]Discovery, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(root, entry.Name())
		kind, err := standalone.DetectConfigKind(path)
		if err != nil {
			return nil, err
		}
		result = append(result, Discovery{Name: entry.Name()[:len(entry.Name())-len(ext)], Path: path, Kind: kind})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
