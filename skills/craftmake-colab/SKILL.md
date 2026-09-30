---
name: craftmake-colab
description: Author and run Craftmake action workflows locally or on Google Colab GPU/CPU runtimes with Google Drive cloud persistence, assisted OAuth session management, and parameter contracts.
---

# Craftmake Colab & Action Workflow Guide

This skill guides you through authoring, planning, executing, and debugging **Craftmake Action workflows** and the **Google Colab remote backend** with Google Drive integration.

---

## 1. Action Workflow Schema (`craftmake.action/v1`)

Action workflows are self-contained task DAG definitions stored under `.craftmake/<name>.yaml` in the project repository.

### Action File Template

```yaml
schema_version: craftmake.action/v1
name: my_action
backend: colab                  # "local" or "colab" (can be overridden via CLI --backend)
inputs:
  sample_id:
    default: "sample-01"
  epochs:
    default: "10"
jobs:
  train:
    steps:
      - run: |
          echo "Training sample ${{ args.sample_id }} for ${{ args.epochs }} epochs"
          nvidia-smi
```

---

## 2. Strict Parameter Contracts

Craftmake strictly validates parameter source namespaces:

| Syntax | Source | Description | Example |
|---|---|---|---|
| `${{ args.<name> }}` | CLI argument | Passed via `--arg KEY=VALUE` (or deprecated `--input`). Must exist in `inputs` block. | `craftmake action run ... --arg sample_id=sample-02` |
| `${{ env.<NAME> }}` | Environment variable | Captured from read-only process environment snapshot at run start. | `${{ env.CUDA_VISIBLE_DEVICES }}` |
| `${{ config.<name> }}` | Config file | Resolved from an associated configuration file passed via `--config`. | `${{ config.data_root }}` |

---

## 3. Session Authentication & Management

Colab sessions store credentials and Drive mount paths in `~/.config/craftmake/colab-auth.json` (`0600`).

### 3.1 Initial Login (Once per Session)

```bash
# Interactive loopback OAuth login. No client configuration is required:
# craftmake bundles the public Cloud SDK "installed app" client that the
# official Colab CLI also ships (project "colab-cli").
craftmake colab auth login --session gpu
```
- Starts an ephemeral loopback HTTP server on `127.0.0.1` and uses PKCE.
- Automatically opens your default browser for authorization.
- Saves the refresh token to `~/.config/craftmake/credentials/<session>.json` (`0600`).
- Override the client with `CRAFTMAKE_COLAB_CLIENT_ID`/`CRAFTMAKE_COLAB_CLIENT_SECRET`,
  the `--client-id`/`--client-secret` flags, or
  `make build COLAB_CLIENT_ID=... COLAB_CLIENT_SECRET=...` (flag > env > bundled).
- Google binds a refresh token to the client that minted it: after switching
  clients, re-run `colab auth login` (the failure is reported as
  `invalid_grant`/`unauthorized_client` with that hint, and `oauth_client_id` in
  `colab-auth.json` records which client the session was authorized with).
- **No re-authentication needed**: Subsequent task runs silently refresh access tokens JIT in <0.5s.

### 3.2 Inspect & Preflight

```bash
# Show non-secret session configuration
craftmake colab auth show --session gpu

# Run offline doctor checks
craftmake colab doctor --session gpu
```

---

## 4. Google Drive Integration

Craftmake uses Google Drive as the durable remote workspace
(`/content/drive/MyDrive/<root>`). The runtime VM is disposable; Drive survives
between jobs.

### 4.1 Drive authorization is one-time per account

Google requires explicit consent before a runtime may access Drive files.
Consent is granted **once per Google account** and remembered server-side by
Google; craftmake stores only your refresh token.

```bash
# Ask for Drive authorization for this session
craftmake colab drive mount --session gpu --authorize
```

1. Craftmake acquires a short-lived probe runtime and asks Colab to propagate
   Drive credentials (`dfs_ephemeral`) as a **dry run**.
