# craftmake

**A native workflow executor for compiling versioned YAML into reproducible task runs.**

Craftmake builds a task DAG, executes it locally or through SLURM, persists state in SQLite, and exposes plan, run, resume, cancel, logs, reports, caching, and artifact-aware recovery. It is the execution layer being integrated into `otter`.

## Proof

A plan against a real `otter.run/v1` snapshot prints the compiled DAG — tasks, resource envelopes, and
dependencies — and executes nothing:

```text
$ craftmake plan --config runs/run-20260916T072148Z-ohyuws/run.yaml --phase step1 --catalog workflows/
Workflow: BeaverBS
Phase: step1
Tasks: 6
Submissions: 6

001  sample    cores=2  memory=4294967296  BeaverBS/step1/fastqc_before/sample=S1
002  sample    cores=2  memory=4294967296  BeaverBS/step1/fastqc_before/sample=S2
003  sample    cores=6  memory=8589934592  BeaverBS/step1/trim_reads/sample=S1
004  sample    cores=2  memory=4294967296  BeaverBS/step1/fastqc_after/sample=S1
     needs: BeaverBS/step1/trim_reads/sample=S1
005  sample    cores=6  memory=8589934592  BeaverBS/step1/trim_reads/sample=S2
006  sample    cores=2  memory=4294967296  BeaverBS/step1/fastqc_after/sample=S2
     needs: BeaverBS/step1/trim_reads/sample=S2
```

Resource envelopes come from the snapshot, not from the command line, and the dependency edges above
are the ones the scheduler will honour.

## What it provides

- Versioned workflow YAML and deterministic catalog routing.
- Lightweight Action workflow mode (`craftmake action`) with strict parameter contracts (`${{ args.* }}`, `${{ env.* }}`, `${{ config.* }}`).
- Google Colab remote execution backend with Jupyter WebSocket kernel protocol, automatic quota recovery (HTTP 412), and zero resource leakage.
- Native Google Drive persistent mounting (`/content/drive/MyDrive/...`) with cloud write flushing.
- Local execution with CPU, memory, and parallelism admission budgets.
- Native SLURM submission with task-level resource envelopes and accounting refresh.
- SQLite run/task/submission state, cache decisions, resume, cancellation, and reports.
- Immutable `otter.run/v1` input/reference boundary and create-only artifact publication.
- Structured run IDs, JSON output, failure classification, and reproducible evidence.

## Where it fits

```text
otter project/config → immutable run.yaml → craftmake → enva/operators → results manifest
```

Craftmake can also run a strict `craftmake.standalone/v1` DAG. It does not create Otter projects or replace domain-specific configuration and sample validation.

## Current boundary

Craftmake is the native executor under integration. Existing Otter production workflows still have an explicit Snakemake compatibility path; Craftmake is not an embedded Snakemake interpreter and is not an automatic fallback.

The accepted Gate 6 evidence is bounded. It covers executor parity and selected recovery/publication behavior for RRBS, RNA-seq, BS-PDX, and RNA-PDX. Fresh seven-input matrix work, representative repeats, production-scale qualification, and WGBS qualification remain deferred and must not be presented as completed.

## Install and build

```bash
git clone https://github.com/otterlab-bio/craftmake.git
cd craftmake
make build
./build/craftmake --help
```

Install to a prefix:

```bash
sudo make install PREFIX="/usr/local"
```

## Controller contract

The Craftmake controller is the supported control plane for every canonical run. It validates the immutable `run.yaml`, compiles the selected catalog phase, schedules tasks, persists SQLite state and `controller.jsonl`, reconciles SLURM accounting, handles resume/cancel, and publishes validated artifacts. Direct hand-written `sbatch` orchestration is not a substitute for controller execution.


```bash
craftmake doctor --backend local
craftmake doctor --backend slurm

craftmake plan \
  --config /analysis/runs/run-20260905T010203Z-abcdef/run.yaml \
  --phase step1 \
  --catalog workflows/

craftmake run \
  --config /analysis/runs/run-20260905T010203Z-abcdef/run.yaml \
  --phase step1 \
  --backend local \
  --workers 4 \
  --max-cores 16 \
  --max-memory 64G
```

