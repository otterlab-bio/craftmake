package colab

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

const resultBegin = "CRAFTMAKE_TASK_RESULT_BEGIN"
const resultEnd = "CRAFTMAKE_TASK_RESULT_END"

func DecodeTaskResult(output string) (*protocol.TaskResult, error) {
	lines := strings.Split(output, "\n")
	begin, end := -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case resultBegin:
			begin = i
		case resultEnd:
			if begin >= 0 {
				end = i
			}
		}
	}
	if begin < 0 || end <= begin+1 {
		return nil, fmt.Errorf("task result sentinel is missing or incomplete")
	}
	payload := strings.TrimSpace(strings.Join(lines[begin+1:end], "\n"))
	var result protocol.TaskResult
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		return nil, fmt.Errorf("decode task result: %w", err)
	}
	if result.ProtocolVersion != protocol.Version {
		return nil, fmt.Errorf("unsupported task result protocol version %d", result.ProtocolVersion)
	}
	return &result, nil
}

func DecodeTaskResultFile(path string) (*protocol.TaskResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open task result %q: %w", path, err)
	}
	defer file.Close()
	var builder strings.Builder
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		builder.WriteString(scanner.Text())
		builder.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read task result %q: %w", path, err)
	}
	return DecodeTaskResult(builder.String())
}
