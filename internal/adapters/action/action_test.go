package action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otterlab-bio/craftmake/internal/spec"
)

func TestActionDefaultAccelerator(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, ".craftmake", "accel.yaml")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("schema_version: craftmake.action/v1\nname: accel\nbackend: colab\ncolab:\n  session: gpu\n  default_accelerator: cpu\njobs:\n  pre:\n    steps:\n      - run: echo pre\n  train:\n    accelerator: gpu\n    steps:\n      - run: echo train\n")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(source, nil, root, filepath.Join(root, ".craftmake", "state"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Action.Colab == nil || loaded.Action.Colab.DefaultAccelerator != "cpu" {
		t.Fatalf("default_accelerator not parsed: %#v", loaded.Action.Colab)
	}
	// Job without accelerator should inherit default
	if got := loaded.Workflow.Jobs["pre"].Accelerator; got != "cpu" {
		t.Fatalf("job pre accelerator = %q, want cpu (inherited default)", got)
	}
	// Job with explicit accelerator should keep it
	if got := loaded.Workflow.Jobs["train"].Accelerator; got != "gpu" {
		t.Fatalf("job train accelerator = %q, want gpu", got)
	}
}

func TestLoadNormalizesSelfContainedAction(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, ".craftmake", "build.yaml")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("schema_version: craftmake.action/v1\nname: build\nbackend: local\ninputs:\n  target:\n    default: hg38\nenv:\n  TAG: ${{ inputs.target }}\njobs:\n  compile:\n    needs: []\n    env:\n      MODE: release\n    outputs:\n      result: results/${{ inputs.target }}.txt\n    steps:\n      - id: make\n        run: echo $TAG > $CRAFTMAKE_OUTPUT\n        env:\n          CFLAGS: -O3\n")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(source, nil, root, filepath.Join(root, ".craftmake", "state"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workflow.Name != "build" || loaded.Workflow.On.Otter.Workflow != "build" || loaded.Workflow.On.Otter.Phase != "main" {
		t.Fatalf("unexpected workflow identity: %#v", loaded.Workflow)
	}
	job := loaded.Workflow.Jobs["compile"]
	if job.Scope != "global" || job.Outputs["result"] != "results/hg38.txt" {
		t.Fatalf("unexpected normalized job: %#v", job)
	}
	if job.Steps[0].Run != "echo $TAG > $CRAFTMAKE_OUTPUT" || job.Env["MODE"] != "release" {
		t.Fatalf("unexpected step/job values: %#v", job)
	}
	if loaded.Context.Raw["inputs"].(map[string]any)["target"] != "hg38" {
		t.Fatalf("missing resolved input: %#v", loaded.Context.Raw)
	}
}

func TestLoadWithSourcesUsesCanonicalArgsAndEnvNamespaces(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "action.yaml")
	content := []byte("schema_version: craftmake.action/v1\nname: contract\ninputs:\n  target: {}\nenv:\n  TAG: ${{ env.BUILD_ID }}\njobs:\n  job:\n    steps:\n      - run: echo ${{ args.target }}\n        env:\n          TAG: ${{ env.TAG }}\n")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWithSources(source, map[string]string{"target": "hg38"}, map[string]string{"BUILD_ID": "ci-7"}, root, filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workflow.Jobs["job"].Steps[0].Run != "echo hg38" {
		t.Fatalf("unexpected args rendering: %#v", loaded.Workflow.Jobs["job"].Steps[0])
	}
	if loaded.Workflow.Jobs["job"].Steps[0].Env["TAG"] != "ci-7" {
		t.Fatalf("unexpected action env rendering: %#v", loaded.Workflow.Jobs["job"].Steps[0].Env)
	}
	if loaded.Context.Raw["env"].(map[string]any)["BUILD_ID"] != "ci-7" {
		t.Fatalf("missing env namespace: %#v", loaded.Context.Raw)
	}
	if loaded.Context.Raw["args"].(map[string]any)["target"] != "hg38" {
		t.Fatalf("missing args namespace: %#v", loaded.Context.Raw)
	}
}

func TestLoadRejectsMissingRequiredInput(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "action.yaml")
	content := []byte("schema_version: craftmake.action/v1\nname: build\ninputs:\n  token:\n    required: true\njobs:\n  job:\n    steps:\n      - run: echo ok\n")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(source, nil, root, filepath.Join(root, "state"))
	if err == nil || !strings.Contains(err.Error(), "input token is required") {
		t.Fatalf("expected required input error, got %v", err)
	}
}

func TestDiscoverOnlyFlatYAMLFiles(t *testing.T) {
	root := t.TempDir()
	craftmake := filepath.Join(root, ".craftmake")
	if err := os.MkdirAll(filepath.Join(craftmake, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.yaml", "b.yml", "nested/c.yaml", "ignore.txt"} {
		path := filepath.Join(craftmake, name)
		if err := os.WriteFile(path, []byte("schema_version: craftmake.action/v1\nname: "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || filepath.Base(entries[0].Path) != "a.yaml" || filepath.Base(entries[1].Path) != "b.yml" {
		t.Fatalf("unexpected discovery: %#v", entries)
	}
}

func TestLoadRendersColabSettingsFromArgsAndEnv(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "action.yaml")
	content := []byte("schema_version: craftmake.action/v1\nname: colab\nbackend: colab\ninputs:\n  session: {}\nenv:\n  RUN_ID: ci-7\ncolab:\n  session: ${{ args.session }}\n  drive_root: /content/drive/MyDrive/${{ env.RUN_ID }}\n  excludes:\n    - .git\njobs:\n  run:\n    steps:\n      - run: echo ok\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWithSources(path, map[string]string{"session": "gpu"}, map[string]string{}, root, filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Action.Colab == nil || loaded.Action.Colab.Session != "gpu" || loaded.Action.Colab.DriveRoot != "/content/drive/MyDrive/ci-7" {
		t.Fatalf("unexpected Colab config: %#v", loaded.Action.Colab)
	}
	if loaded.Context.Raw["colab"] == nil {
		t.Fatal("expected Colab context")
	}
}

func TestLoadRendersColabPathMap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "action.yaml")
	content := []byte("schema_version: craftmake.action/v1\nname: colab\nbackend: colab\ninputs:\n  session: {}\ncolab:\n  session: ${{ args.session }}\n  path_map:\n    /analysis: /content/drive/MyDrive/${{ args.session }}/analysis\njobs:\n  run:\n    steps:\n      - run: echo ok\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWithSources(path, map[string]string{"session": "gpu"}, map[string]string{}, root, filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Action.Colab == nil || loaded.Action.Colab.PathMap["/analysis"] != "/content/drive/MyDrive/gpu/analysis" {
		t.Fatalf("unexpected path_map: %#v", loaded.Action.Colab)
	}
}

func TestLoadRejectsRelativePathMapHost(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "action.yaml")
	content := []byte("schema_version: craftmake.action/v1\nname: colab\nbackend: colab\ncolab:\n  path_map:\n    analysis: /content/drive/MyDrive/analysis\njobs:\n  run:\n    steps:\n      - run: echo ok\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithSources(path, nil, map[string]string{}, root, filepath.Join(root, "state"))
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("expected absolute path_map error, got %v", err)
	}
}

var _ = spec.CurrentVersion