Resume and inspect a run:

```bash
craftmake resume --state /analysis/runs/<run-id>/state/state.sqlite --run <run-id>
craftmake report --state /analysis/runs/<run-id>/state/state.sqlite --run <run-id>
craftmake cancel --state /analysis/runs/<run-id>/state/state.sqlite --run <run-id>
```

The exact workflow phase, backend, resource envelope, and reference identity for an `otter.run/v1`
snapshot are resolved before execution. By default, mutable overrides (`--backend`, `--run-id`,
`--partition`, `--account`, `--qos`, `--time`, `--scratch-root`) are allowed. Pass `--gate` to `run`
or `resume` to enforce the immutable layer: backend, run identity, and SLURM resources are then fixed
to the resolved snapshot and cannot be overridden.

`--gate` also decides the run identity that gets persisted, which matters beyond enforcement. Under
`--gate` the identity is phase-scoped, so the phases of one run are distinct rows. Without it the
plain run id is recorded, and executing a second phase collides with the first on the primary key,
after which `resume --run <id>--<phase>` finds no rows. Callers that execute more than one phase of a
single snapshot should pass `--gate`.
## Action workflows and Google Colab execution

Craftmake provides a lightweight **Action workflow mode** and a **Google Colab remote execution backend** with native Google Drive integration.

### Action workflow mode (`craftmake action`)

Action workflows live in flat `.craftmake/*.yaml` files in your project directory using the `craftmake.action/v1` schema.

```yaml
# .craftmake/hello.yaml
schema_version: craftmake.action/v1
name: hello
backend: colab                  # default backend: local or colab
inputs:
  who:
    default: world
jobs:
  greet:
    steps:
      - run: echo "hello ${{ args.who }} from colab"
```

#### Parameter source contracts

Parameter substitutions enforce strict, explicit source namespaces:

- `${{ args.<name> }}`: Explicit CLI arguments passed via `--arg KEY=VALUE` (or `--input`). Validated against action `inputs` declarations.
- `${{ env.<NAME> }}`: Read-only snapshot of environment variables captured at execution start.
- `${{ config.<name> }}`: Values from an associated workflow configuration file.

#### Action CLI commands

```bash
# List available actions discovered under .craftmake/
craftmake action list

# Compile and preview execution plan without running
craftmake action plan hello --arg who=Craftmake

# Execute action locally
craftmake action run hello --backend local --arg who=World

# Execute action on Google Colab with a configured session
craftmake action run hello --backend colab --colab-session gpu --arg who=World
```

---

### Google Colab remote backend

The `colab` backend executes tasks on real Google Cloud Colab CPU and GPU runtimes without external dependencies or heavy toolchains.

#### Architectural highlights

