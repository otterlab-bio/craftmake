package colab

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Drive mount modes.
const (
	// DriveMountAuto mounts Drive with DriveFS when a Drive credential is
	// configured, and silently falls back when the runtime does not support it.
	DriveMountAuto = "auto"
	// DriveMountDriveFS always attempts the DriveFS mount.
	DriveMountDriveFS = "drivefs"
	// DriveMountRclone mounts Drive with rclone, which accepts the Drive-scoped
	// credential. This is the mode that can actually make /content/drive real.
	DriveMountRclone = "rclone"
	// DriveMountOff never attempts a mount.
	DriveMountOff = "off"
)

// Drive workspace transports.
const (
	// DriveTransportKernel keeps the workspace sync entirely inside the runtime
	// kernel (the default; needs no Drive access at all).
	DriveTransportKernel = "kernel"
	// DriveTransportREST mirrors the workspace into the account's Drive through
	// the Drive REST API.
	DriveTransportREST = "rest"
)

// NormalizeDriveTransport maps operator input onto a known transport.
func NormalizeDriveTransport(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case DriveTransportREST, "drive", "api":
		return DriveTransportREST
	default:
		return DriveTransportKernel
	}
}

// DriveFSBinary is the Drive filesystem binary shipped with Colab runtimes.
const DriveFSBinary = "/opt/google/drive/drive"

// Markers the generated mount code prints back to the executor.
const (
	driveMountUnavailableMarker = "CRAFTMAKE_DRIVE_MOUNT_UNAVAILABLE"
	driveMountOKMarker          = "CRAFTMAKE_DRIVE_MOUNT_OK"
	// driveFlushTimeoutMarker reports that buffered writes had not reached Drive
	// before the mount was released.
	driveFlushTimeoutMarker = "CRAFTMAKE_DRIVE_FLUSH_TIMEOUT"
)

// defaultDriveMountTimeoutSeconds bounds how long the runtime waits for the
// mount to appear.
const defaultDriveMountTimeoutSeconds = 90

// NormalizeDriveMountMode maps operator input onto a known mode.
//
// The default is off: a live investigation showed that DriveFS cannot be mounted
// with a third-party OAuth client (see BuildDriveFSMountSource), so attempting it
// on every run would only cost the mount timeout before falling back. `auto`
// remains available for the case where a credential can satisfy DriveFS, and
// `rclone` selects the mount that does work with it.
func NormalizeDriveMountMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case DriveMountAuto, "if-available":
		return DriveMountAuto
	case DriveMountRclone, "rclone-mount", "fuse":
		return DriveMountRclone
	case DriveMountDriveFS, "on", "true", "1", "drivefs-only":
		return DriveMountDriveFS
	default:
		return DriveMountOff
	}
}

// DriveMountCredential is the Drive-scoped credential a runtime uses to mount
// Drive. The refresh token (not a short-lived access token) is embedded so a
// long-running session can refresh it in place, which is why the mount does not
// expire with the job.
type DriveMountCredential struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
	Email        string
}

// DriveMountSpec describes how the runtime should mount Drive.
type DriveMountSpec struct {
	// Kind selects the bootstrap: DriveMountKindDriveFS (default) or
	// DriveMountKindRclone.
	Kind DriveMountKind
	// Credential is the Drive-scoped credential the mount refreshes from.
	Credential  DriveMountCredential
	MountPath   string
	TimeoutSecs int
	// Required makes a failed mount fail the task. It is set for mounts the
	// operator asked for explicitly, where writing to the ephemeral disk instead
	// of Drive would be a silent data loss.
	Required bool
	// Rclone carries the rclone bootstrap knobs when Kind is rclone.
	Rclone RcloneMountOptions
}

// DriveMountKind names a mount bootstrap.
type DriveMountKind string

// BuildDriveMountSource renders the bootstrap for the spec's mount kind.
//
// runtimeDirectory is where the bootstrap leaves its failure sentinel; see
// BuildRcloneMountSource.
func BuildDriveMountSource(spec DriveMountSpec, runtimeDirectory string) string {
	if spec.Kind == DriveMountKindRclone {
		return BuildRcloneMountSource(spec, runtimeDirectory)
	}
	return BuildDriveFSMountSource(spec)
}

