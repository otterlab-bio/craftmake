package metrics

import "testing"

func TestParseSlurmParsable(t *testing.T) {
	line := "123.0|COMPLETED|0:0|60|4|00:03:00|00:02:00|00:01:00|2G|1G|3G|4G|5G|8G|node01|start|end"
	parsed, _, err := ParseSlurmParsable(line)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.MaxRSSBytes == nil || *parsed.MaxRSSBytes != 2*(1<<30) {
		t.Fatalf("unexpected MaxRSS: %v", parsed.MaxRSSBytes)
	}
	if parsed.UserCPUSeconds == nil || *parsed.UserCPUSeconds != 120 {
		t.Fatalf("unexpected user CPU: %v", parsed.UserCPUSeconds)
	}
}
