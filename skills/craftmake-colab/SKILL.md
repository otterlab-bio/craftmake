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

**The preflight is advisory by default.** The backend probes the consent before
the first task and, when it is missing, prints a notice and lets the run continue
to the runtime-side consent prompt — Colab binds the grant to the requesting
runtime, so a pre-authorization cannot cover a later ephemeral run.
`CRAFTMAKE_COLAB_DRIVE_PREFLIGHT=strict` aborts instead (unattended pipelines),
and `=off` skips the probe entirely.

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

### 4.4 Persisting results

Use `sync_in`/`sync_out` (verified against a live runtime): the remote workspace
root does not need to be on Drive, and outputs return to
`<project>/.craftmake/colab-workspace/`.

```yaml
colab:
  session: gpu
  drive_root: /content/craftmake
  sync_in: true
  sync_out: true
```

A step must never assume `/content/drive` is mounted — writing there without a
real mount silently lands on the ephemeral VM disk while the step still exits 0.
Fail loudly instead:

```python
import os, sys
from pathlib import Path

if not os.path.ismount('/content/drive'):
    print('ERROR: /content/drive is not mounted; writes would be lost', file=sys.stderr)
    sys.exit(2)
```

### 4.5 Mounting Drive with DriveFS

```bash
craftmake colab drive login --session gpu    # Drive-scoped credential (once)
craftmake colab drive logout --session gpu   # forget it
```

With a Drive credential the bootstrap mounts Drive in the runtime through a local
metadata shim plus `/opt/google/drive/drive`, which is what makes
`/content/drive` real without the Colab frontend. **This is experimental**: the
binary starts but exits `rc=12` on the live service, so `/content/drive` is not
mounted yet — prefer `sync_out` as the reliable path.
`CRAFTMAKE_COLAB_DRIVE_MOUNT=auto|drivefs|off` (default `auto`) controls it; the
default client is rclone's public one and can be overridden. If the runtime has
no DriveFS or the mount times out, the run continues with a notice recorded in
the result's `observability_errors` and `sync_out` remains the fallback.

### 4.6 Limitations verified against the live service

- **`/content/drive` is not a mount unless DriveFS mounting succeeded.** Without
  it, writes under that path land on the ephemeral VM disk while the step still
  exits 0; use the DriveFS mount above, `sync_out`, or the Drive REST API.
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
