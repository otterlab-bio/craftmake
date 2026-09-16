package runtime

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
)

func CompressLog(path string) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	compressedPath := path + ".gz"
	temporaryPath := compressedPath + ".tmp"
	output, err := os.Create(temporaryPath)
	if err != nil {
		return "", err
	}
	writer := gzip.NewWriter(output)
	_, copyErr := io.Copy(writer, input)
	closeWriterErr := writer.Close()
	closeOutputErr := output.Close()
	if copyErr != nil || closeWriterErr != nil || closeOutputErr != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("compress log %q", path)
	}
	if err := os.Rename(temporaryPath, compressedPath); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return compressedPath, nil
}
