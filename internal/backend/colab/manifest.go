package colab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type WorkspaceManifest struct {
	Root   string          `json:"root"`
	Files  []WorkspaceFile `json:"files"`
	SHA256 string          `json:"sha256"`
}

type WorkspaceFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

func BuildWorkspaceManifest(ctx context.Context, root string, excludes []string) (WorkspaceManifest, error) {
	root = filepath.Clean(root)
	manifest := WorkspaceManifest{Root: root}
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if excluded(rel, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, WorkspaceFile{Path: filepath.ToSlash(rel), Bytes: info.Size(), Mode: uint32(info.Mode().Perm()), SHA256: hex.EncodeToString(digest[:])})
		return nil
	})
	if err != nil {
		return WorkspaceManifest{}, fmt.Errorf("build workspace manifest: %w", err)
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	canonical := struct {
		Root  string          `json:"root"`
		Files []WorkspaceFile `json:"files"`
	}{manifest.Root, manifest.Files}
	data, err := json.Marshal(canonical)
	if err != nil {
		return WorkspaceManifest{}, err
	}
	digest := sha256.Sum256(data)
	manifest.SHA256 = hex.EncodeToString(digest[:])
	return manifest, nil
}
