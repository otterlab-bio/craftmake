package colab

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type RemoteTaskMapping struct {
	WorkDirectory    string
	TempDirectory    string
	RuntimeDirectory string
	ResultPath       string
}

type NotebookCell struct {
	CellType       string         `json:"cell_type"`
	Source         string         `json:"source"`
	Metadata       map[string]any `json:"metadata"`
	Outputs        []any          `json:"outputs,omitempty"`
	ExecutionCount *int           `json:"execution_count,omitempty"`
}

type Notebook struct {
	Cells         []NotebookCell `json:"cells"`
	Metadata      map[string]any `json:"metadata"`
	Nbformat      int            `json:"nbformat"`
	NbformatMinor int            `json:"nbformat_minor"`
}

// NotebookOptions carries optional behaviour for a generated notebook.
type NotebookOptions struct {
	// DriveMount, when set, makes the bootstrap cell mount Google Drive with the
	// runtime's DriveFS binary before the steps run.
	DriveMount *DriveMountSpec
}

func BuildNotebook(manifest *protocol.TaskManifest, mapping RemoteTaskMapping) (*Notebook, error) {
	return BuildNotebookWithOptions(manifest, mapping, NotebookOptions{})
}

func BuildNotebookWithOptions(manifest *protocol.TaskManifest, mapping RemoteTaskMapping, options NotebookOptions) (*Notebook, error) {
	if manifest == nil {
		return nil, fmt.Errorf("task manifest is required")
	}
	if mapping.WorkDirectory == "" || mapping.TempDirectory == "" || mapping.RuntimeDirectory == "" || mapping.ResultPath == "" {
		return nil, fmt.Errorf("complete remote task mapping is required")
	}
	cells := []NotebookCell{{CellType: "code", Source: bootstrapSource(mapping, options), Metadata: map[string]any{}}}
	for _, step := range manifest.Steps {
		cells = append(cells, NotebookCell{CellType: "code", Source: bashSource(step, mapping), Metadata: map[string]any{}})
	}
	cells = append(cells, NotebookCell{CellType: "code", Source: finalizerSource(manifest, mapping), Metadata: map[string]any{}})
	return &Notebook{Cells: cells, Metadata: map[string]any{"craftmake": map[string]any{"run_id": manifest.RunID, "task_id": manifest.TaskID, "attempt": manifest.Attempt}}, Nbformat: 4, NbformatMinor: 5}, nil
}

func (n *Notebook) JSON() ([]byte, error) { return json.MarshalIndent(n, "", "  ") }

// BuildNotebookRedacted builds a notebook and scrubs known secrets from every
// code cell source so tokens never land in the auditable .ipynb artifact.
func BuildNotebookRedacted(manifest *protocol.TaskManifest, mapping RemoteTaskMapping, redactor *Redactor) (*Notebook, error) {
	return BuildNotebookRedactedWithOptions(manifest, mapping, redactor, NotebookOptions{})
}

func BuildNotebookRedactedWithOptions(manifest *protocol.TaskManifest, mapping RemoteTaskMapping, redactor *Redactor, options NotebookOptions) (*Notebook, error) {
	notebook, err := BuildNotebookWithOptions(manifest, mapping, options)
	if err != nil {
		return nil, err
	}
	if redactor == nil || !redactor.HasSecrets() {
		return notebook, nil
	}
	for i := range notebook.Cells {
		notebook.Cells[i].Source = redactor.Redact(notebook.Cells[i].Source)
	}
	return notebook, nil
}

// driveMountSource returns the bootstrap code that makes /content/drive usable,
// or an empty string when the workspace is not on Drive.
//
// With a Drive credential the runtime mounts Drive itself with DriveFS, which is
// the only mechanism that works over a bare kernel WebSocket. Without one it
// falls back to the historical best-effort google.colab.drive.mount(), which
// requires the Colab frontend and therefore only succeeds in a real notebook.
func driveMountSource(mapping RemoteTaskMapping, options NotebookOptions) string {
	onDrive := strings.HasPrefix(mapping.WorkDirectory, "/content/drive") || strings.HasPrefix(mapping.ResultPath, "/content/drive")
	if !onDrive {
		return ""
	}
	if options.DriveMount != nil {
		return strings.TrimRight(BuildDriveFSMountSource(*options.DriveMount), "\n")
	}
	return `if not os.path.ismount('/content/drive'):
    try:
        from google.colab import drive
        drive.mount('/content/drive', force_remount=False)
    except Exception as _e:
        print(f"Notice: auto drive.mount: {_e}")`
}

func bootstrapSource(mapping RemoteTaskMapping, options NotebookOptions) string {
	mountBlock := driveMountSource(mapping, options)
	lines := []string{
		"import os",
		"from pathlib import Path",
	}
	if mountBlock != "" {
		lines = append(lines, mountBlock)
	}
	lines = append(lines,
		"work = Path("+pythonString(mapping.WorkDirectory)+")",
		"temp = Path("+pythonString(mapping.TempDirectory)+")",
		"runtime = Path("+pythonString(mapping.RuntimeDirectory)+")",
		"result = Path("+pythonString(mapping.ResultPath)+")",
		"for path in (work, temp, runtime): path.mkdir(parents=True, exist_ok=True)",
		`os.environ["CRAFTMAKE_WORK"] = str(work)`,
		`os.environ["CRAFTMAKE_TEMP"] = str(temp)`,
		`os.environ["CRAFTMAKE_RUNTIME"] = str(runtime)`,
	)
	return strings.Join(lines, "\n") + "\n"
}

