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
# Interactive loopback OAuth login using Google's built-in Colab client
craftmake colab auth login --session gpu
```
- Starts an ephemeral loopback HTTP server on `127.0.0.1`.
- Automatically opens your default browser for authorization.
- Saves the refresh token to `~/.config/craftmake/credentials/<session>.json` (`0600`).
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

Craftmake supports using Google Drive as a durable remote workspace (`/content/drive/MyDrive/<root>`).

### 4.1 One-Time Drive Authorization

Google Colab requires explicit user consent before granting a runtime access to Google Drive files:

```bash
# Launch interactive Drive mount authorization
craftmake colab drive mount --session gpu --authorize
```
1. The CLI prints the official Google authorization URL and opens the browser.
2. Sign in with your Google account and click **Allow**.
3. Return to the terminal and press **Enter**.
4. Drive authorization is confirmed and permanently bound.

### 4.2 Auto-Mount in Action Workflows

When an action accesses `/content/drive` or has `drive_root` configured, the executor automatically mounts Drive during bootstrap.

Always flush writes to cloud storage before the runtime is released:

```yaml
schema_version: craftmake.action/v1
name: drive_task
backend: colab
jobs:
  save_model:
    steps:
      - run: |
          python3 - << 'EOF'
          import os
          from pathlib import Path
          from google.colab import drive

          # 1. Verify Drive mount
          if not os.path.ismount('/content/drive'):
              drive.mount('/content/drive', force_remount=False)

          # 2. Save your outputs directly to Google Drive
          output_dir = Path('/content/drive/MyDrive/my_project/checkpoints')
          output_dir.mkdir(parents=True, exist_ok=True)
          (output_dir / 'model.pt').write_text('model weights')

          # 3. CRITICAL: Flush FUSE cache to Google Drive cloud
          drive.flush_and_unmount()
          print('Model successfully flushed to Google Drive!')
          EOF
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

# Execute action on Google Colab
craftmake action run <name> --backend colab --colab-session gpu [--arg KEY=VALUE] [--force]
```

### Run Inspection & Recovery

```bash
# View controller event logs
cat .craftmake/state/runs/<run_id>/controller.jsonl

# View task results and step stdout/stderr
cat .craftmake/state/runs/<run_id>/tasks/<task_id>/attempt-001/result.json
cat .craftmake/state/runs/<run_id>/tasks/<task_id>/attempt-001/step-0.stdout

# Resume interrupted run
craftmake resume --backend colab --colab-session gpu --run <run_id>
```

---

## 6. Troubleshooting & Operational Rules

1. **HTTP 412 (TooManyAssignmentsError)**:
   - Google Colab limits accounts to 1 concurrent runtime on free tiers.
   - Craftmake automatically detects 412 errors, queries existing dangling assignments via `GET /v1/assignments`, and unassigns them automatically before retrying.
2. **Missing Files on Google Drive**:
   - Always call `drive.flush_and_unmount()` at the end of scripts writing to `/content/drive`.
   - Because Craftmake immediately unassigns the virtual machine upon task completion, uncommitted FUSE write buffers will be lost if not explicitly flushed.
3. **Automated Test Isolation**:
   - Inside test suites (`go test`), the browser launcher is automatically silenced (`isRunningInTest()`).
   - Use mock servers for control plane endpoints and set `CRAFTMAKE_NO_BROWSER=1` for headless environments.
4. **Local Credential Fallback**:
   - `resolveColabRefreshToken` automatically detects and prioritizes `~/.config/craftmake/credentials/<session>.json`.
