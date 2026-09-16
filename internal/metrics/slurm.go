package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func ParseSlurmParsable(line string) (*TaskMetrics, map[string]string, error) {
	fields := strings.Split(strings.TrimSpace(line), "|")
	if len(fields) < 17 {
		return nil, nil, fmt.Errorf("unexpected sacct field count %d", len(fields))
	}
	raw := map[string]string{
		"JobID": fields[0], "State": fields[1], "ExitCode": fields[2], "ElapsedRaw": fields[3], "AllocCPUS": fields[4],
		"TotalCPU": fields[5], "UserCPU": fields[6], "SystemCPU": fields[7], "MaxRSS": fields[8], "AveRSS": fields[9],
		"MaxVMSize": fields[10], "MaxDiskRead": fields[11], "MaxDiskWrite": fields[12], "ReqMem": fields[13],
		"NodeList": fields[14], "Start": fields[15], "End": fields[16],
	}
	result := &TaskMetrics{Source: "slurm_sacct", Quality: "accounting", Raw: raw}
	result.WallSeconds = parseFloatPointer(raw["ElapsedRaw"])
	result.UserCPUSeconds = parseSlurmDuration(raw["UserCPU"])
	result.SystemCPUSeconds = parseSlurmDuration(raw["SystemCPU"])
	result.AllocatedCPUs = parseIntPointer(raw["AllocCPUS"])
	result.MaxRSSBytes = parseSlurmBytes(raw["MaxRSS"])
	result.DiskReadBytes = parseSlurmBytes(raw["MaxDiskRead"])
	result.DiskWriteBytes = parseSlurmBytes(raw["MaxDiskWrite"])
	return result, raw, nil
}

func parseSlurmDuration(value string) *float64 {
	value = strings.TrimSpace(value)
	if value == "" || value == "Unknown" {
		return nil
	}
	days := 0.0
	if separator := strings.Index(value, "-"); separator >= 0 {
		parsedDays, err := strconv.ParseFloat(value[:separator], 64)
		if err != nil {
			return nil
		}
		days = parsedDays
		value = value[separator+1:]
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return nil
	}
	hours, errHour := strconv.ParseFloat(parts[0], 64)
	minutes, errMinute := strconv.ParseFloat(parts[1], 64)
	seconds, errSecond := strconv.ParseFloat(parts[2], 64)
	if errHour != nil || errMinute != nil || errSecond != nil {
		return nil
	}
	result := days*24*3600 + hours*3600 + minutes*60 + seconds
	return &result
}

func parseSlurmBytes(value string) *int64 {
	trimmed := strings.TrimSpace(strings.ToUpper(value))
	if trimmed == "" || trimmed == "UNKNOWN" {
		return nil
	}
	multipliers := map[byte]float64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40, 'P': 1 << 50}
	last := trimmed[len(trimmed)-1]
	multiplier := float64(1)
	if configured, exists := multipliers[last]; exists {
		multiplier = configured
		trimmed = trimmed[:len(trimmed)-1]
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return nil
	}
	bytes := int64(parsed * multiplier)
	return &bytes
}

func FormatSlurmMemory(memoryBytes int64) string {
	if memoryBytes <= 0 {
		return "1G"
	}
	const mebibyte = int64(1 << 20)
	megabytes := (memoryBytes + mebibyte - 1) / mebibyte
	return fmt.Sprintf("%dM", megabytes)
}

func SlurmTimeOrDefault(value string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return (24 * time.Hour).String()
}
