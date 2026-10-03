#!/usr/bin/env bash
# run_demo.sh - minimal local verification demo for the craftmake Google Colab backend.
#
# It emulates the Colab control plane and one Colab Jupyter runtime locally
# (see mockcolab.go) and then drives the *real* craftmake binary:
#
#   Phase 1  `craftmake colab drive mount --authorize`
#            - first invocation must ask for Drive authorization,
#            - second invocation must report "already authorized" with no prompt.
#   Phase 2  `craftmake action run --backend colab`
#            - a run without Drive authorization must fail fast with an
#              actionable message and must not execute any task,
#            - after authorizing, the same action (session taken from the action
#              file, no --colab-session flag) must run silently and succeed.
#   Phase 3  failure propagation: a step that exits non-zero must fail the run.
#   Phase 4  workspace sync: `colab.sync_in` / `colab.sync_out` must move files
#            in both directions and stay idempotent across runs.
#
# No Google account, network access, or OAuth client is required.
#
# Usage:  verify/colab-minimal/run_demo.sh [--strict]
#         CRAFTMAKE_DEMO_DIR=/some/dir verify/colab-minimal/run_demo.sh
#
# Exit status: 0 when every check passes.

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
STRICT=0
if [ "${1:-}" = "--strict" ]; then STRICT=1; fi

DEMO_DIR="${CRAFTMAKE_DEMO_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/craftmake-colab-demo.XXXXXX")}"
# Normalize the path: $TMPDIR may end in a slash, and craftmake reports the
# absolute path with a single separator.
DEMO_DIR="$(cd "$DEMO_DIR" && pwd -P)"
BIN="$DEMO_DIR/bin"
STATE="$DEMO_DIR/state"
CONTENT="$DEMO_DIR/content"
PROJECT="$DEMO_DIR/project"
AUTH_CONFIG="$DEMO_DIR/colab-auth.json"
CRED_FILE="$DEMO_DIR/credentials/demo.json"
MOCK_LOG="$STATE/calls.log"
MOCK_PID=""

mkdir -p "$BIN" "$STATE" "$CONTENT" "$DEMO_DIR/credentials" "$PROJECT/.craftmake"

# The Go build cache must be writable; the sandbox may not allow the default.
export GOCACHE="${GOCACHE:-${TMPDIR:-/tmp}/craftmake-gocache}"
mkdir -p "$GOCACHE"

PASS=0; FAIL=0; KNOWN=0
ok()    { printf '  \033[32mPASS\033[0m      %s\n' "$1"; PASS=$((PASS + 1)); }
bad()   { printf '  \033[31mFAIL\033[0m      %s\n' "$1"; FAIL=$((FAIL + 1)); }
known() { printf '  \033[33mKNOWN-BUG\033[0m %s\n' "$1"; KNOWN=$((KNOWN + 1)); }

