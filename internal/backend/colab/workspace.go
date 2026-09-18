package colab

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type WorkspaceSyncRequest struct {
	RunID      string
	LocalRoot  string
	RemoteRoot string
	Direction  string
	Excludes   []string
}

type WorkspaceSyncer interface {
	SyncIn(context.Context, WorkspaceSyncRequest) error
	SyncOut(context.Context, WorkspaceSyncRequest) error
}

// FileWorkspaceSyncer is an offline/test implementation. A production adapter
// can use Drive API/FUSE or an rsync-like transport behind the same seam.
type FileWorkspaceSyncer struct{}

func (FileWorkspaceSyncer) SyncIn(ctx context.Context, request WorkspaceSyncRequest) error {
	request.Direction = "in"
	return syncWorkspace(ctx, request)
}

func (FileWorkspaceSyncer) SyncOut(ctx context.Context, request WorkspaceSyncRequest) error {
	request.Direction = "out"
	return syncWorkspace(ctx, request)
}

func syncWorkspace(ctx context.Context, request WorkspaceSyncRequest) error {
	if request.LocalRoot == "" || request.RemoteRoot == "" {
		return fmt.Errorf("workspace sync requires local_root and remote_root")
	}
	if request.Direction != "" && request.Direction != "in" && request.Direction != "out" {
		return fmt.Errorf("unsupported workspace sync direction %q", request.Direction)
	}
	return copyTree(ctx, request.LocalRoot, request.RemoteRoot, request.Excludes)
}

func copyTree(ctx context.Context, sourceRoot, targetRoot string, excludes []string) error {
	sourceRoot = filepath.Clean(sourceRoot)
	targetRoot = filepath.Clean(targetRoot)
	if sourceRoot == targetRoot {
		return fmt.Errorf("workspace sync source and target must differ")
	}
	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		return err
	}
	return filepath.Walk(sourceRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceRoot, path)
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
		target := filepath.Join(targetRoot, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func excluded(rel string, excludes []string) bool {
	rel = filepath.ToSlash(rel)
	for _, item := range excludes {
		item = strings.Trim(filepath.ToSlash(item), "/")
		if item == "" {
			continue
		}
		if rel == item || strings.HasPrefix(rel, item+"/") {
			return true
		}
	}
	return false
}