2. Already authorized → it prints `Google Drive is already authorized` and exits
   without any browser prompt.
3. Otherwise it prints Google's authorization URL and waits until the grant is
   detected (polls every 3s; press **Enter** to re-check immediately).

Every later runtime repeats only step 1 — a silent dry-run probe plus a real
credential propagation — so "authorize once" refers to the user consent, not to
the per-runtime mount.

**Runs fail fast without authorization.** Before any task is scheduled the
backend probes the consent and aborts with the URL and the exact command to run.
Sessions that do not need Drive can skip the probe with
`CRAFTMAKE_COLAB_DRIVE_PREFLIGHT=off`.

### 4.2 Action files may declare their Colab configuration

```yaml
schema_version: craftmake.action/v1
name: mixed_pipeline
backend: colab
colab:
  session: gpu                    # --colab-session becomes optional
  auth_config: ~/.config/craftmake/colab-auth.json
  drive_root: /content/drive/MyDrive/craftmake
  remote_root: /content/craftmake
  scratch_root: /content
  default_accelerator: cpu
  sync_in: true                   # upload the local project before the run
  sync_out: true                  # mirror the remote workspace back after it
  excludes: [data]
  path_map:
    /host/data: /content/drive/MyDrive/craftmake/data
jobs:
  preprocess:
    accelerator: cpu
    steps:
      - run: echo preprocess
```

Precedence is **CLI flag > action file > session/auth defaults**.

### 4.3 Workspace sync (`sync_in` / `sync_out`)

Both directions are opt-in and travel through the Jupyter kernel, so no Drive API
client or extra service is needed:

- `sync_in` archives the local project (tar.gz) and extracts it into
  `<drive_root>/work` once per run.
- `sync_out` archives the remote workspace root and merges it into
  `<project>/.craftmake/colab-workspace/`.
- The local mirror is excluded from the upload, so repeated runs never nest
  `work/work/...`.
- Symlinks are never uploaded, escaping archive paths are rejected, and
  craftmake's remote `runtime/` directory is excluded from the download.
- Payloads travel as kernel messages, so both directions are bounded by
  `CRAFTMAKE_COLAB_SYNC_MAX_BYTES` (default 32 MiB). Keep large data on Drive.

### 4.4 Writing to Drive from a step

The runtime bootstrap already mounts Drive when `drive_root` is configured, so
only fall back to an explicit mount, and keep the flush guarded:

```python
import os
from pathlib import Path

if not os.path.ismount('/content/drive'):
    try:
        from google.colab import drive
        drive.mount('/content/drive', force_remount=False)
    except Exception as exc:
        print('Notice: Drive is not mounted:', exc)

output_dir = Path('/content/drive/MyDrive/my_project/checkpoints')
output_dir.mkdir(parents=True, exist_ok=True)
(output_dir / 'model.pt').write_text('model weights')

# The task finalizer always flushes FUSE writes before the instance is released.
try:
    from google.colab import drive
    drive.flush_and_unmount()
except Exception as exc:
    print('Notice: flush skipped:', exc)
```

---

## 5. Command Reference

### Action Commands

```bash
# List all discovered actions under .craftmake/*.yaml
craftmake action list

# Inspect and compile action DAG without execution
craftmake action plan <name> [--arg KEY=VALUE]

# Execute action locally
craftmake action run <name> --backend local [--arg KEY=VALUE]

# Execute action on Google Colab (--colab-session is optional when the action
# file declares colab.session)
craftmake action run <name> --backend colab [--colab-session gpu] [--force]
```

### Session Commands

```bash
craftmake colab auth login --session gpu
craftmake colab auth configure --config ~/.config/craftmake/colab-auth.json \
  --session gpu --drive-root /content/drive/MyDrive/craftmake
craftmake colab auth show --session gpu
craftmake colab doctor --session gpu
craftmake colab drive mount --session gpu --authorize
```

