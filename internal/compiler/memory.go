package compiler

import (
	"fmt"
	"strconv"
	"strings"
)

func ParseMemory(value string) (int64, error) {
	trimmed := strings.TrimSpace(strings.ToUpper(value))
	if trimmed == "" {
		return 0, nil
	}
	units := []struct {
		suffix     string
		multiplier int64
	}{{"TIB", 1 << 40}, {"TB", 1_000_000_000_000}, {"GIB", 1 << 30}, {"GB", 1_000_000_000}, {"G", 1 << 30}, {"MIB", 1 << 20}, {"MB", 1_000_000}, {"M", 1 << 20}, {"KIB", 1 << 10}, {"KB", 1_000}, {"K", 1 << 10}, {"B", 1}}
	for _, unit := range units {
		if strings.HasSuffix(trimmed, unit.suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(trimmed, unit.suffix))
			parsed, err := strconv.ParseFloat(number, 64)
			if err != nil || parsed < 0 {
				return 0, fmt.Errorf("invalid memory value %q", value)
			}
			return int64(parsed * float64(unit.multiplier)), nil
		}
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid memory value %q", value)
	}
	return parsed, nil
}
