package metrics

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type TaskMetrics struct {
	Source                     string
	Quality                    string
	WallSeconds                *float64
	UserCPUSeconds             *float64
	SystemCPUSeconds           *float64
	AllocatedCPUs              *int64
	RequestedMemoryBytes       *int64
	MaxRSSBytes                *int64
	DiskReadBytes              *int64
	DiskWriteBytes             *int64
	FilesystemInputOperations  *int64
	FilesystemOutputOperations *int64
	InputArtifactBytes         *int64
	OutputArtifactBytes        *int64
	MajorPageFaults            *int64
	MinorPageFaults            *int64
	VoluntaryContextSwitches   *int64
	InvoluntaryContextSwitches *int64
	Raw                        map[string]string
}

func ParseGNUTimeFile(path string) (*TaskMetrics, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open GNU time metrics: %w", err)
	}
	defer file.Close()

	raw := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		separator := strings.LastIndex(line, ": ")
		if separator < 0 {
			continue
		}
		raw[strings.TrimSpace(line[:separator])] = strings.TrimSpace(line[separator+2:])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read GNU time metrics: %w", err)
	}
	result := &TaskMetrics{Source: "gnu_time", Quality: "portable", Raw: raw}
	result.UserCPUSeconds = parseFloatPointer(raw["User time (seconds)"])
	result.SystemCPUSeconds = parseFloatPointer(raw["System time (seconds)"])
	result.WallSeconds = parseElapsed(raw["Elapsed (wall clock) time (h:mm:ss or m:ss)"])
	result.MaxRSSBytes = multiplyPointer(parseIntPointer(raw["Maximum resident set size (kbytes)"]), 1024)
	result.MajorPageFaults = parseIntPointer(raw["Major (requiring I/O) page faults"])
	result.MinorPageFaults = parseIntPointer(raw["Minor (reclaiming a frame) page faults"])
	result.VoluntaryContextSwitches = parseIntPointer(raw["Voluntary context switches"])
	result.InvoluntaryContextSwitches = parseIntPointer(raw["Involuntary context switches"])
	result.FilesystemInputOperations = parseIntPointer(raw["File system inputs"])
	result.FilesystemOutputOperations = parseIntPointer(raw["File system outputs"])
	return result, nil
}

func parseFloatPointer(value string) *float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func parseIntPointer(value string) *int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func multiplyPointer(value *int64, multiplier int64) *int64 {
	if value == nil {
		return nil
	}
	result := *value * multiplier
	return &result
}

func parseElapsed(value string) *float64 {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return nil
	}
	var duration time.Duration
	for index, part := range parts {
		parsed, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil
		}
		remaining := len(parts) - index - 1
		switch remaining {
		case 2:
			duration += time.Duration(parsed * float64(time.Hour))
		case 1:
			duration += time.Duration(parsed * float64(time.Minute))
		case 0:
			duration += time.Duration(parsed * float64(time.Second))
		}
	}
	seconds := duration.Seconds()
	return &seconds
}