assert_contains()     { if grep -q -- "$3" "$2" 2>/dev/null; then ok "$1"; else bad "$1 (missing: $3)"; fi; }
assert_not_contains() { if grep -q -- "$3" "$2" 2>/dev/null; then bad "$1 (unexpected: $3)"; else ok "$1"; fi; }
assert_rc()           { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (exit $2, want $3)"; fi; }
assert_nonzero()      { if [ "$2" -ne 0 ]; then ok "$1"; else bad "$1 (exit 0)"; fi; }
assert_file()         { if [ -f "$2" ]; then ok "$1"; else bad "$1 (missing $2)"; fi; }

cleanup() {
  if [ -n "$MOCK_PID" ]; then kill "$MOCK_PID" 2>/dev/null; fi
}
trap cleanup EXIT

start_mock() {
  # $1 = fresh|keep  (fresh removes the persisted Drive consent state)
  if [ -n "$MOCK_PID" ]; then kill "$MOCK_PID" 2>/dev/null; wait "$MOCK_PID" 2>/dev/null; MOCK_PID=""; fi
  if [ "$1" = "fresh" ]; then rm -f "$STATE/consent.json"; fi
  rm -f "$STATE/mock.url"
  "$BIN/mockcolab" \
    --addr 127.0.0.1:0 \
    --ready-file "$STATE/mock.url" \
    --state "$STATE/consent.json" \
    --log "$MOCK_LOG" \
    --content-root "$CONTENT" \
    --consent-delay 600ms > "$STATE/mock.out" 2>&1 &
  MOCK_PID=$!
  local i
  for i in $(seq 1 100); do
    if [ -s "$STATE/mock.url" ]; then break; fi
    sleep 0.1
  done
  local url
  url="$(cat "$STATE/mock.url")"
  export CRAFTMAKE_COLAB_DOMAIN="$url"
  export CRAFTMAKE_COLAB_GAPI_DOMAIN="$url"
  export CRAFTMAKE_COLAB_TOKEN_URL="$url/token"
  MOCK_URL="$url"
}

log_mark() {
  if [ -f "$MOCK_LOG" ]; then wc -l < "$MOCK_LOG" | tr -d ' '; else echo 0; fi
}
log_since() { tail -n "+$(( $1 + 1 ))" "$MOCK_LOG" 2>/dev/null; }

latest_result_json() {
  find "$PROJECT/.craftmake/state/runs" -name result.json 2>/dev/null | sort | tail -1
}
count_result_json() {
  find "$PROJECT/.craftmake/state/runs" -name result.json 2>/dev/null | wc -l | tr -d ' '
}
# Run ids end in random hex, so results must be looked up per run rather than by
# sorting directory names.
run_id_of() { grep -m1 '^run_id: ' "$1" 2>/dev/null | awk '{print $2}'; }
result_json_of() { find "$PROJECT/.craftmake/state/runs/$1" -name result.json 2>/dev/null | head -1; }
step_stdout_of() { find "$PROJECT/.craftmake/state/runs/$1" -name 'step-1.stdout' 2>/dev/null | head -1; }

# ------------------------------------------------------------------ build

echo "==> building craftmake and the mock Colab runtime"
if ! (cd "$ROOT" && go build -o "$BIN/craftmake" ./cmd/craftmake) > "$STATE/build.log" 2>&1; then
  echo "build failed:"; cat "$STATE/build.log"; exit 1
fi
if ! (cd "$ROOT/verify/colab-minimal" && go build -o "$BIN/mockcolab" .) >> "$STATE/build.log" 2>&1; then
  echo "mock build failed:"; cat "$STATE/build.log"; exit 1
fi
CRAFTMAKE="$BIN/craftmake"

# ------------------------------------------------------------------ session

echo "==> configuring session 'demo' (no OAuth client configured: the bundled public client is used)"
# Deliberately NOT setting CRAFTMAKE_COLAB_CLIENT_ID/SECRET: the CLI must work
# out of the box with its bundled public client.
export CRAFTMAKE_NO_BROWSER=1
export CRAFTMAKE_COLAB_AUTH_TIMEOUT=30s
printf 'demo-refresh-token\n' > "$CRED_FILE"

start_mock fresh
"$CRAFTMAKE" colab auth configure \
  --config "$AUTH_CONFIG" --session demo \
  --drive-root /content/drive/MyDrive/craftmake --mount-path /content/drive \
  --colab-credential-file "$CRED_FILE" --drive-credential-file "$CRED_FILE" > /dev/null

# The actions declare their session and drive root in the file, so the runs
# below never pass --colab-session.
cat > "$PROJECT/.craftmake/hello.yaml" <<'YAML'
schema_version: craftmake.action/v1
name: hello
backend: colab
colab:
  session: demo
  default_accelerator: cpu
  drive_root: /content/drive/MyDrive/craftmake
jobs:
  greet:
    steps:
      - run: |
          echo "hello from emulated colab runtime"
          mkdir -p /content/drive/MyDrive/craftmake
          echo "written to emulated drive" > /content/drive/MyDrive/craftmake/hello.txt
          cat /content/drive/MyDrive/craftmake/hello.txt
YAML

cat > "$PROJECT/.craftmake/failing.yaml" <<'YAML'
schema_version: craftmake.action/v1
name: failing
backend: colab
colab:
  session: demo
  default_accelerator: cpu
  drive_root: /content/drive/MyDrive/craftmake
jobs:
  boom:
    steps:
      - run: |
          echo "about to fail"
          exit 7
YAML

cat > "$PROJECT/.craftmake/sync_demo.yaml" <<'YAML'
schema_version: craftmake.action/v1
name: sync_demo
backend: colab
colab:
  session: demo
  default_accelerator: cpu
  drive_root: /content/drive/MyDrive/craftmake
  sync_in: true
  sync_out: true
jobs:
  roundtrip:
    steps:
      - run: |
          echo "synced input says: $(cat /content/drive/MyDrive/craftmake/work/input.txt)"
          mkdir -p /content/drive/MyDrive/craftmake/outputs
          echo "produced-by-runtime" > /content/drive/MyDrive/craftmake/outputs/out.txt
YAML

cat > "$PROJECT/.craftmake/drive_rest_probe.yaml" <<'YAML'
schema_version: craftmake.action/v1
name: drive_rest_probe
backend: colab
colab:
  session: demo
  default_accelerator: cpu
  drive_root: /content/craftmake
  sync_in: true
  sync_out: true
  drive_transport: rest
  drive_folder: craftmake-e2e
jobs:
  persist:
    steps:
      - run: |
          echo "runtime read: $(cat /content/craftmake/work/input.txt)"
          mkdir -p /content/craftmake/outputs
          echo "rest-transport-output" > /content/craftmake/outputs/out.txt
YAML

cat > "$PROJECT/.craftmake/drive_mount_probe.yaml" <<'YAML'
schema_version: craftmake.action/v1
name: drive_mount_probe
backend: colab
colab:
  session: demo
  default_accelerator: cpu
  drive_root: /content/drive/MyDrive/craftmake
jobs:
  probe:
    steps:
      - run: |
          echo "step ran; mount state is reported by the bootstrap cell"
YAML

printf 'local-input-content\n' > "$PROJECT/input.txt"

# ------------------------------------------------------------------ phase 1

echo
echo "==> phase 1: Drive authorization via the CLI (fresh consent state)"
"$CRAFTMAKE" colab drive mount --config "$AUTH_CONFIG" --session demo --authorize --timeout 30s \
  < /dev/null > "$STATE/drive-mount-1.out" 2>&1
assert_rc "drive mount #1 exits 0" "$?" "0"
assert_contains "drive mount #1 requests authorization" "$STATE/drive-mount-1.out" "authorization required"
assert_contains "drive mount #1 completes after consent" "$STATE/drive-mount-1.out" "successfully authorized"

MARK=$(log_mark)
"$CRAFTMAKE" colab drive mount --config "$AUTH_CONFIG" --session demo --authorize --timeout 30s \
  < /dev/null > "$STATE/drive-mount-2.out" 2>&1
assert_rc "drive mount #2 exits 0" "$?" "0"
assert_contains "drive mount #2 reports already authorized" "$STATE/drive-mount-2.out" "already authorized"
assert_not_contains "drive mount #2 asks for no new authorization" "$STATE/drive-mount-2.out" "authorization required"
log_since "$MARK" > "$STATE/phase1-second.log"
assert_contains "second probe succeeds silently" "$STATE/phase1-second.log" "success=true"

# ------------------------------------------------------------------ phase 2

echo
echo "==> phase 2: Drive preflight modes + action run using the action file session"
start_mock fresh   # forget the Drive consent: this is the "first time" again

# strict: unattended pipelines fail fast instead of blocking on consent.
CRAFTMAKE_COLAB_DRIVE_PREFLIGHT=strict "$CRAFTMAKE" action run hello --backend colab \
  --colab-auth-config "$AUTH_CONFIG" --dir "$PROJECT" --force \
  < /dev/null > "$STATE/action-blocked.out" 2>&1
assert_nonzero "strict mode fails the run without Drive authorization" "$?"
assert_contains "blocked run explains the required authorization" "$STATE/action-blocked.out" "Google Drive authorization is required"
assert_contains "blocked run prints the authorization URL" "$STATE/action-blocked.out" "/consent"
assert_contains "blocked run prints the working command" "$STATE/action-blocked.out" "colab drive mount --config $AUTH_CONFIG --session demo --authorize"
if [ "$(count_result_json)" = "0" ]; then
  ok "blocked run executed no task"
else
  bad "blocked run executed a task before authorization"
fi

"$CRAFTMAKE" colab drive mount --config "$AUTH_CONFIG" --session demo --authorize --timeout 30s \
  < /dev/null > "$STATE/drive-mount-3.out" 2>&1
assert_rc "authorizing after the blocked run succeeds" "$?" "0"

# default (notify): the run proceeds; the runtime-side prompt handles consent.
"$CRAFTMAKE" action run hello --backend colab \
  --colab-auth-config "$AUTH_CONFIG" --dir "$PROJECT" --force \
  < /dev/null > "$STATE/action-1.out" 2>&1
assert_rc "action run exits 0 once authorized" "$?" "0"
assert_not_contains "authorized run needs no authorization" "$STATE/action-1.out" "authorization required"
assert_contains "task succeeded" "$(result_json_of "$(run_id_of "$STATE/action-1.out")")" '"status": "succeeded"'
if [ -f "$CONTENT/drive/MyDrive/craftmake/hello.txt" ]; then
  ok "action wrote to the emulated Drive workspace"
else
  bad "action wrote to the emulated Drive workspace"
fi

# notify is advisory: a missing consent must not stop the run.
start_mock fresh
"$CRAFTMAKE" action run hello --backend colab \
  --colab-auth-config "$AUTH_CONFIG" --dir "$PROJECT" --force \
  < /dev/null > "$STATE/action-notify.out" 2>&1
assert_contains "notify mode warns about missing Drive authorization" "$STATE/action-notify.out" "continuing, the runtime will ask for Drive authorization"
assert_not_contains "notify mode does not stop the run at BeginRun" "$STATE/action-notify.out" "Drive mount is not authorized"

# ------------------------------------------------------------------ phase 3

echo
echo "==> phase 3: failure propagation for a step that exits non-zero"
"$CRAFTMAKE" action run failing --backend colab \
  --colab-auth-config "$AUTH_CONFIG" --dir "$PROJECT" --force \
  < /dev/null > "$STATE/action-failing.out" 2>&1
FAIL_RC=$?
FAIL_RESULT="$(result_json_of "$(run_id_of "$STATE/action-failing.out")")"
assert_nonzero "failing step fails the run" "$FAIL_RC"
assert_contains "failing step is reported as failed" "$FAIL_RESULT" '"status": "failed"'
assert_contains "failing step keeps its exit code" "$FAIL_RESULT" '"exit_code": 7'
assert_contains "failing step keeps its manifest index" "$FAIL_RESULT" '"index": 1'

# ------------------------------------------------------------------ phase 4

echo
echo "==> phase 4: workspace sync_in / sync_out"
"$CRAFTMAKE" action run sync_demo --backend colab \
  --colab-auth-config "$AUTH_CONFIG" --dir "$PROJECT" --force \
  < /dev/null > "$STATE/action-sync-1.out" 2>&1
assert_rc "sync run #1 exits 0" "$?" "0"
SYNC_STDOUT="$(step_stdout_of "$(run_id_of "$STATE/action-sync-1.out")")"
assert_contains "runtime read the synced local file" "$SYNC_STDOUT" "synced input says: local-input-content"
MIRROR="$PROJECT/.craftmake/colab-workspace"
assert_file "remote output came back to the local mirror" "$MIRROR/outputs/out.txt"
assert_contains "mirrored output has the runtime content" "$MIRROR/outputs/out.txt" "produced-by-runtime"

"$CRAFTMAKE" action run sync_demo --backend colab \
  --colab-auth-config "$AUTH_CONFIG" --dir "$PROJECT" --force \
  < /dev/null > "$STATE/action-sync-2.out" 2>&1
assert_rc "sync run #2 exits 0" "$?" "0"
if [ "$(find "$PROJECT" -path '*work/work*' | wc -l | tr -d ' ')" = "0" ]; then
  ok "repeated sync does not nest work/work"
else
  bad "repeated sync nested the workspace mirror"
fi
SYNC_STDOUT_2="$(step_stdout_of "$(run_id_of "$STATE/action-sync-2.out")")"
assert_contains "runtime still reads the synced file on the second run" "$SYNC_STDOUT_2" "synced input says: local-input-content"

# ------------------------------------------------------------------ phase 5

echo
echo "==> phase 5: DriveFS mount is attempted and degrades gracefully"
# A Drive credential makes the bootstrap try to mount Drive with DriveFS. The
# emulator has no /opt/google/drive/drive binary, so the generated code must
# report that and let the run continue instead of failing the task.
CRAFTMAKE_COLAB_DRIVE_MOUNT=drivefs CRAFTMAKE_DRIVE_REFRESH_TOKEN="dummy-drive-refresh-token" "$CRAFTMAKE" action run drive_mount_probe --backend colab \
  --colab-auth-config "$AUTH_CONFIG" --dir "$PROJECT" --force \
  < /dev/null > "$STATE/action-drivefs.out" 2>&1
assert_rc "run with a Drive credential exits 0" "$?" "0"
assert_contains "bootstrap reports that DriveFS is unavailable" "$STATE/action-drivefs.out" "CRAFTMAKE_DRIVE_MOUNT_UNAVAILABLE"
assert_contains "the missing mount is reported to the user" "$STATE/action-drivefs.out" "DriveFS binary not present"
DRIVE_RESULT="$(result_json_of "$(run_id_of "$STATE/action-drivefs.out")")"
assert_contains "the task still succeeds without a mount" "$DRIVE_RESULT" '"status": "succeeded"'
assert_contains "the missing mount is recorded on the result" "$DRIVE_RESULT" "observability_errors"

# ------------------------------------------------------------------ phase 6

echo
echo "==> phase 6: Drive REST transport persists the workspace to Drive"
# A local Drive v3 emulator stands in for the real API, so the transport is
# verified deterministically and without the shared client's quota.
python3 "$(dirname "$0")/fakedrive.py" --port 0 > "$STATE/fakedrive.port" 2>"$STATE/fakedrive.err" &
FAKE_DRIVE_PID=$!
for _ in $(seq 1 50); do
  [ -s "$STATE/fakedrive.port" ] && break
  sleep 0.1
done
FAKE_DRIVE_PORT="$(cat "$STATE/fakedrive.port" 2>/dev/null)"
if [ -n "$FAKE_DRIVE_PORT" ]; then
  ok "local Drive emulator started on port $FAKE_DRIVE_PORT"
else
  bad "local Drive emulator did not start: $(cat "$STATE/fakedrive.err" 2>/dev/null)"
fi

CRAFTMAKE_COLAB_DRIVE_TRANSPORT=rest \
CRAFTMAKE_DRIVE_REFRESH_TOKEN="dummy-drive-refresh-token" \
CRAFTMAKE_DRIVE_API_URL="http://127.0.0.1:$FAKE_DRIVE_PORT/drive/v3" \
CRAFTMAKE_DRIVE_UPLOAD_URL="http://127.0.0.1:$FAKE_DRIVE_PORT/upload/drive/v3" \
  "$CRAFTMAKE" action run drive_rest_probe --backend colab \
  --colab-auth-config "$AUTH_CONFIG" --dir "$PROJECT" --force \
  < /dev/null > "$STATE/action-drive-rest.out" 2>&1
assert_rc "run with the Drive REST transport exits 0" "$?" "0"
assert_contains "the task succeeded" "$(result_json_of "$(run_id_of "$STATE/action-drive-rest.out")")" '"status": "succeeded"'

python3 - "$FAKE_DRIVE_PORT" > "$STATE/fakedrive-state.json" <<'PYEOF'
import json, sys, urllib.request
with urllib.request.urlopen("http://127.0.0.1:%s/__state" % sys.argv[1], timeout=10) as response:
    json.dump(json.load(response), sys.stdout)
PYEOF
if grep -q "craftmake-e2e/outputs/out.txt" "$STATE/fakedrive-state.json"; then
  ok "the runtime's output reached Drive at craftmake-e2e/outputs/out.txt"
else
  bad "the runtime's output did not reach Drive: $(head -c 300 "$STATE/fakedrive-state.json")"
fi
if grep -q "rest-transport-output" "$STATE/fakedrive-state.json"; then
  ok "the uploaded content matches what the runtime wrote"
else
  bad "the uploaded Drive content is wrong"
fi
if grep -q "craftmake-e2e/work/input.txt" "$STATE/fakedrive-state.json"; then
  ok "sync_in uploaded the local project to Drive as well"
else
  bad "the local project was not mirrored to Drive"
fi

# A second run of the same action must not transfer anything: every file is
# already in Drive with the same checksum. This is the incremental path, and it
# is asserted through the real CLI rather than only in unit tests.
read_fake_uploads() {
  python3 - "$FAKE_DRIVE_PORT" > "$1" <<'PYEOF'
import json, sys, urllib.request
with urllib.request.urlopen("http://127.0.0.1:%s/__state" % sys.argv[1], timeout=10) as response:
    json.dump(json.load(response), sys.stdout)
PYEOF
  python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['uploads'])" "$1"
}
UPLOADS_BEFORE="$(read_fake_uploads "$STATE/fakedrive-before.json")"

