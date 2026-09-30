package colab

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultWorkspaceArchiveLimit bounds the compressed workspace archive that is
// transferred through the Jupyter kernel. Large data belongs on Drive, not in a
// kernel payload.
const defaultWorkspaceArchiveLimit = 32 << 20

// workspaceArchiveLimit returns the configured archive size limit. It can be
// raised with CRAFTMAKE_COLAB_SYNC_MAX_BYTES.
func workspaceArchiveLimit() int {
	if raw := strings.TrimSpace(os.Getenv("CRAFTMAKE_COLAB_SYNC_MAX_BYTES")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			return value
		}
	}
	return defaultWorkspaceArchiveLimit
}

// BuildWorkspaceArchive archives the local workspace as a tar.gz whose member
// names are the remote paths the files must be written to. hostToRemote maps
// each local file to its remote location (honoring the action's path_map).
func BuildWorkspaceArchive(localRoot string, hostToRemote PathMapper, excludes []string) ([]byte, error) {
	if strings.TrimSpace(localRoot) == "" {
		return nil, fmt.Errorf("workspace sync requires a local root")
	}
	if hostToRemote.RemoteRoot == "" {
		return nil, fmt.Errorf("workspace sync requires a remote root")
	}
	limit := workspaceArchiveLimit()
	var buffer bytes.Buffer
	gzipWriter, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	tarWriter := tar.NewWriter(gzipWriter)
	writeErr := filepath.Walk(localRoot, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(localRoot, current)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if excluded(relative, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Only regular files are transferred: symlinks could point anywhere on
		// the host and would silently widen the upload.
		if !info.Mode().IsRegular() {
			return nil
		}
		remote, err := hostToRemote.Map(current)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		header := &tar.Header{
			Name:    strings.TrimPrefix(filepath.ToSlash(remote), "/"),
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: info.ModTime(),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if _, err := tarWriter.Write(data); err != nil {
			return err
		}
		if buffer.Len() > limit {
			return fmt.Errorf("workspace archive exceeds the %d byte sync limit; exclude large inputs (CRAFTMAKE_COLAB_SYNC_MAX_BYTES overrides)", limit)
		}
		return nil
	})
	if writeErr == nil {
		writeErr = tarWriter.Close()
	} else {
		_ = tarWriter.Close()
	}
	if writeErr == nil {
		writeErr = gzipWriter.Close()
	} else {
		_ = gzipWriter.Close()
	}
	if writeErr != nil {
		return nil, writeErr
	}
	if buffer.Len() > limit {
		return nil, fmt.Errorf("workspace archive exceeds the %d byte sync limit", limit)
	}
	return buffer.Bytes(), nil
}

// ExtractWorkspaceArchive writes a downloaded archive into the local workspace.
// Member names are relative to remoteRoot; the absolute remote path is
// reconstructed before mapping, so entries under a path_map remote prefix are
// restored to their host prefix and everything else lands under localRoot.
func ExtractWorkspaceArchive(archive []byte, localRoot string, remoteRoot string, pathMap map[string]string, excludes []string) error {
	if strings.TrimSpace(localRoot) == "" {
		return fmt.Errorf("workspace sync requires a local root")
	}
	if len(archive) > workspaceArchiveLimit() {
		return fmt.Errorf("downloaded workspace archive exceeds the %d byte sync limit", workspaceArchiveLimit())
	}
	reverse := reversePathMap(pathMap)
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("open workspace archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read workspace archive: %w", err)
		}
		if !header.FileInfo().Mode().IsRegular() {
			continue
		}
		relative := path.Clean(strings.TrimPrefix(filepath.ToSlash(header.Name), "/"))
		if relative == "." || strings.HasPrefix(relative, "../") {
			continue
		}
		if excluded(relative, excludes) {
			continue
		}
		target, err := localPathFor(relative, localRoot, remoteRoot, reverse)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeArchiveFile(target, tarReader); err != nil {
			return err
		}
	}
}

func writeArchiveFile(target string, reader io.Reader) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// localPathFor maps a root-relative archive member onto a local absolute path.
// It returns an empty string when the entry is outside the syncable area and
// must be ignored.
func localPathFor(relative, localRoot, remoteRoot string, reverse map[string]string) (string, error) {
	remote := path.Join("/", filepath.ToSlash(remoteRoot), relative)
	for remotePrefix, hostPrefix := range reverse {
		trimmed := strings.TrimSuffix(remotePrefix, "/")
		if remote == trimmed || strings.HasPrefix(remote, trimmed+"/") {
			rest := strings.TrimPrefix(remote, trimmed)
			target := filepath.Join(hostPrefix, filepath.FromSlash(strings.TrimPrefix(rest, "/")))
			clean, err := filepath.Abs(target)
			if err != nil {
				return "", err
			}
			if !isWithin(hostPrefix, clean) {
				return "", fmt.Errorf("workspace archive entry %q escapes mapped prefix %q", relative, hostPrefix)
			}
			return clean, nil
		}
	}
	if remoteRoot == "" || !isWithin(remoteRoot, remote) {
		return "", nil
	}
	within, err := filepath.Rel(filepath.FromSlash(remoteRoot), filepath.FromSlash(remote))
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(localRoot, within))
	if err != nil {
		return "", err
	}
	if !isWithin(localRoot, target) {
		return "", fmt.Errorf("workspace archive entry %q escapes %q", relative, localRoot)
	}
	return target, nil
}

func reversePathMap(pathMap map[string]string) map[string]string {
	if len(pathMap) == 0 {
		return nil
	}
	reverse := make(map[string]string, len(pathMap))
	for host, remote := range pathMap {
		reverse[strings.TrimSuffix(filepath.ToSlash(remote), "/")] = host
	}
	return reverse
}
