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