CRAFTMAKE_COLAB_DRIVE_TRANSPORT=rest \
CRAFTMAKE_DRIVE_REFRESH_TOKEN="dummy-drive-refresh-token" \
CRAFTMAKE_DRIVE_API_URL="http://127.0.0.1:$FAKE_DRIVE_PORT/drive/v3" \
CRAFTMAKE_DRIVE_UPLOAD_URL="http://127.0.0.1:$FAKE_DRIVE_PORT/upload/drive/v3" \
  "$CRAFTMAKE" action run drive_rest_probe --backend colab \
  --colab-auth-config "$AUTH_CONFIG" --dir "$PROJECT" --force \
  < /dev/null > "$STATE/action-drive-rest-again.out" 2>&1
assert_rc "the repeated REST run exits 0" "$?" "0"
assert_contains "the repeated run is still successful" "$(result_json_of "$(run_id_of "$STATE/action-drive-rest-again.out")")" '"status": "succeeded"'
assert_contains "the repeated run reports the unchanged files" "$STATE/action-drive-rest-again.out" "unchanged (skipped)"
UPLOADS_AFTER="$(read_fake_uploads "$STATE/fakedrive-after.json")"
if [ "$UPLOADS_AFTER" = "$UPLOADS_BEFORE" ]; then
  ok "the unchanged workspace was not transferred again (uploads stayed at $UPLOADS_BEFORE)"
else
  bad "the second run re-uploaded content ($UPLOADS_BEFORE -> $UPLOADS_AFTER)"
fi

kill "$FAKE_DRIVE_PID" 2>/dev/null

# ------------------------------------------------------------------ summary

echo
echo "==================== summary ===================="
printf 'pass=%d  fail=%d  known-bug=%d\n' "$PASS" "$FAIL" "$KNOWN"
echo "artifacts: $DEMO_DIR"
echo "mock call log: $MOCK_LOG"
echo "generated notebook: (cd \"$ROOT\" && go run ./verify/notebookdump)"

if [ "$FAIL" -gt 0 ]; then exit 1; fi
if [ "$STRICT" = "1" ] && [ "$KNOWN" -gt 0 ]; then exit 1; fi
exit 0
