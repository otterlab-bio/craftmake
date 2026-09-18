package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResourceRequestAccelerator(t *testing.T) {
	req := ResourceRequest{Cores: 2, MemoryByte: 1024, Accelerator: "gpu"}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var back ResourceRequest
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Accelerator != "gpu" {
		t.Fatalf("accelerator = %q, want gpu", back.Accelerator)
	}
}

func TestResourceRequestAcceleratorOmittedWhenEmpty(t *testing.T) {
	req := ResourceRequest{Cores: 1}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "accelerator") {
		t.Fatalf("accelerator should be omitted when empty: %s", data)
	}
}
