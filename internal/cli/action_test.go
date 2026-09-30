package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	colabpkg "github.com/otterlab-bio/craftmake/internal/backend/colab"
	"github.com/otterlab-bio/craftmake/internal/compiler"
)

func TestLoadActionPlanCarriesAccelerator(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".craftmake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mixed.yaml")
	content := []byte("schema_version: craftmake.action/v1\nname: mixed\nbackend: colab\ncolab:\n  session: gpu\n  default_accelerator: cpu\njobs:\n  pre:\n    steps:\n      - run: echo pre\n  train:\n    accelerator: gpu\n    needs: [pre]\n    steps:\n      - run: echo train\n  post:\n    needs: [train]\n    steps:\n      - run: echo post\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, _, err := loadActionPlan(root, "mixed", nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, task := range plan.Tasks {
		byID[task.JobID] = task.Accelerator
	}
	if byID["pre"] != "cpu" || byID["train"] != "gpu" || byID["post"] != "cpu" {
		t.Fatalf("unexpected accelerators: %#v", byID)
	}
	if len(plan.Order) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(plan.Order))
	}
}

func TestActionPlanPrintsAccelerator(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".craftmake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "accel.yaml")
	content := []byte("schema_version: craftmake.action/v1\nname: accel\nbackend: colab\ncolab:\n  session: gpu\n  default_accelerator: cpu\njobs:\n  train:\n    accelerator: gpu\n    steps:\n      - run: echo train\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadActionPlan(root, "accel", nil); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	cmd := newActionPlanCommand()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"accel", "--dir", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "accel=gpu") {
		t.Fatalf("plan output missing accel=gpu: %q", buf.String())
	}
}

func TestParallelAcceleratorOffline(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".craftmake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "parallel.yaml")
	content := []byte("schema_version: craftmake.action/v1\nname: parallel\nbackend: colab\ncolab:\n  session: gpu\n  default_accelerator: cpu\njobs:\n  data_qc:\n    accelerator: cpu\n    steps:\n      - run: echo qc\n  gpu_precompute:\n    accelerator: gpu\n    steps:\n      - run: echo gpu\n  integrate:\n    accelerator: cpu\n    needs: [data_qc, gpu_precompute]\n    steps:\n      - run: echo integrate\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, _, err := loadActionPlan(root, "parallel", nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, task := range plan.Tasks {
		byID[task.JobID] = task.Accelerator
	}
	if byID["data_qc"] != "cpu" || byID["gpu_precompute"] != "gpu" || byID["integrate"] != "cpu" {
		t.Fatalf("unexpected accelerators: %#v", byID)
	}
	// integrate must depend on both data_qc and gpu_precompute.
	var integrate *compiler.Task
	for _, task := range plan.Tasks {
		if task.JobID == "integrate" {
			integrate = task
			break
		}
	}
	if integrate == nil {
		t.Fatalf("integrate task not found: %#v", plan.Tasks)
	}
	if len(integrate.Dependencies) != 2 {
		t.Fatalf("integrate should depend on 2 tasks, got %d: %#v", len(integrate.Dependencies), integrate.Dependencies)
	}
}

func TestLoadActionPlanCompilesSelfContainedAction(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".craftmake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "build.yaml")
	content := []byte("schema_version: craftmake.action/v1\nname: build\ninputs:\n  target:\n    default: hg38\njobs:\n  compile:\n    outputs:\n      result: results/${{ inputs.target }}.txt\n    steps:\n      - run: echo ok\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, options, err := loadActionPlan(root, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Workflow != "build" || plan.Phase != "main" || len(plan.Tasks) != 1 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	if options.resolvedBackend != "local" || string(options.configKind) != "craftmake.action/v1" {
		t.Fatalf("unexpected options: %#v", options)
	}
}

