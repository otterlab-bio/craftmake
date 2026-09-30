# Minimal local verification demo for the craftmake Colab backend

This directory emulates the parts of Google Colab that the `colab` backend talks
to, so the backend can be exercised **locally, offline, and without a Google
account**:

| File | Purpose |
|---|---|
| `mockcolab.go` | Dependency-free (stdlib only) emulator of the Colab control plane and one Jupyter runtime |
| `run_demo.sh` | Builds craftmake, starts the emulator, and drives the real CLI through phases 1-4 |
| `../notebookdump/main.go` | Prints the notebook craftmake would execute (debugging aid) |

## Run it

```bash
verify/colab-minimal/run_demo.sh           # 28 checks, ~10s
verify/colab-minimal/run_demo.sh --strict  # also fails on known bugs
```

Artifacts (mock call log, captured CLI output, results) are printed at the end;
override the location with `CRAFTMAKE_DEMO_DIR=/some/dir`.

## What the emulator implements

* `GET/POST /tun/m/assign`, `GET/POST /tun/m/unassign/<endpoint>` — the
  GET-then-POST XSRF choreography used by `ColabServerClient`.
* `GET/POST /tun/m/credentials-propagation/<endpoint>?authtype=dfs_ephemeral&dryrun=...`
  — the Drive authorization seam. While no consent is recorded it answers
  `{"success": false, "unauthorized_redirect_uri": ...}`; once consent is
  recorded it answers `{"success": true}`. Consent is persisted to a state file so
  it survives separate CLI invocations (the way Google remembers the grant).
* `POST /token` — OAuth refresh-token exchange.
* `GET /v1/assignments`, `GET /api/kernels`.
* `WS /api/kernels/<id>/channels` — a Jupyter kernel protocol endpoint that
  **really executes the notebook cells** with the local `bash`/`python3`. Because
  the emulator is not a Colab VM, the runtime's `/content` prefix is rewritten to
  a writable directory supplied via `--content-root`, and executed cells receive
  `CRAFTMAKE_COLAB_ROOT` pointing at the directory that stands in for `/` (the
  parent of the emulated `/content`).
* `GET /consent` — simulates the user pressing **Allow**.
* `--consent-delay` controls how long after the URL is shown consent is granted.

## What the phases verify

* **Phase 1 — Drive authorization via the CLI.**
  `craftmake colab drive mount --authorize` on a fresh state must print the
  authorization URL and only complete once the grant is detected; a second
  invocation must report `Google Drive is already authorized` without prompting.

* **Phase 2 — fail-fast preflight and the action-file session.**
  A run without Drive authorization must fail before executing any task, naming
  the authorization URL and the exact command to run. After authorizing, the same
  action — whose `colab.session` comes from the action file, with no
  `--colab-session` flag — must run silently and succeed.

* **Phase 3 — failure propagation.**
  An action whose step exits `7` must fail the run, and the recorded result must
  keep the step's 1-based index and exit code.

* **Phase 4 — workspace sync.**
  With `colab.sync_in`/`colab.sync_out` enabled, the runtime must read a file
  uploaded from the local project, the remote output must be mirrored back into
  `<project>/.craftmake/colab-workspace/`, and a second run must not nest
  `work/work/...`.