// BuildDriveFSMountSource renders the bootstrap code that mounts Drive inside the
// runtime.
//
// It starts a local metadata server that impersonates the GCE metadata endpoint
// and serves Drive access tokens (refreshing them from the embedded refresh
// token), then starts the runtime's own DriveFS binary against it and waits for
// the mount to appear. This is the mechanism reference implementations use,
// because Colab's own drive.mount() needs the Colab frontend and cannot work
// over a bare kernel WebSocket.
//
// Verified limitation: DriveFS's sync engine (CelloFS) calls Google-internal
// Drive APIs that reject a token minted by a third-party OAuth client. A live run
// with a valid Drive-scoped credential and a correct metadata shim reached
// `CANNOT_INIT_CELLOFS: PERMISSION_DENIED: Failed to initialize sync engine` and
// exited rc=12 (CANNOT_START_CORE), so mounting is not achievable without the
// Colab frontend, which is the only party able to obtain a suitably scoped token.
// The Drive REST API v3 does work with the same credential, and is the supported
// way to persist to Drive.
//
// The code is deliberately non-fatal: when the runtime has no DriveFS binary or
// the mount does not appear, it reports the reason and returns so the caller can
// fall back to the workspace sync instead of failing the task.
func BuildDriveFSMountSource(spec DriveMountSpec) string {
	mountPath := spec.MountPath
	if mountPath == "" {
		mountPath = "/content/drive"
	}
	timeout := spec.TimeoutSecs
	if timeout <= 0 {
		timeout = defaultDriveMountTimeoutSeconds
	}
	return fmt.Sprintf(`import json as _json, os, subprocess as _sp, threading as _th, time as _t, urllib.parse as _up, urllib.request as _ur
from http.server import BaseHTTPRequestHandler as _Handler, HTTPServer as _Server
from pathlib import Path as _Path

_MOUNT_POINT = %s
_DRIVEFS_BINARY = %s
_SCOPES = %s
_CLIENT_ID = %s
_CLIENT_SECRET = %s
_REFRESH_TOKEN = %s
_USER_EMAIL = %s
_TIMEOUT_S = %d
_OK_MARKER = %s
_UNAVAILABLE_MARKER = %s
_token_state = {"access_token": None, "expires_at": 0.0}
_token_lock = _th.Lock()


def _access_token():
    with _token_lock:
        if _token_state["access_token"] is None or _t.time() >= _token_state["expires_at"]:
            _form = _up.urlencode({
                "grant_type": "refresh_token",
                "refresh_token": _REFRESH_TOKEN,
                "client_id": _CLIENT_ID,
                "client_secret": _CLIENT_SECRET,
            }).encode()
            with _ur.urlopen(_ur.Request("https://oauth2.googleapis.com/token", data=_form), timeout=30) as _resp:
                _payload = _json.loads(_resp.read().decode())
            _token_state["access_token"] = _payload["access_token"]
            _token_state["expires_at"] = _t.time() + _payload.get("expires_in", 3600) - 300
        return _token_state["access_token"]


class _MetadataHandler(_Handler):
    def _send(self, body, content_type="application/json"):
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.end_headers()
        self.wfile.write(body if isinstance(body, bytes) else body.encode())

    def do_GET(self):
        try:
            if "/token" in self.path:
                self._send(_json.dumps({
                    "access_token": _access_token(),
                    "expires_in": 3600,
                    "scope": _SCOPES,
                    "token_type": "Bearer",
                }))
            elif "guest-attributes" in self.path:
                # DriveFS asks for
                # /computeMetadata/v1/instance/guest-attributes/auth/user-id
                # to decide which account this runtime belongs to; the runtime's
                # own credential server answers 404 without a Colab frontend, so
                # the shim answers it here.
                self._send(_USER_EMAIL, "text/plain")
            elif "/email" in self.path:
                self._send(_USER_EMAIL, "text/plain")
            elif "/scopes" in self.path:
                self._send(_SCOPES, "text/plain")
            elif "service-accounts" in self.path:
                self._send("default/\n", "text/plain")
            else:
                self._send("ok", "text/plain")
        except Exception as _exc:
            self._send(_json.dumps({"error": str(_exc)}))

    def log_message(self, *args):
        pass


def _mount_drive():
    if os.path.ismount(_MOUNT_POINT) and os.path.isdir(os.path.join(_MOUNT_POINT, "My Drive")):
        print(_OK_MARKER + " already mounted at " + _MOUNT_POINT)
        return
    if not os.path.exists(_DRIVEFS_BINARY):
        print(_UNAVAILABLE_MARKER + " DriveFS binary not present at " + _DRIVEFS_BINARY)
        return
    try:
        _server = _Server(("127.0.0.1", 0), _MetadataHandler)
        _th.Thread(target=_server.serve_forever, daemon=True).start()
        _port = _server.server_address[1]
        _sp.run(["pkill", "-9", "-x", "drive"], capture_output=True)
        _t.sleep(0.3)
        os.makedirs(_MOUNT_POINT, exist_ok=True)
        _sp.run(["umount", "-f", _MOUNT_POINT], capture_output=True)
        _sp.run(["umount", _MOUNT_POINT], capture_output=True)
        _binary_dir = os.path.dirname(_DRIVEFS_BINARY)
        # "drive: [options] [/path/to/mount]" - the mount point is positional as
        # well as carried in the preferences string.
        _log_path = os.path.join(_MOUNT_POINT, "drivefs.log")
        _log = open(_log_path, "wb")
        _process = _sp.Popen(
            " ".join([
                _DRIVEFS_BINARY,
                "--features=crash_throttle_percentage:100,fuse_max_background:1000,max_read_qps:1000,max_write_qps:1000,max_operation_batch_size:15,max_parallel_push_task_instances:10,opendir_timeout_ms:120000,virtual_folders_omit_spaces:true",
                "--metadata_server_auth_uri=http://127.0.0.1:" + str(_port) + "/computeMetadata/v1",
                "--preferences=trusted_root_certs_file_path:" + _binary_dir + "/roots.pem,feature_flag_restart_seconds:129600,mount_point_path:" + _MOUNT_POINT,
                _MOUNT_POINT,
            ]),
            shell=True,
            env={"HOME": "/root", "FUSE_DEV_NAME": "/dev/fuse", "PATH": os.environ.get("PATH", "")},
            stdout=_log,
            stderr=_sp.STDOUT,
        )
        for _ in range(_TIMEOUT_S):
            _t.sleep(1)
            if os.path.isdir(os.path.join(_MOUNT_POINT, "My Drive")):
                print(_OK_MARKER + " mounted at " + _MOUNT_POINT)
                return
            if _process.poll() is not None:
                break
        try:
            _process.terminate()
        except Exception:
            pass
        _detail = ""
        try:
            # The binary's own stdout mostly carries the global feature config
            # complaint. The actionable reason is in the DriveFS log.
            _log.flush()
            with open(_log_path, "rb") as _reader:
                _stdout_text = _reader.read().decode("utf-8", "replace")
            _detail = _stdout_text.strip()[-300:]
            _fs_log = os.path.join(os.path.expanduser("~"), ".config", "Google", "DriveFS", "Logs", "drive_fs.txt")
            with open(_fs_log, "rb") as _reader:
                _fs_text = _reader.read().decode("utf-8", "replace")
            _keys = ("permission_denied", "cannot_init", "handleauthstatus", "failed to initialize", "return code")
            _interesting = [line for line in _fs_text.splitlines() if any(k in line.lower() for k in _keys)]
            if _interesting:
                _detail = (_detail + " | " + " / ".join(_interesting[-4:])).strip()
        except Exception:
            pass
        import re as _re
        _detail = _re.sub(r"ya29\.[A-Za-z0-9_\-\.]+", "<redacted>", _detail)
        _detail = _re.sub(r"1//[A-Za-z0-9_\-]+", "<redacted>", _detail)
        print(_UNAVAILABLE_MARKER + " DriveFS did not mount within " + str(_TIMEOUT_S) + "s (rc=" + str(_process.returncode) + ") " + _detail)
    except Exception as _exc:
        print(_UNAVAILABLE_MARKER + " " + repr(_exc))


_mount_drive()
`, pythonString(mountPath), pythonString(DriveFSBinary), pythonString("email https://www.googleapis.com/auth/drive"),
		pythonString(spec.Credential.ClientID), pythonString(spec.Credential.ClientSecret),
		pythonString(spec.Credential.RefreshToken), pythonString(spec.Credential.Email), timeout,
		pythonString(driveMountOKMarker), pythonString(driveMountUnavailableMarker))
}

// DriveMountTimeoutFromEnv reports the configured mount timeout.
func DriveMountTimeoutFromEnv() int {
	if raw := strings.TrimSpace(os.Getenv("CRAFTMAKE_COLAB_DRIVE_MOUNT_TIMEOUT_SECONDS")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			return value
		}
	}
	return defaultDriveMountTimeoutSeconds
}