func writeActionFile(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, ".craftmake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const colabActionYAML = "schema_version: craftmake.action/v1\nname: hello\nbackend: colab\ncolab:\n  session: yaml-session\n  auth_config: /yaml/colab-auth.json\n  drive_root: /content/drive/MyDrive/from-action\n  remote_root: /content/from-action\n  scratch_root: /content/scratch\n  default_accelerator: gpu\n  sync_in: true\n  sync_out: true\n  excludes:\n    - data\n  path_map:\n    /host/data: /content/drive/MyDrive/data\njobs:\n  greet:\n    steps:\n      - run: echo hello\n"

// TestLoadActionPlanCarriesColabBlock guards the regression where the `colab:`
// block of an action file was parsed and validated but never handed to the
// backend builder.
func TestLoadActionPlanCarriesColabBlock(t *testing.T) {
	root := t.TempDir()
	writeActionFile(t, root, "hello", colabActionYAML)
	_, options, err := loadActionPlan(root, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if options.colab == nil {
		t.Fatal("colab block was not carried into the run options")
	}
	if options.colab.Session != "yaml-session" || options.colab.DriveRoot != "/content/drive/MyDrive/from-action" {
		t.Fatalf("unexpected colab spec: %#v", options.colab)
	}
}

func TestResolveColabBackendConfigPrecedence(t *testing.T) {
	root := t.TempDir()
	writeActionFile(t, root, "hello", colabActionYAML)
	_, options, err := loadActionPlan(root, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Without flags the action file supplies the session and auth config.
	fromFile := resolveColabBackendConfig(options, "", "~/.config/craftmake/colab-auth.json", false, root)
	if fromFile.SessionID != "yaml-session" || fromFile.AuthConfig != "/yaml/colab-auth.json" {
		t.Fatalf("action file config was ignored: %#v", fromFile)
	}
	if fromFile.DriveRoot != "/content/drive/MyDrive/from-action" || fromFile.RemoteRoot != "/content/from-action" || fromFile.ScratchRoot != "/content/scratch" {
		t.Fatalf("action file paths were ignored: %#v", fromFile)
	}
	if fromFile.DefaultAccelerator != "gpu" || !fromFile.SyncIn || !fromFile.SyncOut || len(fromFile.Excludes) != 1 {
		t.Fatalf("action file behaviour flags were ignored: %#v", fromFile)
	}
	if fromFile.PathMap["/host/data"] != "/content/drive/MyDrive/data" {
		t.Fatalf("action file path_map was ignored: %#v", fromFile.PathMap)
	}

	// Flags win over the action file.
	fromFlags := resolveColabBackendConfig(options, "flag-session", "/flag/colab-auth.json", true, root)
	if fromFlags.SessionID != "flag-session" || fromFlags.AuthConfig != "/flag/colab-auth.json" {
		t.Fatalf("flags must win over the action file: %#v", fromFlags)
	}

	// Without a colab block only the flags apply.
	plain := resolveColabBackendConfig(commonOptions{}, "flag-session", "/flag/auth.json", false, root)
	if plain.SessionID != "flag-session" || plain.AuthConfig != "/flag/auth.json" || plain.DriveRoot != "" {
		t.Fatalf("unexpected defaults: %#v", plain)
	}
}

// TestActionRunHonorsColabSessionFromActionFile runs the real command: with the
// session declared only in the action file the run must get past backend
// construction instead of failing with "requires --colab-session".
func TestActionRunHonorsColabSessionFromActionFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRAFTMAKE_COLAB_REFRESH_TOKEN", "")
	t.Setenv("CRAFTMAKE_COLAB_DRIVE_PREFLIGHT", "off")
	// A closed port keeps the eventual control-plane call offline and instant.
	t.Setenv("CRAFTMAKE_COLAB_DOMAIN", "http://127.0.0.1:1")
	t.Setenv("CRAFTMAKE_COLAB_GAPI_DOMAIN", "http://127.0.0.1:1")

	root := t.TempDir()
	writeActionFile(t, root, "hello", colabActionYAML)
	authPath := filepath.Join(root, "colab-auth.json")
	if err := colabpkg.UpsertSessionAuth(authPath, colabpkg.SessionAuth{SessionID: "yaml-session", DriveRoot: "/content/drive/MyDrive/craftmake", MountPath: "/content/drive", ColabCredentialFile: "/tmp/c.json", DriveCredentialFile: "/tmp/d.json"}); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		command := newActionRunCommand(BuildInfo{})
		command.SetArgs(args)
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		err := command.Execute()
		if err == nil {
			return ""
		}
		return err.Error()
	}
	base := []string{"hello", "--dir", root, "--colab-auth-config", authPath, "--force"}

	if message := run(append(append([]string{}, base...), "--backend", "colab", "--colab-session", "yaml-session")...); strings.Contains(message, "requires --colab-session") {
		t.Fatalf("explicit session was rejected: %s", message)
	}
	// The same command without the flag must still work, using the YAML session.
	if message := run(append(append([]string{}, base...), "--backend", "colab")...); strings.Contains(message, "requires --colab-session") {
		t.Fatalf("action file session was ignored: %s", message)
	}
}
