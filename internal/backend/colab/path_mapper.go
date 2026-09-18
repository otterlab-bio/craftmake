package colab

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// PathMapper maps local host paths to remote Colab/Drive paths. Paths under
// HostRoot are mapped relative to RemoteRoot. Absolute paths outside HostRoot
// are only allowed when they match an explicit PathMap entry (host prefix ->
// remote prefix). Any other absolute path, or a path that escapes the mapped
// root via "..", is rejected.
type PathMapper struct {
	HostRoot   string
	RemoteRoot string
	PathMap    map[string]string
}

// Map resolves a host path to its remote equivalent.
func (m PathMapper) Map(path string) (string, error) {
	host, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve host path: %w", err)
	}
	if m.RemoteRoot == "" {
		return "", fmt.Errorf("remote root is required")
	}
	// 1. Explicit path_map entry (longest host prefix wins).
	if mapped, ok := m.longestPathMap(host); ok {
		return mapped, nil
	}
	// 2. Relative mapping under HostRoot.
	root, err := filepath.Abs(m.HostRoot)
	if err != nil {
		return "", fmt.Errorf("resolve host root: %w", err)
	}
	relative, err := filepath.Rel(root, host)
	if err != nil {
		return "", fmt.Errorf("map path: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside host root %q and not mapped by path_map", path, m.HostRoot)
	}
	return filepath.Join(m.RemoteRoot, relative), nil
}

// longestPathMap returns the remote path for the longest host prefix in PathMap
// that is a parent of (or equal to) the given host path. It rejects traversal
// in the remainder.
func (m PathMapper) longestPathMap(host string) (string, bool) {
	if len(m.PathMap) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(m.PathMap))
	for key := range m.PathMap {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, key := range keys {
		absKey, err := filepath.Abs(key)
		if err != nil {
			continue
		}
		if !isWithin(absKey, host) {
			continue
		}
		relative, err := filepath.Rel(absKey, host)
		if err != nil {
			continue
		}
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		return filepath.Join(m.PathMap[key], relative), true
	}
	return "", false
}

// isWithin reports whether child is equal to or under parent.
func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
