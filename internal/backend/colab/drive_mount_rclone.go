package colab

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Drive mount kinds. The kind selects which bootstrap a runtime runs.
const (
	// DriveMountKindDriveFS uses the runtime's own DriveFS binary against a
	// local metadata shim. Verified limitation: its sync engine rejects a
	// third-party OAuth client, so it does not mount.
	DriveMountKindDriveFS = "drivefs"
	// DriveMountKindRclone mounts Drive through rclone, which talks to the
	// public Drive API and therefore accepts the Drive-scoped credential.
	DriveMountKindRclone = "rclone"
)

const (
	// DefaultRcloneArchiveURL is fetched when the runtime image does not ship
	// rclone. Colab runtimes have network access but no rclone binary.
	DefaultRcloneArchiveURL = "https://downloads.rclone.org/rclone-current-linux-amd64.zip"
	// defaultRcloneInstallTimeoutSeconds bounds the rclone download and the
	// FUSE helper installation so a blocked package manager cannot hang a run
	// for the whole task timeout.
	defaultRcloneInstallTimeoutSeconds = 240
)

// rcloneMountMissingSentinel is written into the runtime directory when a
// required mount did not succeed. The finalizer turns it into a failed task:
// a run whose workspace is supposed to be on Drive must not report success
// while its writes landed on the ephemeral VM disk.
const rcloneMountMissingSentinel = "drive-mount.missing"

// RcloneMountOptions carries the runtime bootstrap knobs for the rclone mount.
type RcloneMountOptions struct {
	// ArchiveURL is downloaded when rclone is absent from the image.
	ArchiveURL string
	// InstallTimeoutSecs bounds the download and the FUSE helper install.
	InstallTimeoutSecs int
	// AutoInstall downloads rclone when the image does not ship it.
	AutoInstall bool
	// ExtraArgs are appended to the `rclone mount` command line.
	ExtraArgs []string
}

// RcloneMountOptionsFromEnv reads the rclone bootstrap knobs, falling back to
// the defaults.
func RcloneMountOptionsFromEnv() RcloneMountOptions {
	options := RcloneMountOptions{
		ArchiveURL:         strings.TrimSpace(os.Getenv("CRAFTMAKE_COLAB_RCLONE_URL")),
		InstallTimeoutSecs: defaultRcloneInstallTimeoutSeconds,
		AutoInstall:        true,
	}
	if options.ArchiveURL == "" {
		options.ArchiveURL = DefaultRcloneArchiveURL
	}
	if raw := strings.TrimSpace(os.Getenv("CRAFTMAKE_COLAB_RCLONE_INSTALL_TIMEOUT_SECONDS")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			options.InstallTimeoutSecs = value
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CRAFTMAKE_COLAB_RCLONE_NO_INSTALL"))) {
	case "1", "true", "yes", "on":
		options.AutoInstall = false
	}
	if raw := strings.TrimSpace(os.Getenv("CRAFTMAKE_COLAB_RCLONE_ARGS")); raw != "" {
		options.ExtraArgs = strings.Fields(raw)
	}
	return options
}

