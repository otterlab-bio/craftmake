package colab

import (
	"context"
	"fmt"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/backend"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type fakeResultReader struct{ contents map[string]string }

func (f fakeResultReader) ReadResultFile(_ context.Context, path string) ([]byte, error) {
	if value, ok := f.contents[path]; ok {
		return []byte(value), nil
	}
	return nil, fmt.Errorf("%s not found", path)
}

func encodeResult(manifest *protocol.TaskManifest) string {
	result := fmt.Sprintf("CRAFTMAKE_TASK_RESULT_BEGIN\n{\"protocol_version\":%d,\"run_id\":\"%s\",\"task_id\":\"%s\",\"attempt\":%d,\"status\":\"succeeded\",\"steps\":[]}\nCRAFTMAKE_TASK_RESULT_END\n", protocol.Version, manifest.RunID, manifest.TaskID, manifest.Attempt)
	return result
}

func TestRecoverSubmissionAcceptsOnlyMatchingRemoteResult(t *testing.T) {
	manifest := &protocol.TaskManifest{RunID: "run-1", TaskID: "task-1", Attempt: 2}
	path := "/content/craftmake/runtime/task-1/result.json"
	reader := fakeResultReader{contents: map[string]string{path: encodeResult(manifest)}}
	backendInstance := &Backend{Config: Config{DriveRoot: "/content/craftmake"}, ResultReader: reader}
	result, err := backendInstance.RecoverSubmission(context.Background(), backend.RecoveryRequest{SubmissionID: "sub-1", Manifests: []*protocol.TaskManifest{manifest}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, ok := result.Tasks["task-1"]
	if !ok || outcome.Result == nil || outcome.Result.TaskResult.TaskID != "task-1" {
		t.Fatalf("expected recovered result: %#v", outcome)
	}
	mismatched := &protocol.TaskManifest{RunID: "run-1", TaskID: "task-other", Attempt: 1}
	result, err = backendInstance.RecoverSubmission(context.Background(), backend.RecoveryRequest{SubmissionID: "sub-2", Manifests: []*protocol.TaskManifest{mismatched}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, ok = result.Tasks["task-other"]; !ok || outcome.Err == nil {
		t.Fatalf("expected mismatch failure: %#v", outcome)
	}
}