- **Standard Library RFC 6455 WebSocket**: Uses a minimal, pure Go RFC 6455 client supporting transparent TLS (`wss://`), custom proxy token headers (`X-Colab-Runtime-Proxy-Token`), and client identification (`X-Colab-Client-Agent: vscode`).
- **Jupyter Kernel Protocol**: Connects to the Colab runtime proxy WebSocket channels (`/api/kernels/<kernel_id>/channels`), manages `execute_request`, parses `stream`, `display_data`/`execute_result` and `error` envelopes, and coordinates kernel interruption.
- **Dynamic Kernel Discovery**: Queries `GET /api/kernels` or initializes sessions via `POST /api/sessions` / `POST /api/kernels` with automatic fallback.
- **Ephemeral instance lifecycle**: Runtimes are acquired per submission and released immediately after each task finishes, because Google Drive (not the VM) is the durable shared state between jobs. `EndRun` sweeps any residual assignment, so a cancelled or failed run cannot leak a machine.
- **Drive authorization preflight**: Before the first task starts, the backend probes Colab for the Drive consent and reports the authorization URL with the exact command when it is missing. The default is advisory (`notify`) because Colab binds the grant to the requesting runtime; `strict` fails fast instead. See [Drive authorization](#drive-authorization-is-a-one-time-per-account-consent).
- **412 Quota Recovery**: Detects Google Colab free-tier concurrent assignment limits (HTTP 412 `TooManyAssignmentsError`), scans existing dangling assignments, and cleans them up automatically before retrying.
- **Local Observability Materialization**: Streams from Jupyter cells are captured and written to local task logs (`step-<index>.stdout`, `step-<index>.stderr`) and `result.json` in the state directory, keyed by the manifest's 1-based step index. A step that records no exit code is reported as failed rather than silently succeeding.

---

### Session authentication & management

Colab sessions store credentials and Drive mount preferences in `~/.config/craftmake/colab-auth.json` (0600 file permissions).

#### 1. Interactive OAuth Login

```bash
# Login via browser OAuth loopback flow
craftmake colab auth login --session gpu
```

- Binds an ephemeral local port on `127.0.0.1` and uses PKCE.
- **No OAuth client configuration is required.** Craftmake bundles the public credentials of Google's Cloud SDK "installed app" client — the same client the official Colab CLI ships in [`src/colab_cli/oauth_config.json`](https://github.com/googlecolab/google-colab-cli/blob/main/src/colab_cli/oauth_config.json) (project `colab-cli`, Apache-2.0) and that colab-vscode calls `..._CLIENT_NOT_SO_SECRET`. Installed-app client secrets are not confidential by design, which is why Google ships it inside its own tooling. Requests the `https://www.googleapis.com/auth/colaboratory` scope.
- **Use your own client when you distribute craftmake** — the consent screen otherwise names Google's client project, and its quota/availability is outside your control:

  ```bash
  # runtime override
  export CRAFTMAKE_COLAB_CLIENT_ID="..." CRAFTMAKE_COLAB_CLIENT_SECRET="..."
  # or at build time
  make build COLAB_CLIENT_ID="..." COLAB_CLIENT_SECRET="..."
  # or per login
  craftmake colab auth login --session gpu --client-id ... --client-secret ...
  ```

  Precedence is flag > environment > bundled client.
- **Google binds a refresh token to the client that minted it.** If you switch clients (for example after this default was introduced, or when moving to your own client), the stored token can no longer be redeemed and the run fails with `invalid_grant`/`unauthorized_client` plus a hint — run `craftmake colab auth login --session <name>` again to mint a token for the new client. Sessions record the client id they were authorized with (`oauth_client_id` in `colab-auth.json`) so the mismatch is reported up front.
- Automatically stores the refresh token in `~/.config/craftmake/credentials/<session>.json` (`0600`).
- **Login once, run indefinitely**: Subsequent runs transparently refresh access tokens in under 0.5s without browser prompts.

#### 2. Inspect and Check Health

```bash
# View non-secret session configuration
craftmake colab auth show --session gpu

# Check offline session readiness and credential files
craftmake colab doctor --session gpu
```

---

### Google Drive integration

Craftmake uses your personal Google Drive as the durable remote workspace (`/content/drive/MyDrive/<root>`). The runtime VM is disposable; Drive is what survives between jobs.

#### Drive authorization is a one-time, per-account consent

Google Colab requires explicit user consent before a runtime may access Drive files. Consent is granted **once per Google account** (until revoked) and is remembered server-side by Google — craftmake stores only your refresh token, never the consent itself:

```bash
# Ask for Drive authorization for this session
craftmake colab drive mount --session gpu --authorize
```

1. craftmake acquires a short-lived probe runtime and asks Colab to propagate Drive credentials (`dfs_ephemeral`) as a *dry run*.
2. If the account has already authorized Drive, the command reports `Google Drive is already authorized` and exits — no browser, no prompt.
3. Otherwise it prints Google's authorization URL, opens your browser, and waits until it detects the completed grant (polling every 3s, or press **Enter** to re-check immediately).

Every later runtime repeats only step 1: a silent dry-run probe plus a real credential propagation. So "authorize once" refers to the user consent, not to the per-runtime mount.

**The preflight is advisory by default.** Before the first task the backend probes
the consent; when it is missing it prints a notice and the run continues to the
runtime-side consent prompt, because (see the limitations below) Colab binds the
grant to the runtime that requested it and a pre-authorization cannot cover a
later ephemeral run. `CRAFTMAKE_COLAB_DRIVE_PREFLIGHT=strict` fails before any
task is scheduled — useful for unattended pipelines — and `=off` skips the probe
entirely (it is also the only mode that does not spend a probe runtime):

```text
# strict
backend begin run: Drive mount is not authorized for session "demo": Google Drive
authorization is required for session "demo"; open https://colab.research.google.com/...
to grant access; run `craftmake colab drive mount --config <auth.json> --session demo
--authorize` once to authorize

# notify (default)
notice: Google Drive authorization is required for session "demo"; continuing, the
runtime will ask for Drive authorization when it is needed
```

#### Workspace sync (`colab.sync_in` / `colab.sync_out`)

Both directions are **opt-in** and travel through the Jupyter kernel, so they need no Drive API client and no extra service:

```yaml
schema_version: craftmake.action/v1
name: sync_demo
backend: colab
colab:
  session: gpu
  drive_root: /content/drive/MyDrive/craftmake
  sync_in: true
  sync_out: true
  excludes:
    - data
```

- `sync_in` archives the local project (tar.gz) and extracts it into `/content/drive/MyDrive/craftmake/work` once per run.
- `sync_out` archives the remote workspace root and merges it into `<project>/.craftmake/colab-workspace/`.
- The local mirror is always excluded from the upload, so repeated runs never nest `work/work/...`.
- Symlinks are never uploaded, archive paths that escape the workspace are rejected, and craftmake's own remote `runtime/` directory is excluded from the download.
- Since the payload travels as a kernel message, both directions are bounded by `CRAFTMAKE_COLAB_SYNC_MAX_BYTES` (default 32 MiB). Keep large datasets on Drive instead.
- `colab.path_map` maps host prefixes to remote prefixes in both directions (e.g. ship a local dataset directory to a specific Drive folder).

#### Honoring the action file's `colab:` block

`craftmake action run` reads the action's `colab:` block; command-line flags win over it, which in turn wins over the session/auth defaults:

| Field | Meaning |
|---|---|
| `session` | Named session (equivalent to `--colab-session`) |
| `auth_config` | Auth config path (equivalent to `--colab-auth-config`) |
| `drive_root` / `remote_root` / `scratch_root` | Remote directories used for the durable workspace, the workspace root, and scratch space |
| `default_accelerator` | Accelerator for jobs that do not set one (`cpu`, `gpu`, `tpu`) |
| `sync_in` / `sync_out` | Enable the workspace sync described above |
| `drive_mount` | How the runtime makes `/content/drive` real: `off` (default), `rclone` (works with the Drive credential), `auto` / `drivefs` (DriveFS, does not mount) |
| `drive_transport` / `drive_folder` | Mirror the workspace into Drive through the Drive REST API |
| `excludes` | Paths skipped by the sync (relative to the project and to the remote root) |
| `path_map` | Host prefix → remote prefix mapping used by the sync |

With `colab.session` declared, `--colab-session` is optional:

```bash
craftmake action run hello --backend colab --force
```

#### Persistence that works today: `sync_in` / `sync_out`

The reliable way to keep results is the workspace sync, which travels through the
kernel and is verified against a live Colab runtime. For results that must outlive
the runtime, mirror them into Drive with
[`colab.drive_transport: rest`](#persisting-the-workspace-to-google-drive-colabdrive_transport-rest)
or mount Drive with [`colab.drive_mount: rclone`](#mounting-drive):

```yaml
# .craftmake/sync_roundtrip.yaml
schema_version: craftmake.action/v1
name: sync_roundtrip
backend: colab
colab:
  session: gpu
  # The remote workspace root does not have to live on Drive.
  drive_root: /content/craftmake
  sync_in: true
  sync_out: true
  excludes:
    - .craftmake/state
jobs:
  roundtrip:
    steps:
      - run: |
          echo "runtime read: $(cat /content/craftmake/work/input.txt)"
          mkdir -p /content/craftmake/outputs
          echo "written-by-colab-runtime" > /content/craftmake/outputs/out.txt
```

```bash
craftmake action run sync_roundtrip --backend colab --force
# the runtime reads the uploaded project, and outputs come back to
# <project>/.craftmake/colab-workspace/outputs/out.txt
```

#### Persisting the workspace to Google Drive (`colab.drive_transport: rest`)

The workspace sync can mirror itself into the account's Google Drive through the
Drive REST API, which is what makes results durable across ephemeral runtimes:

```yaml
colab:
  session: gpu
  drive_root: /content/craftmake
  sync_in: true
  sync_out: true
  drive_transport: rest      # kernel (default) | rest
  drive_folder: craftmake    # folder under My Drive (derived from drive_root if omitted)
```

```bash
craftmake colab drive login --session gpu   # once; a Drive-scoped credential
craftmake action run sync_roundtrip --backend colab --force
```

- `rest` needs the Drive-scoped credential from `colab drive login`; without it
  the run fails with the exact command to run.
- `sync_out` uploads the runtime's workspace to `<drive_folder>/…`; `sync_in`
  restores that folder into the runtime before the local project is uploaded, so
  local files win where they overlap.
- Uploads replace an existing file of the same name, so repeated runs converge
  instead of piling up duplicates.
- **Unchanged files are not transferred.** Each file's MD5 is compared with the
  `md5Checksum` Drive reports for the file already there, and a match skips the
  transfer: an unchanged workspace costs one listing. A file whose checksum Drive
  does not report (its Google-native document types have no byte content) is
  uploaded rather than assumed identical. The run reports the split, e.g.
  `notice: Drive workspace persisted: 3 uploaded, 41 unchanged (skipped), 1 resumed`.
- **Large files are chunked and resumable.** Anything above 5 MiB is sent as a
  resumable session in 8 MiB chunks (`Content-Range`), so a dropped connection
  does not restart a multi-gigabyte checkpoint. After a failed chunk the client
  asks the server how much it actually committed before sending anything else —
  a chunk whose response was lost may already have been stored, and resending it
  would append the same bytes twice.
- **An interrupted upload resumes on the next run.** The resumable session is
  remembered under `<project>/.craftmake/state/drive-uploads/` (`0600`, and that
  directory is excluded from the workspace sync and from git), keyed by folder,
  name, size and checksum, so a changed file never resumes into an unrelated
  upload. Override the location with `CRAFTMAKE_COLAB_DRIVE_RESUME_DIR`, or leave
  it empty/unwritable to fall back to resuming only within a single run. An
  expired session is detected and replaced, not retried forever.
- Both directions stream: the file is read from disk (and written back to disk)
  rather than held in memory, so a large artifact does not have to fit in RAM.
- **Use your own OAuth client for real workloads.** The bundled default is
  rclone's public client, whose per-project quota is shared with every other
  rclone user; it can reject requests with
  `403 ... reason rateLimitExceeded` (`Quota exceeded ... project_number:202264815644`).
  Transient rejections are retried with backoff and reported honestly, but your
  own client removes the limit:
  `CRAFTMAKE_COLAB_DRIVE_CLIENT_ID` / `CRAFTMAKE_COLAB_DRIVE_CLIENT_SECRET`.

#### Writing to Drive from a step

A step must not assume `/content/drive` exists: writing there without a real
mount silently lands on the ephemeral VM disk while the step still exits 0. The
checked-in [`drive_hello.yaml`](.craftmake/drive_hello.yaml) therefore fails
loudly when the mount is missing:

```python
import os, sys
from pathlib import Path

if not os.path.ismount('/content/drive'):
    print('ERROR: /content/drive is not mounted, so anything written here would be lost', file=sys.stderr)
    sys.exit(2)

drive_dir = Path('/content/drive/MyDrive/craftmake')
drive_dir.mkdir(parents=True, exist_ok=True)
(drive_dir / 'helloworld.txt').write_text('helloworld from craftmake colab!')

try:
    from google.colab import drive
    drive.flush_and_unmount()
except Exception as exc:
    print('Notice: flush skipped:', exc)
```

**Optional Drive file service.** Setting `CRAFTMAKE_COLAB_DRIVE_FILES_URL` to a service exposing the `/drive/read` and `/drive/write` endpoints additionally enables Drive-backed log materialization and `RecoverSubmission`, so logs and results can be recovered from the durable workspace without restarting a runtime.

#### Mounting Drive

Mounting is opt-in (`colab.drive_mount` or `CRAFTMAKE_COLAB_DRIVE_MOUNT`) and
there are two mechanisms. **`rclone` is the one that works**; `drivefs` is the
runtime's own binary and does not.

```bash
craftmake colab drive login --session gpu     # Drive-scoped credential (once)
craftmake action run drive_hello --backend colab --force
```

##### `drive_mount: rclone` — a real mount, verified live

```yaml
colab:
  session: gpu
  drive_root: /content/drive/MyDrive/craftmake
  drive_mount: rclone
```

DriveFS rejects a third-party credential, so the bootstrap mounts Drive with
rclone instead, which talks to the public Drive API v3 that accepts the
Drive-scoped credential. The bootstrap:

- installs rclone when the image does not ship it
  (`CRAFTMAKE_COLAB_RCLONE_URL`, disable with `CRAFTMAKE_COLAB_RCLONE_NO_INSTALL`);
- provides the `fusermount3` helper rclone needs — the Colab image only ships the
  FUSE2 `fusermount`, and exposing it under the expected name is enough (verified
  live: the mount is ready in about a second). Installing `fuse3` remains the
  fallback and is bounded by
  `CRAFTMAKE_COLAB_RCLONE_INSTALL_TIMEOUT_SECONDS` (default 240s), so a stalled
  package manager cannot hang the run;
- writes a `0600` rclone configuration holding the refresh token;
- mounts **My Drive at `<mount_path>/MyDrive`** and aliases `<mount_path>/My Drive`
  to it, so both Colab spellings keep working. rclone mounts the My Drive *root*,
  so mounting at `<mount_path>` itself would leave `/content/drive/MyDrive/...`
  pointing at the ephemeral disk;
- starts the mount as a detached background process rather than with `--daemon`,
  which rclone 1.75 rejects when its RC endpoint is enabled (`Daemon timed out …
  daemon exited with error code 1`).

A requested mount is **required**: if it does not come up (`CRAFTMAKE_COLAB_DRIVE_MOUNT_TIMEOUT_SECONDS`,
default 90) the runtime writes a sentinel, the finalizer fails the task with exit
code 2, and the reason is recorded in the result's `observability_errors`. That is
deliberate — a run whose workspace is supposed to be on Drive must not report
success for files that landed on the ephemeral VM disk.

Writing through the mount is buffered, not immediate: rclone queues the upload and
sends it afterwards. The finalizer therefore waits for that queue to drain, using
the RC endpoint the bootstrap enabled, before unmounting — a write followed by an
immediate unmount is lost otherwise (both behaviours were verified live). A queue
that never drains is reported as `CRAFTMAKE_DRIVE_FLUSH_TIMEOUT` in
`observability_errors` instead of being passed off as success.

Costs and caveats: every ephemeral runtime downloads rclone and mounts before the
first step (~10–30s), the mount is a network filesystem, and buffered writes only
reach Drive once they drain. Keep large sequential IO outside the mount and use
`sync_out` or `drive_transport: rest` when durability must not depend on the
mount. Verified live: a step wrote `e2e.txt` through the mount and the content was
read back from the account's Drive afterwards.

##### `drive_mount: drivefs` — does not mount

`CRAFTMAKE_COLAB_DRIVE_MOUNT` also accepts `auto` and `drivefs`, which use the
runtime's own `/opt/google/drive/drive` binary through a local metadata shim:

```bash
craftmake colab drive login --session gpu     # browser approval once
craftmake colab drive logout --session gpu    # forget it
craftmake action run drive_hello --backend colab --force
```

- `CRAFTMAKE_COLAB_DRIVE_MOUNT` selects `off` (default), `rclone`, `auto` (mount
  when a credential is present) or `drivefs` (always attempt). Use `rclone`:
  DriveFS is expected to fail with a third-party client; see the limitation
  below.
- The default Drive client is rclone's public installed-app client; override it
  with `CRAFTMAKE_COLAB_DRIVE_CLIENT_ID`/`_SECRET`, `--client-id`/`--client-secret`,
  or `make build DRIVE_CLIENT_ID=... DRIVE_CLIENT_SECRET=...`.
- The refresh token (not a short-lived access token) is embedded in the
  bootstrap cell so the metadata shim can refresh it in place; the notebook is
  never written to disk.
- **Status: mounting is not achievable without the Colab frontend, and the
  investigation is closed.** A live run with a valid Drive-scoped credential and a
  metadata shim that DriveFS accepted (all three of its requests - the
  `guest-attributes/auth/user-id` identity, the account email and a freshly
  refreshed access token - answered 200) still ended in
  `CANNOT_INIT_CELLOFS: PERMISSION_DENIED: Failed to initialize sync engine` and
  `rc=12` (`CANNOT_START_CORE`). DriveFS's sync engine calls Google-internal Drive
  APIs that reject a token minted by a third-party OAuth client; only the Colab
  frontend can obtain a suitably scoped token. `CRAFTMAKE_COLAB_DRIVE_MOUNT`
  therefore defaults to `off`, and `drivefs` is only for experimentation.
- **What does work: the Drive REST API.** With the same Drive-scoped credential,
  `drive/v3` succeeds against the live account (verified: `about` reports the
  account and quota, a file was created, read back and deleted). Persisting to
  Drive through the REST API is the supported path; `sync_in`/`sync_out` remain
  the transport that needs no Drive at all.
- When the runtime has no DriveFS binary, or the mount does not appear in time
  (`CRAFTMAKE_COLAB_DRIVE_MOUNT_TIMEOUT_SECONDS`, default 90), the run does
  **not** fail: it prints a notice, records it in the result's
  `observability_errors`, and continues, so `sync_out` remains the fallback.
- Because Google binds a refresh token to its client, switching Drive clients
  requires `colab drive login` again; the mismatch is reported with that exact
  command.

#### Limitations verified against the live service

These were observed on real Colab runtimes, not in a simulator:

- **`/content/drive` is not a mount unless you ask for one.** `google.colab.drive.mount()` needs the Colab *frontend*; over a bare kernel WebSocket it raises (`'NoneType' object has no attribute 'kernel'`), `os.path.ismount('/content/drive')` stays false, and the directory simply exists on the ephemeral disk. `drive_mount: rclone` is what makes the path real (verified live); code that writes under `/content/drive/...` without it writes to the VM's local disk, which is destroyed with the VM. The task still exits 0 in that case unless the mount was requested — which is why a requested rclone mount fails the task instead. Use `colab.sync_out`, `colab.drive_transport: rest`, or a mounted Drive; do not rely on `mount_path` alone.
- **Drive consent is per runtime, not per account.** The `dfs_ephemeral` authorization URL carries the runtime endpoint, and a newly assigned runtime reports the credential as unauthorized again, so `colab drive mount --authorize` cannot pre-authorize a later ephemeral run — the consent has to be given while the run that needs it is waiting. With `CRAFTMAKE_COLAB_DRIVE_PREFLIGHT` left at its default the run stops early and prints that URL; the run-level consent prompt (`AuthConsentHandler`) is the path that actually completes.
- **The first connection to a fresh runtime may be too early.** A workspace upload (`sync_in`) issued before the notebook has driven the kernel can time out waiting for the kernel to answer; `sync_out` after the notebook is reliable. `sync_in` defaults to off for this reason.

The exact workflow phase, backend, resource envelope, and reference identity for an `otter.run/v1` snapshot are resolved before execution. By default, mutable overrides (`--backend`, `--run-id`, `--partition`, `--account`, `--qos`, `--time`, `--scratch-root`) are allowed. Pass `--gate` to `run` or `resume` to enforce the immutable layer: backend, run identity, and SLURM resources are then fixed to the resolved snapshot and cannot be overridden.

```bash
# Mutable overrides are allowed by default.
craftmake run \
  --config /analysis/runs/run-20260905T010203Z-abcdef/run.yaml \
  --phase step1 \
  --backend local \
  --partition compute

# --gate enforces the immutable snapshot (backend, run id, SLURM resources).
craftmake run \
  --config /analysis/runs/run-20260905T010203Z-abcdef/run.yaml \
  --phase step1 \
  --gate
```

## ReferenceBuild

Craftmake can download, build, and publish an immutable reference genome release through the `ReferenceBuild` workflow. It reads a `reference-build.yaml` configuration and runs the `acquire_sources → prepare_assets → publish_release` DAG, which calls `otter reference build` to publish the standard registry directory.

```bash
craftmake plan \
  --reference-build-config \
  --config reference-build.yaml \
  --workflow workflows/ReferenceBuild/build.yaml \
  --phase build \
  --catalog workflows/

craftmake run \
  --reference-build-config \
  --config reference-build.yaml \
  --workflow workflows/ReferenceBuild/build.yaml \
  --phase build \
  --catalog workflows/ \
  --gate
```

The reference-build backend and partition are configurable in `reference-build.yaml`:

```yaml
reference_build:
  backend: local        # or slurm; default slurm
  partition: ""        # empty uses the site profile / --partition / CRAFTMAKE_SLURM_PARTITION
  # ... fasta/gtf URLs, checksums, registry_root, tool binaries ...
```

A default `reference-build.yaml` template ships in the release archive under `share/craftmake/configs/`. The `--gate` flag keeps the reference-build run immutable; without it, SLURM resources may be overridden.

## Workflow assets

Current catalog families include:

- `BeaverBS` — RRBS/WGBS-compatible bisulfite phases;
- `BeaverRNA` — RNA-seq phases;
- `BeaverPDX` — bisulfite PDX phases;
- `BeaverRNASEQPDX` — RNA-PDX phases;
- `SRAArchiveDecode` — validated archive decode and paired-FASTQ publication.

See [benchmark evidence](doc/benchmarks/README.md) for the reproducible PDX controller-reconciliation dataset and generator.

## Operational limits

- Admission budgets coordinate scheduling; they do not create OS cgroups for child processes.
- SLURM jobs may remain in `COMPLETING`; inspect Craftmake state and `sacct` together.
- A valid manifest is necessary but is not by itself scientific parity.
- Real production-scale throughput is not established by the current bounded evidence.

## Development

```bash
go test ./...
go vet ./...
make benchmark-pdx-scheduler
```

The repository uses Go 1.26.x as declared in `go.mod`. Each source revision is independent from the parent `otter` checkout; update the parent gitlink only when intentionally integrating a new revision.

## Continuous integration

The repository includes two GitHub Actions workflows:

- `.github/workflows/ci.yaml`: runs on `push`, `pull_request`, and manual dispatch. Validates tests, `go vet`, builds release archives with `make release`, verifies archive checksums, and validates automatic workflow routing. Artifacts are retained for 90 days.
- `.github/workflows/executor-comparison.yml`: runs an executor parity test between Craftmake and Snakemake under the local execution backend, with the executor as the only differing variable. Both engines execute on identical sample fixtures (`sampleA`, `sampleB`) and shared configuration, with Snakemake invoked through `enva run --name otter-snakemake -- snakemake ...`. The workflow asserts byte-for-byte SHA-256 equivalence of all generated output tables (`output/samples/*.tsv` and `output/summary.tsv`), and uploads execution logs and comparison reports for 90 days.

## License

MIT