// BuildRcloneMountSource renders the bootstrap code that makes the Drive mount
// point usable through rclone.
//
// DriveFS is not an option (its sync engine requires a token from Google's own
// client), but rclone is: it mounts Drive through the public Drive API v3, which
// accepts the same Drive-scoped credential the REST transport uses. The runtime
// downloads rclone when it is absent, provides the fusermount3 helper rclone
// needs, writes a 0600 configuration holding the embedded refresh token, and
// mounts `My Drive` at <mount>/MyDrive so the Colab path convention
// (/content/drive/MyDrive/...) keeps working.
//
// Failures are reported with the CRAFTMAKE_DRIVE_MOUNT_UNAVAILABLE marker, which
// the backend records as an observability error. When the mount was explicitly
// requested (spec.Required) the bootstrap also writes a sentinel file that the
// finalizer converts into a failed task, because continuing would silently write
// the workspace to the ephemeral VM disk.
func BuildRcloneMountSource(spec DriveMountSpec, runtimeDirectory string) string {
	mountRoot := spec.MountPath
	if mountRoot == "" {
		mountRoot = "/content/drive"
	}
	timeout := spec.TimeoutSecs
	if timeout <= 0 {
		timeout = defaultDriveMountTimeoutSeconds
	}
	options := spec.Rclone
	archiveURL := options.ArchiveURL
	if archiveURL == "" {
		archiveURL = DefaultRcloneArchiveURL
	}
	installTimeout := options.InstallTimeoutSecs
	if installTimeout <= 0 {
		installTimeout = defaultRcloneInstallTimeoutSeconds
	}
	sentinel := strings.TrimRight(runtimeDirectory, "/") + "/" + rcloneMountMissingSentinel
	if strings.TrimSpace(runtimeDirectory) == "" {
		sentinel = rcloneMountMissingSentinel
	}
	return fmt.Sprintf(`import json as _json, os as _os, shutil as _shutil, subprocess as _sp, time as _t, urllib.parse as _up, urllib.request as _ur, zipfile as _zip

_MOUNT_ROOT = %s
_MY_DRIVE_DIR = _MOUNT_ROOT + "/MyDrive"
_ALIAS_DIR = _MOUNT_ROOT + "/My Drive"
_CONFIG_DIR = "/root/.config/rclone"
_CONFIG_PATH = _CONFIG_DIR + "/rclone.conf"
_LOG_PATH = "/tmp/rclone-mount.log"
_REMOTE = "craftmake"
_CLIENT_ID = %s
_CLIENT_SECRET = %s
_REFRESH_TOKEN = %s
_TIMEOUT_S = %d
_INSTALL_TIMEOUT_S = %d
_ARCHIVE_URL = %s
_AUTO_INSTALL = %s
_REQUIRED = %s
_EXTRA_ARGS = %s
_SENTINEL_PATH = %s
_OK_MARKER = %s
_UNAVAILABLE_MARKER = %s
_USER_AGENT = "craftmake-colab-rclone-bootstrap/1.0"
_DOWNLOAD_TIMEOUT_S = %d
_RC_ADDR = "127.0.0.1:5572"


def _pad(_value, _width):
    # Manual zero padding: keeps the generated template free of printf verbs.
    _text = str(_value)
    return "0" * (_width - len(_text)) + _text


def _rfc3339(_epoch):
    _parts = _t.gmtime(_epoch)
    return (_pad(_parts.tm_year, 4) + "-" + _pad(_parts.tm_mon, 2) + "-" + _pad(_parts.tm_mday, 2)
            + "T" + _pad(_parts.tm_hour, 2) + ":" + _pad(_parts.tm_min, 2) + ":" + _pad(_parts.tm_sec, 2) + "Z")


def _redact(_text):
    import re as _re
    _text = _re.sub(r"ya29\.[A-Za-z0-9_\-\.]+", "<redacted>", _text)
    _text = _re.sub(r"1//[A-Za-z0-9_\-]+", "<redacted>", _text)
    for _secret in (_CLIENT_SECRET, _REFRESH_TOKEN):
        if _secret:
            _text = _text.replace(_secret, "<redacted>")
    return _text


def _fail(_reason):
    _reason = _redact(str(_reason))
    if _REQUIRED:
        try:
            _os.makedirs(_os.path.dirname(_SENTINEL_PATH) or ".", exist_ok=True)
            with open(_SENTINEL_PATH, "w") as _handle:
                _handle.write(_reason)
        except Exception:
            pass
    print(_UNAVAILABLE_MARKER + " " + _reason, flush=True)


def _token_payload():
    _form = _up.urlencode({
        "grant_type": "refresh_token",
        "refresh_token": _REFRESH_TOKEN,
        "client_id": _CLIENT_ID,
        "client_secret": _CLIENT_SECRET,
    }).encode()
    with _ur.urlopen(_ur.Request("https://oauth2.googleapis.com/token", data=_form), timeout=30) as _resp:
        return _json.loads(_resp.read().decode())


def _download(_url, _target):
    # A browser-ish User-Agent matters: the rclone release host answers 403 to
    # Python's default urllib agent, which is how a live run failed to install
    # rclone. curl is a second, independent path.
    try:
        _request = _ur.Request(_url, headers={"User-Agent": _USER_AGENT})
        with _ur.urlopen(_request, timeout=_DOWNLOAD_TIMEOUT_S) as _response, open(_target, "wb") as _file:
            _shutil.copyfileobj(_response, _file)
        return True
    except Exception:
        pass
    _curl = _shutil.which("curl")
    if _curl:
        _result = _sp.run([_curl, "-fsSL", "--max-time", str(_DOWNLOAD_TIMEOUT_S), "-o", _target, _url], capture_output=True)
        if _result.returncode == 0:
            return True
    return False


def _ensure_rclone():
    _binary = _shutil.which("rclone")
    if _binary:
        return _binary
    if not _AUTO_INSTALL:
        _fail("rclone is not installed and automatic installation is disabled")
        return None
    try:
        _archive_path = "/tmp/rclone.zip"
        if not _download(_ARCHIVE_URL, _archive_path):
            _fail("could not download rclone from " + _ARCHIVE_URL)
            return None
        _installed = None
        with _zip.ZipFile(_archive_path) as _archive:
            for _name in _archive.namelist():
                if _name.endswith("/rclone"):
                    _installed = "/usr/local/bin/rclone"
                    with _archive.open(_name) as _source, open(_installed, "wb") as _destination:
                        _shutil.copyfileobj(_source, _destination)
                    break
        if not _installed:
            _fail("the rclone archive at " + _ARCHIVE_URL + " did not contain the rclone binary")
            return None
        _os.chmod(_installed, 0o755)
    except Exception as _exc:
        _fail("could not install rclone: " + repr(_exc))
        return None
    return _shutil.which("rclone") or _installed


def _ensure_fuse_helper():
    if _shutil.which("fusermount3"):
        return True
    # rclone mounts through fusermount3, but the Colab image only ships the FUSE2
    # helper. Exposing that helper under the expected name works (verified live:
    # the mount came up in about a second), and it is instant, so it is tried
    # before the package manager. Installing fuse3 stays as the fallback and is
    # bounded, so a stalled apt cannot hang the run.
    _fallback = _shutil.which("fusermount")
    if _fallback:
        try:
            _os.symlink(_fallback, "/usr/local/bin/fusermount3")
            if _shutil.which("fusermount3"):
                return True
        except FileExistsError:
            if _shutil.which("fusermount3"):
                return True
        except Exception:
            pass
    try:
        _sp.run(["apt-get", "install", "-y", "-qq", "fuse3"], capture_output=True,
                timeout=_INSTALL_TIMEOUT_S,
                env=dict(_os.environ, DEBIAN_FRONTEND="noninteractive"))
    except Exception:
        pass
    if _shutil.which("fusermount3"):
        return True
    _fallback = _shutil.which("fusermount")
    if not _fallback:
        return False
    try:
        _os.symlink(_fallback, "/usr/local/bin/fusermount3")
    except FileExistsError:
        pass
    except Exception:
        return False
    return True


def _write_config(_token):
    _os.makedirs(_CONFIG_DIR, exist_ok=True)
    _expiry = _rfc3339(_t.time() + int(_token.get("expires_in", 3600)) - 600)
    _payload = {
        "access_token": _token["access_token"],
        "token_type": "Bearer",
        "refresh_token": _REFRESH_TOKEN,
        "expiry": _expiry,
    }
    _lines = [
        "[" + _REMOTE + "]",
        "type = drive",
        "scope = drive",
        "client_id = " + _CLIENT_ID,
        "client_secret = " + _CLIENT_SECRET,
        "token = " + _json.dumps(_payload),
    ]
    # The token is credential material: create the file 0600 from the start.
    _handle = _os.open(_CONFIG_PATH, _os.O_WRONLY | _os.O_CREAT | _os.O_TRUNC, 0o600)
    with _os.fdopen(_handle, "w") as _file:
        _file.write("\n".join(_lines) + "\n")


def _mount_drive():
    if _os.path.ismount(_MY_DRIVE_DIR):
        print(_OK_MARKER + " already mounted at " + _MY_DRIVE_DIR, flush=True)
        return
    _binary = _ensure_rclone()
    if not _binary:
        return
    if not _ensure_fuse_helper():
        _fail("no FUSE mount helper (fusermount3 or fusermount) is available")
        return
    try:
        _token = _token_payload()
        _write_config(_token)
    except Exception as _exc:
        _fail("could not prepare the rclone credential: " + repr(_exc))
        return
    try:
        _os.makedirs(_MOUNT_ROOT, exist_ok=True)
        _os.makedirs(_MY_DRIVE_DIR, exist_ok=True)
        _sp.run(["umount", "-f", _MY_DRIVE_DIR], capture_output=True)
        _command = [
            _binary, "mount", _REMOTE + ":", _MY_DRIVE_DIR,
            "--config", _CONFIG_PATH,
            "--vfs-cache-mode", "writes",
            "--vfs-write-back", "1s",
            "--log-file", _LOG_PATH,
            "--log-level", "INFO",
            # The RC endpoint lets the finalizer wait for pending uploads before
            # the mount is released; without it a write followed by an immediate
            # unmount is lost (verified live: the file was queued, never
            # uploaded).
            "--rc",
            "--rc-addr", _RC_ADDR,
            "--rc-no-auth",
        ] + _EXTRA_ARGS
        # --daemon is not compatible with --rc in rclone 1.75: the daemon parent
        # exits rc=1 ("Daemon timed out ... daemon exited with error code 1"),
        # verified live. Start the mount as a detached background child instead:
        # the kernel process outlives the bootstrap cell, so the mount stays up
        # for the whole run and the RC endpoint stays reachable. Its output goes
        # to a file, because a pipe would be inherited and never reach EOF.
        _launch_log_path = _LOG_PATH + ".launch"
        _launch_log = open(_launch_log_path, "wb")
        try:
            _process = _sp.Popen(_command, stdout=_launch_log, stderr=_sp.STDOUT, start_new_session=True)
        except Exception as _exc:
            _fail("could not start rclone mount: " + repr(_exc))
            return
        _deadline = _t.time() + _TIMEOUT_S
        while _t.time() < _deadline:
            _t.sleep(1)
            if _os.path.ismount(_MY_DRIVE_DIR):
                break
            if _process.poll() is not None:
                break
        if not _os.path.ismount(_MY_DRIVE_DIR):
            _detail = ""
            for _candidate in (_launch_log_path, _LOG_PATH):
                try:
                    with open(_candidate) as _reader:
                        _lines = [line for line in _reader.read().splitlines() if line.strip()]
                    if _lines:
                        _detail = " | ".join(_lines[-3:])
                        break
                except Exception:
                    continue
            _fail("rclone did not mount " + _MY_DRIVE_DIR + " within " + str(_TIMEOUT_S) + "s (rc="
                  + str(_process.poll()) + ")" + (": " + _detail if _detail else ""))
            return
    except Exception as _exc:
        _fail(repr(_exc))
        return
    # DriveFS exposes "My Drive" under the mount point; rclone mounts the My
    # Drive root itself, so recreate both spellings Colab notebooks expect.
    try:
        if not _os.path.exists(_ALIAS_DIR):
            _os.symlink(_MY_DRIVE_DIR, _ALIAS_DIR)
    except Exception:
        pass
    print(_OK_MARKER + " rclone mounted My Drive at " + _MY_DRIVE_DIR, flush=True)


_mount_drive()
`, pythonString(mountRoot), pythonString(spec.Credential.ClientID), pythonString(spec.Credential.ClientSecret),
		pythonString(spec.Credential.RefreshToken), timeout, installTimeout, pythonString(archiveURL),
		pythonBool(options.AutoInstall), pythonBool(spec.Required),
		pythonList(options.ExtraArgs), pythonString(sentinel), pythonString(driveMountOKMarker),
		pythonString(driveMountUnavailableMarker), installTimeout)
}

// pythonBool renders a Go bool as a Python literal.
func pythonBool(value bool) string {
	if value {
		return "True"
	}
	return "False"
}

// pythonList renders a Go string slice as a Python list literal.
func pythonList(values []string) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, pythonString(value))
	}
	return "[" + strings.Join(items, ", ") + "]"
}