func bashSource(step protocol.StepManifest, mapping RemoteTaskMapping) string {
	lines := []string{"%%bash", "set -uo pipefail", "export CRAFTMAKE_WORK=" + shellString(mapping.WorkDirectory), "export CRAFTMAKE_TEMP=" + shellString(mapping.TempDirectory), "export CRAFTMAKE_RUNTIME=" + shellString(mapping.RuntimeDirectory)}
	for key, value := range step.Env {
		lines = append(lines, "export "+key+"="+shellString(value))
	}
	stdout := step.StdoutPath
	stderr := step.StderrPath
	if stdout == "" {
		stdout = filepath.Join(mapping.RuntimeDirectory, fmt.Sprintf("step-%d.stdout", step.Index))
	}
	if stderr == "" {
		stderr = filepath.Join(mapping.RuntimeDirectory, fmt.Sprintf("step-%d.stderr", step.Index))
	}
	exitPath := shellString(filepath.Join(mapping.RuntimeDirectory, fmt.Sprintf("step-%d.exit", step.Index)))
	startPath := shellString(filepath.Join(mapping.RuntimeDirectory, fmt.Sprintf("step-%d.start", step.Index)))
	endPath := shellString(filepath.Join(mapping.RuntimeDirectory, fmt.Sprintf("step-%d.end", step.Index)))
	// Step artifacts are keyed by the manifest's step index, which the compiler
	// assigns 1-based (see compiler.go). The finalizer must read the same keys.
	lines = append(lines,
		"set +e",
		fmt.Sprintf("date +%%s > %s", startPath),
		"("+step.Command+") >"+shellString(stdout)+" 2>"+shellString(stderr),
		"code=$?",
		fmt.Sprintf("date +%%s > %s", endPath),
		fmt.Sprintf("printf '%%s\\n' \"$code\" > %s", exitPath),
		"exit 0",
	)
	return strings.Join(lines, "\n") + "\n"
}

// finalizerSource renders the cell that collects the per-step exit codes
// written by the %%bash cells and emits the task result payload.
//
// Step artifacts are keyed by protocol.StepManifest.Index (1-based as produced
// by the compiler), so the finalizer iterates the manifest's real indices
// instead of assuming a 0-based range. A missing exit file is reported as a
// failure rather than silently treated as success.
func finalizerSource(manifest *protocol.TaskManifest, mapping RemoteTaskMapping) string {
	indices := make([]string, 0, len(manifest.Steps))
	for _, step := range manifest.Steps {
		indices = append(indices, strconv.Itoa(step.Index))
	}
	return fmt.Sprintf(`import json
from datetime import datetime, timezone
from pathlib import Path

runtime_dir = Path(%s)
result_path = Path(%s)
step_indices = [%s]


def _timestamp(name):
    path = runtime_dir / name
    if not path.exists():
        return None
    raw = path.read_text().strip()
    if not raw:
        return None
    try:
        return datetime.fromtimestamp(float(raw), tz=timezone.utc).isoformat().replace("+00:00", "Z")
    except ValueError:
        return None


steps = []
overall_exit = 0
for i in step_indices:
    exit_file = runtime_dir / f"step-{i}.exit"
    code = int(exit_file.read_text().strip()) if exit_file.exists() else -1
    if code != 0 and overall_exit == 0:
        overall_exit = code
    steps.append({
        "index": i,
        "status": "succeeded" if code == 0 else "failed",
        "exit_code": code,
        "started_at": _timestamp(f"step-{i}.start"),
        "finished_at": _timestamp(f"step-{i}.end"),
        "stdout_path": str(runtime_dir / f"step-{i}.stdout"),
        "stderr_path": str(runtime_dir / f"step-{i}.stderr"),
    })

payload = {
    "protocol_version": %d,
    "run_id": %s,
    "task_id": %s,
    "attempt": %d,
    "status": "succeeded" if overall_exit == 0 else "failed",
    "exit_code": overall_exit,
    "steps": steps,
}
for i in step_indices:
    stdout_file = runtime_dir / f"step-{i}.stdout"
    if stdout_file.exists():
        text = stdout_file.read_text().strip()
        if text:
            print(f"[step-{i} stdout]\n{text}")
    stderr_file = runtime_dir / f"step-{i}.stderr"
    if stderr_file.exists():
        err_text = stderr_file.read_text().strip()
        if err_text:
            print(f"[step-{i} stderr]\n{err_text}")
result_path.parent.mkdir(parents=True, exist_ok=True)
result_path.write_text(json.dumps(payload, indent=2))
try:
    from google.colab import drive
    drive.flush_and_unmount()
except Exception:
    pass
print("CRAFTMAKE_TASK_RESULT_BEGIN")
print(json.dumps(payload, sort_keys=True))
print("CRAFTMAKE_TASK_RESULT_END")
`, pythonString(mapping.RuntimeDirectory), pythonString(mapping.ResultPath), strings.Join(indices, ", "), protocol.Version, pythonString(manifest.RunID), pythonString(manifest.TaskID), manifest.Attempt)
}

func pythonString(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }
func shellString(value string) string  { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
