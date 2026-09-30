// Command notebookdump prints the Colab notebook that the craftmake colab
// backend would execute for a one-step task manifest. It is a debugging aid for
// verifying the generated bootstrap / step / finalizer cells locally.
//
// Usage: go run ./verify/notebookdump
package main

import (
	"fmt"

	colab "github.com/otterlab-bio/craftmake/internal/backend/colab"
	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

func main() {
	manifest := &protocol.TaskManifest{
		ProtocolVersion: protocol.Version,
		RunID:           "run-demo",
		TaskID:          "demo/main/greet",
		Attempt:         1,
		Steps: []protocol.StepManifest{
			{Index: 1, Name: "step-1", Command: "echo hello"},
		},
	}
	mapping := colab.RemoteTaskMapping{
		WorkDirectory:    "/content/drive/MyDrive/craftmake/work",
		TempDirectory:    "/content/tmp",
		RuntimeDirectory: "/content/drive/MyDrive/craftmake/runtime/demo/main/greet",
		ResultPath:       "/content/drive/MyDrive/craftmake/runtime/demo/main/greet/result.json",
	}
	notebook, err := colab.BuildNotebook(manifest, mapping)
	if err != nil {
		panic(err)
	}
	data, err := notebook.JSON()
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))
}