### Run Inspection & Recovery

Run ids contain a random suffix, so look results up per run rather than by
sorting run directories.

```bash
# View controller event logs
cat .craftmake/state/runs/<run_id>/controller.jsonl

# View task results and step stdout/stderr (step indexes are 1-based)
cat .craftmake/state/runs/<run_id>/tasks/<task_id>/attempt-001/result.json
cat .craftmake/state/runs/<run_id>/tasks/<task_id>/attempt-001/step-1.stdout

# Resume interrupted run
craftmake resume --backend colab --colab-session gpu --run <run_id>
```

---

### 4.5 Limitations verified against the live service

- **`/content/drive` is not a mount with the WebSocket executor.** `drive.mount()`
  needs the Colab frontend and raises over a bare kernel WebSocket, so writes
  under `/content/drive/...` land on the ephemeral VM disk while the step still
  exits 0. Prefer `sync_out`, or call the Drive REST API with the propagated
  credentials.
- **Drive consent is per runtime, not per account.** A newly assigned runtime
  reports the Drive credential as unauthorized again, so `colab drive mount
  --authorize` cannot pre-authorize a later ephemeral run; consent must be given
  while the run that needs it is waiting (the run-level prompt is the working
  path).
- **`sync_in` before the notebook can be too early** on a fresh runtime; `sync_out`
  after the notebook is reliable, which is why `sync_in` defaults to off.

---

## 6. Troubleshooting & Operational Rules

1. **HTTP 412 (TooManyAssignmentsError)**:
   - Google Colab limits accounts to 1 concurrent runtime on free tiers.
   - Craftmake automatically detects 412 errors, queries existing dangling
     assignments via `GET /v1/assignments`, and unassigns them automatically
     before retrying.
2. **"Drive mount is not authorized"**:
   - The preflight rejected the run before executing any task; run the exact
     command printed in the message (`colab drive mount ... --authorize`).
   - The probe runtime is always released, so a failed preflight leaks nothing.
   - Set `CRAFTMAKE_COLAB_DRIVE_PREFLIGHT=off` for sessions that never touch Drive.
3. **Failed steps are never reported as successful**:
   - Each step writes `step-<index>.exit`; the finalizer fails the task when a
     step exits non-zero, and a *missing* exit code also fails the step.
   - Local step logs are materialized at
     `.craftmake/state/runs/<run_id>/tasks/<task_id>/attempt-001/step-<index>.stdout`.
4. **Missing files on Google Drive**:
   - The task finalizer calls `drive.flush_and_unmount()` before the instance is
     released; steps that must guarantee durability should flush too (guarded).
   - Craftmake releases the VM immediately after each task, so uncommitted FUSE
     buffers are lost if they are never flushed.
5. **Workspace sync limits**:
   - `sync_in`/`sync_out` travel as kernel messages: raise
     `CRAFTMAKE_COLAB_SYNC_MAX_BYTES` only for code-sized payloads, and keep
     large data on Drive.
   - Downloaded files are mirrored into `.craftmake/colab-workspace/`; that
     directory is excluded from uploads.
6. **Automated test isolation**:
   - Inside test suites (`go test`), the browser launcher is automatically
     silenced (`isRunningInTest()`).
   - Use mock servers for the control plane and set `CRAFTMAKE_NO_BROWSER=1` for
     headless environments.
7. **Local credential fallback**:
   - `resolveColabRefreshToken` automatically detects and prioritizes
     `~/.config/craftmake/credentials/<session>.json`, so tests that build a
     Colab backend must isolate `HOME`.
8. **Optional Drive file service**:
   - Setting `CRAFTMAKE_COLAB_DRIVE_FILES_URL` to a service exposing
     `/drive/read` and `/drive/write` enables Drive-backed log materialization
     and result recovery (`RecoverSubmission`) without restarting a runtime.
