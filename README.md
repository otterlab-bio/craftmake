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
- **Drive authorization preflight**: Before the first task starts, the backend probes Colab for the account's Drive consent and fails fast with the authorization URL and the exact command when it is missing, instead of blocking mid-execution. See [Drive authorization](#drive-authorization-is-a-one-time-per-account-consent).
- **412 Quota Recovery**: Detects Google Colab free-tier concurrent assignment limits (HTTP 412 `TooManyAssignmentsError`), scans existing dangling assignments, and cleans them up automatically before retrying.
- **Local Observability Materialization**: Streams from Jupyter cells are captured and written to local task logs (`step-<index>.stdout`, `step-<index>.stderr`) and `result.json` in the state directory, keyed by the manifest's 1-based step index. A step that records no exit code is reported as failed rather than silently succeeding.

---

### Session authentication & management

Colab sessions store credentials and Drive mount preferences in `~/.config/craftmake/colab-auth.json` (0600 file permissions).

#### 1. Interactive OAuth Login

```bash
# Login via browser OAuth loopback flow
export CRAFTMAKE_COLAB_CLIENT_ID="..."
export CRAFTMAKE_COLAB_CLIENT_SECRET="..."
craftmake colab auth login --session gpu
```

- Binds an ephemeral local port on `127.0.0.1` and uses PKCE.
- Uses a runtime-configured Google OAuth client with the `https://www.googleapis.com/auth/colaboratory` scope; craftmake deliberately does not embed an OAuth client, so `CRAFTMAKE_COLAB_CLIENT_ID`/`CRAFTMAKE_COLAB_CLIENT_SECRET` (or `--client-id`/`--client-secret`) are required.
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

**A run without Drive authorization fails fast.** The preflight runs before any task is scheduled:

```text
backend begin run: Drive mount is not authorized for session "demo": Google Drive
authorization is required for session "demo"; open https://colab.research.google.com/...
to grant access; run `craftmake colab drive mount --config <auth.json> --session demo
--authorize` once to authorize
```

Sessions that do not need Drive can skip the probe with `CRAFTMAKE_COLAB_DRIVE_PREFLIGHT=off`.

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
| `excludes` | Paths skipped by the sync (relative to the project and to the remote root) |
| `path_map` | Host prefix → remote prefix mapping used by the sync |

With `colab.session` declared, `--colab-session` is optional:

```bash
craftmake action run hello --backend colab --force
```

#### Drive persistence example

```yaml
# .craftmake/drive_hello.yaml
schema_version: craftmake.action/v1
name: drive_hello
backend: colab
colab:
  session: gpu
  drive_root: /content/drive/MyDrive/craftmake
jobs:
  drive_test:
    steps:
      - run: |
          python3 - << 'EOF'
          import os
          from pathlib import Path

          # The runtime bootstrap mounts Drive when drive_root is configured;
          # only fall back to an explicit mount when it is missing.
          if not os.path.ismount('/content/drive'):
              try:
                  from google.colab import drive
                  drive.mount('/content/drive', force_remount=False)
              except Exception as exc:
                  print('Notice: Drive is not mounted:', exc)

          drive_dir = Path('/content/drive/MyDrive/craftmake')
          drive_dir.mkdir(parents=True, exist_ok=True)

          hello_file = drive_dir / 'helloworld.txt'
          hello_file.write_text('helloworld from craftmake colab!')
          print('Created cloud file:', hello_file)

          # The task finalizer always flushes FUSE writes before the instance is
          # released; flush here too when the step must guarantee durability.
          try:
              from google.colab import drive
              drive.flush_and_unmount()
              print('Flushed to cloud successfully!')
          except Exception as exc:
              print('Notice: flush skipped:', exc)
          EOF
```

Execute the action:

```bash
craftmake action run drive_hello --backend colab --force
```

The file is written directly to your Google Drive and is immediately accessible from the web, mobile app, or subsequent workflow runs.

**Optional Drive file service.** Setting `CRAFTMAKE_COLAB_DRIVE_FILES_URL` to a service exposing the `/drive/read` and `/drive/write` endpoints additionally enables Drive-backed log materialization and `RecoverSubmission`, so logs and results can be recovered from the durable workspace without restarting a runtime.

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
