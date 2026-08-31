#!/usr/bin/env bash
set -Eeuo pipefail

readonly SCRIPT_NAME='verify-r1-native-isolation'
readonly POLICY='--mcp-protocol=2026-07-28'

log() {
  printf '[%s] %s\n' "$SCRIPT_NAME" "$*" >&2
}

die() {
  log "SETUP ERROR: $*"
  exit 2
}

fail() {
  log "FAIL: $*"
  exit 1
}

usage() {
  cat <<'USAGE'
Usage: scripts/verify-r1-native-isolation.sh --output-dir DIR [options]

Options:
  --source-root DIR  Candidate source root. Defaults to the repository containing this script.
  --output-dir DIR   Fresh evidence directory. It must be empty when it already exists.
  --keep-base-dir    Preserve the fresh process/socket BaseDir below TMPDIR after successful evidence generation.
  -h, --help         Show this help.
USAGE
}

require_option_value() {
  local option=$1
  local count=$2
  (( count >= 2 )) || die "missing value for $option"
}

find_python() {
  local candidate

  if [[ -n "${PYTHON3:-}" ]]; then
    if "$PYTHON3" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 0) else 1)' >/dev/null 2>&1; then
      printf '%s\n' "$PYTHON3"
      return 0
    fi
    return 1
  fi

  for candidate in python3 python; do
    if command -v "$candidate" >/dev/null 2>&1 && \
      "$candidate" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 0) else 1)' >/dev/null 2>&1; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done

  return 1
}

source_root_arg=''
output_dir_arg=''
keep_base_dir=0

while (( $# > 0 )); do
  case "$1" in
    --source-root)
      require_option_value "$1" "$#"
      source_root_arg=$2
      shift 2
      ;;
    --source-root=*)
      source_root_arg=${1#*=}
      [[ -n "$source_root_arg" ]] || die 'missing value for --source-root'
      shift
      ;;
    --output-dir)
      require_option_value "$1" "$#"
      output_dir_arg=$2
      shift 2
      ;;
    --output-dir=*)
      output_dir_arg=${1#*=}
      [[ -n "$output_dir_arg" ]] || die 'missing value for --output-dir'
      shift
      ;;
    --keep-base-dir)
      keep_base_dir=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
if [[ -n "$source_root_arg" ]]; then
  source_root="$(cd -- "$source_root_arg" && pwd -P)" || die "source root is not a directory: $source_root_arg"
else
  source_root="$(cd -- "$script_dir/.." && pwd -P)"
fi

[[ -n "$output_dir_arg" ]] || die '--output-dir is required so the runner never adopts an OS temporary directory'
[[ ! -L "$output_dir_arg" ]] || die "output directory must not be a symbolic link: $output_dir_arg"
if [[ -e "$output_dir_arg" ]]; then
  [[ -d "$output_dir_arg" ]] || die "output path is not a directory: $output_dir_arg"
  shopt -s nullglob dotglob
  output_entries=("$output_dir_arg"/*)
  shopt -u nullglob dotglob
  (( ${#output_entries[@]} == 0 )) || die "output directory must be empty: $output_dir_arg"
else
  output_parent="$(cd -- "$(dirname -- "$output_dir_arg")" && pwd -P)" || die "output directory parent does not exist: $output_dir_arg"
  output_leaf=$(basename -- "$output_dir_arg")
  [[ "$output_leaf" != '.' && "$output_leaf" != '..' ]] || die "output directory must name a fresh directory: $output_dir_arg"
  mkdir -- "$output_parent/$output_leaf" || die "failed to create output directory: $output_dir_arg"
fi
output_dir="$(cd -- "$output_dir_arg" && pwd -P)"

for required_input in \
  go.mod \
  cmd/mcp-mux/main.go \
  testdata/mock_modern_server.go \
  testdata/mock_server.go \
  testdata/modern_opening_corpus.ndjson; do
  [[ -f "$source_root/$required_input" ]] || die "source root is missing $required_input: $source_root"
done

if [[ -n "${MCP_MUX_R1_GO:-}" ]]; then
  GO_BIN="$MCP_MUX_R1_GO"
else
  GO_BIN="$(command -v go)" || die 'Go is required to build the candidate fixtures'
fi
[[ -x "$GO_BIN" ]] || die "Go executable is unavailable: $GO_BIN"
command -v git >/dev/null 2>&1 || die 'Git is required to record the candidate source SHA'
PYTHON_BIN="$(find_python)" || die 'Python 3 is required for the Unix customer-proof runner'

source_sha="$(git -C "$source_root" rev-parse HEAD 2>/dev/null)" || die "cannot resolve Git source SHA for $source_root"
[[ "$source_sha" =~ ^[0-9a-fA-F]{7,64}$ ]] || die "Git source SHA is not hexadecimal: $source_sha"
go_version="$(GOTOOLCHAIN=local "$GO_BIN" version)" || die 'cannot determine the local Go version'

[[ -n "${TMPDIR:-}" ]] || die 'TMPDIR is required for the fresh Unix runner scratch directory'
base_parent="${MCP_MUX_R1_BASE_ROOT:-$TMPDIR}"
[[ -d "$base_parent" && ! -L "$base_parent" ]] || die "Unix process base parent must be a real directory: $base_parent"
base_dir="$base_parent/r1-$PPID-$$"
runtime_dir="$base_dir/runtime"
artifacts_dir="$output_dir/artifacts"
mux_bin="$base_dir/bin/mcp-mux"
modern_fixture_bin="$base_dir/bin/mock_modern_server"
legacy_fixture_bin="$base_dir/bin/mock_server"
base_created=0
evidence_durable=0

orig_tmpdir_state=${TMPDIR+x}
orig_tmpdir=${TMPDIR-}
orig_tmp_state=${TMP+x}
orig_tmp=${TMP-}
orig_temp_state=${TEMP+x}
orig_temp=${TEMP-}
orig_home_state=${HOME+x}
orig_home=${HOME-}
orig_xdg_cache_state=${XDG_CACHE_HOME+x}
orig_xdg_cache=${XDG_CACHE_HOME-}
orig_xdg_config_state=${XDG_CONFIG_HOME+x}
orig_xdg_config=${XDG_CONFIG_HOME-}
orig_xdg_data_state=${XDG_DATA_HOME+x}
orig_xdg_data=${XDG_DATA_HOME-}
orig_gocache_state=${GOCACHE+x}
orig_gocache=${GOCACHE-}
orig_gomodcache_state=${GOMODCACHE+x}
orig_gomodcache=${GOMODCACHE-}
orig_gotmpdir_state=${GOTMPDIR+x}
orig_gotmpdir=${GOTMPDIR-}

restore_environment() {
  if [[ -n "$orig_tmpdir_state" ]]; then export TMPDIR="$orig_tmpdir"; else unset TMPDIR; fi
  if [[ -n "$orig_tmp_state" ]]; then export TMP="$orig_tmp"; else unset TMP; fi
  if [[ -n "$orig_temp_state" ]]; then export TEMP="$orig_temp"; else unset TEMP; fi
  if [[ -n "$orig_home_state" ]]; then export HOME="$orig_home"; else unset HOME; fi
  if [[ -n "$orig_xdg_cache_state" ]]; then export XDG_CACHE_HOME="$orig_xdg_cache"; else unset XDG_CACHE_HOME; fi
  if [[ -n "$orig_xdg_config_state" ]]; then export XDG_CONFIG_HOME="$orig_xdg_config"; else unset XDG_CONFIG_HOME; fi
  if [[ -n "$orig_xdg_data_state" ]]; then export XDG_DATA_HOME="$orig_xdg_data"; else unset XDG_DATA_HOME; fi
  if [[ -n "$orig_gocache_state" ]]; then export GOCACHE="$orig_gocache"; else unset GOCACHE; fi
  if [[ -n "$orig_gomodcache_state" ]]; then export GOMODCACHE="$orig_gomodcache"; else unset GOMODCACHE; fi
  if [[ -n "$orig_gotmpdir_state" ]]; then export GOTMPDIR="$orig_gotmpdir"; else unset GOTMPDIR; fi
}

cleanup() {
  local status=$?
  trap - EXIT

  if [[ -x "$mux_bin" && -d "$runtime_dir" ]]; then
    env \
      TMPDIR="$runtime_dir" \
      TMP="$runtime_dir" \
      TEMP="$runtime_dir" \
      HOME="$runtime_dir/home" \
      XDG_CACHE_HOME="$runtime_dir/xdg-cache" \
      XDG_CONFIG_HOME="$runtime_dir/xdg-config" \
      XDG_DATA_HOME="$runtime_dir/xdg-data" \
      MCPMUX_ENGINE=1 \
      MCP_MUX_NO_DAEMON=0 \
      MCP_MUX_ISOLATED=0 \
      MCP_MUX_STATELESS=0 \
      MCP_MUX_DAEMON=0 \
      MCP_MUX_DEFAULT_MODE=global \
      MCP_MUX_SHIM_LOG='' \
      "$mux_bin" stop --force >/dev/null 2>&1 || true
  fi

  if (( status == 0 && evidence_durable == 1 && keep_base_dir == 0 && base_created == 1 )); then
    if ! rm -rf -- "$base_dir"; then
      log "FAIL: unable to remove fresh base directory after durable evidence: $base_dir"
      status=1
    fi
  fi

  restore_environment
  exit "$status"
}

[[ ! -e "$base_dir" ]] || die "fresh output directory already contains base: $base_dir"
mkdir -p \
  "$base_dir/bin" \
  "$runtime_dir/home" \
  "$runtime_dir/xdg-cache" \
  "$runtime_dir/xdg-config" \
  "$runtime_dir/xdg-data" \
  "$base_dir/go-cache" \
  "$base_dir/go-mod-cache" \
  "$base_dir/go-tmp" \
  "$artifacts_dir" || die 'failed to create fresh runner directories'
base_created=1
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

export TMPDIR="$runtime_dir"
export TMP="$runtime_dir"
export TEMP="$runtime_dir"
export HOME="$runtime_dir/home"
export XDG_CACHE_HOME="$runtime_dir/xdg-cache"
export XDG_CONFIG_HOME="$runtime_dir/xdg-config"
[[ -n "${MCP_MUX_R1_GOMODCACHE:-}" && -d "$MCP_MUX_R1_GOMODCACHE" ]] || die 'MCP_MUX_R1_GOMODCACHE must name a populated module cache for the offline build'
export GOMODCACHE="$MCP_MUX_R1_GOMODCACHE"
export GOTOOLCHAIN=local
export GOPROXY=off
export GOCACHE="$base_dir/go-cache"
export GOTMPDIR="$base_dir/go-tmp"

log 'building the exact candidate binary and both in-tree fixtures'
if ! (
  cd -- "$source_root"
  "$GO_BIN" build -o "$mux_bin" ./cmd/mcp-mux
  "$GO_BIN" build -o "$modern_fixture_bin" ./testdata/mock_modern_server.go
  "$GO_BIN" build -o "$legacy_fixture_bin" ./testdata/mock_server.go
); then
  fail 'candidate or fixture build failed'
fi

for built_path in "$mux_bin" "$modern_fixture_bin" "$legacy_fixture_bin"; do
  [[ -f "$built_path" && -s "$built_path" && -x "$built_path" ]] || fail "candidate build did not produce an executable: $built_path"
done

run_harness() {
  "$PYTHON_BIN" - \
    "$source_root" \
    "$output_dir" \
    "$base_dir" \
    "$runtime_dir" \
    "$mux_bin" \
    "$modern_fixture_bin" \
    "$legacy_fixture_bin" \
    "$source_root/testdata/modern_opening_corpus.ndjson" \
    "$source_sha" \
    "$go_version" \
    "$keep_base_dir" \
    "$POLICY" <<'PY'
import base64
import hashlib
import json
import os
import platform
import selectors
import subprocess
import sys
import time
import traceback
from pathlib import Path

(
    SOURCE_ROOT,
    OUTPUT_DIR,
    BASE_DIR,
    RUNTIME_DIR,
    MUX_BIN,
    MODERN_FIXTURE_BIN,
    LEGACY_FIXTURE_BIN,
    CORPUS_PATH,
    SOURCE_SHA,
    GO_VERSION,
    KEEP_BASE_DIR,
    POLICY,
) = sys.argv[1:]

SOURCE_ROOT = Path(SOURCE_ROOT)
OUTPUT_DIR = Path(OUTPUT_DIR)
BASE_DIR = Path(BASE_DIR)
RUNTIME_DIR = Path(RUNTIME_DIR)
MUX_BIN = Path(MUX_BIN)
MODERN_FIXTURE_BIN = Path(MODERN_FIXTURE_BIN)
LEGACY_FIXTURE_BIN = Path(LEGACY_FIXTURE_BIN)
CORPUS_PATH = Path(CORPUS_PATH)
ARTIFACTS_DIR = OUTPUT_DIR / "artifacts"
TRANSCRIPT_PATH = OUTPUT_DIR / "transcript.ndjson"
KEEP_BASE_DIR = KEEP_BASE_DIR == "1"
TIMEOUT_SECONDS = 45
STOP_TIMEOUT_SECONDS = 20

MODERN_FACTS = {
    "protocol_era": "2026-07-28",
    "sharing_policy": "forced-isolated",
    "cache_policy": "off",
    "lifecycle_policy": "r1-quarantine",
}
MODERN_PROHIBITED_KEYS = {
    "sessions",
    "inflight",
    "oldest_request_age_ms",
    "finalization_error",
    "owner_generation",
    "restored_from_owner_generation",
    "restore_source",
    "mux_engines",
    "topology",
    "registry",
    "registry_descriptor",
    "taxonomy",
    "counter",
    "counters",
    "logging",
    "logs",
}


class CheckFailure(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise CheckFailure(message)


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def sha256_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def relative(path):
    return path.relative_to(OUTPUT_DIR).as_posix()

def evidence_or_absolute(path):
    try:
        return relative(path)
    except ValueError:
        return str(path)


def utf8(data):
    return data.decode("utf-8", errors="replace")


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def append_transcript(record):
    with TRANSCRIPT_PATH.open("a", encoding="utf-8") as stream:
        stream.write(json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n")


def product_environment(extra=None):
    env = os.environ.copy()
    env.update(
        {
            "TMPDIR": str(RUNTIME_DIR),
            "TMP": str(RUNTIME_DIR),
            "TEMP": str(RUNTIME_DIR),
            "HOME": str(RUNTIME_DIR / "home"),
            "XDG_CACHE_HOME": str(RUNTIME_DIR / "xdg-cache"),
            "XDG_CONFIG_HOME": str(RUNTIME_DIR / "xdg-config"),
            "XDG_DATA_HOME": str(RUNTIME_DIR / "xdg-data"),
            "MCPMUX_ENGINE": "1",
            "MCP_MUX_NO_DAEMON": "0",
            "MCP_MUX_ISOLATED": "0",
            "MCP_MUX_STATELESS": "0",
            "MCP_MUX_DAEMON": "0",
            "MCP_MUX_DEFAULT_MODE": "global",
            "MCP_MUX_SHIM_LOG": "",
        }
    )
    if extra:
        for key, value in extra.items():
            if value is None:
                env.pop(key, None)
            else:
                env[key] = str(value)
    return env


def command_record(result):
    return {
        "argv": result["argv"],
        "exit_code": result["exit_code"],
        "stdout": utf8(result["stdout"]),
        "stderr": utf8(result["stderr"]),
        "stdout_sha256": sha256_bytes(result["stdout"]),
        "stderr_sha256": sha256_bytes(result["stderr"]),
    }


def execute(label, argv, input_bytes=None, extra_env=None, timeout=TIMEOUT_SECONDS):
    command = [str(item) for item in argv]
    try:
        completed = subprocess.run(
            command,
            cwd=str(RUNTIME_DIR),
            env=product_environment(extra_env),
            input=input_bytes,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
            check=False,
        )
    except subprocess.TimeoutExpired as error:
        stdout = error.stdout or b""
        stderr = error.stderr or b""
        raise CheckFailure(
            f"{label} timed out after {timeout}s; stdout_sha256={sha256_bytes(stdout)} "
            f"stderr_sha256={sha256_bytes(stderr)}"
        ) from error
    except OSError as error:
        raise CheckFailure(f"{label} could not start: {error}") from error
    return {
        "label": label,
        "argv": command,
        "exit_code": completed.returncode,
        "stdout": completed.stdout,
        "stderr": completed.stderr,
    }


def stop_daemon(label, strict=True):
    result = execute(label, [MUX_BIN, "stop", "--force"], timeout=STOP_TIMEOUT_SECONDS)
    record = command_record(result)
    if strict:
        require(result["exit_code"] == 0, f"{label}: built mcp-mux stop --force exited {result['exit_code']}")

    deadline = time.monotonic() + 10
    attempts = 0
    last = None
    while time.monotonic() < deadline:
        attempts += 1
        last = execute(f"{label}-wait-stopped", [MUX_BIN, "status"], timeout=5)
        if last["exit_code"] == 0 and utf8(last["stdout"]).strip() == "No active mcp-mux instances found.":
            record["retirement_wait"] = {
                "attempts": attempts,
                "status": "No active mcp-mux instances found.",
            }
            return record
        time.sleep(0.05)

    message = f"{label}: daemon did not retire after stop; last_status={command_record(last) if last else None}"
    if strict:
        raise CheckFailure(message)
    record["retirement_wait_error"] = message
    return record


def run_status(label):
    result = execute(label, [MUX_BIN, "status"], timeout=TIMEOUT_SECONDS)
    require(result["exit_code"] == 0, f"{label}: built mcp-mux status exited {result['exit_code']}")
    return result


def decode_status(result, label):
    text = utf8(result["stdout"]).strip()
    if text == "No active mcp-mux instances found.":
        return None, []
    try:
        decoded = json.loads(text)
    except json.JSONDecodeError as error:
        raise CheckFailure(f"{label}: status stdout is neither an empty-state message nor JSON: {text!r}") from error
    if isinstance(decoded, dict):
        servers = decoded.get("servers", [])
        require(isinstance(servers, list), f"{label}: daemon status servers is not an array")
        return decoded, servers
    if isinstance(decoded, list):
        return decoded, decoded
    raise CheckFailure(f"{label}: status JSON has unsupported shape {type(decoded).__name__}")


def exact_capture(path, raw_opening, label):
    deadline = time.monotonic() + 3
    while not path.is_file() and time.monotonic() < deadline:
        time.sleep(0.02)
    require(path.is_file(), f"{label}: fixture capture was not created")
    data = path.read_bytes()
    expected = raw_opening + b"\n"
    require(data == expected, f"{label}: upstream capture is not exactly the host opening plus LF framing")
    return {
        "path": relative(path),
        "sha256": sha256_bytes(data),
        "opening_sha256": sha256_bytes(raw_opening),
        "opening_base64": base64.b64encode(raw_opening).decode("ascii"),
    }


def parse_ndjson(data, label):
    frames = []
    for number, raw_line in enumerate(data.splitlines(), start=1):
        if not raw_line.strip():
            continue
        try:
            decoded = json.loads(raw_line.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise CheckFailure(f"{label}: stdout line {number} is not UTF-8 JSON-RPC NDJSON") from error
        require(isinstance(decoded, dict), f"{label}: stdout line {number} is not a JSON object")
        frames.append(decoded)
    return frames


def response_for(frames, request_id, label):
    matches = [
        frame
        for frame in frames
        if frame.get("jsonrpc") == "2.0" and frame.get("id") == request_id and "result" in frame
    ]
    require(len(matches) == 1, f"{label}: expected exactly one native result for request ID {request_id!r}")
    require("error" not in matches[0], f"{label}: native response unexpectedly has an error")
    return matches[0]


def assert_native_result(request, response, label):
    result = response.get("result")
    method = request["method"]
    require(isinstance(result, dict), f"{label}: native {method} result is not an object")
    if method == "tools/list":
        tools = result.get("tools")
        require(isinstance(tools, list), f"{label}: tools/list did not return tools")
        require(any(tool.get("name") == "modern_echo" for tool in tools if isinstance(tool, dict)), f"{label}: tools/list omitted modern_echo")
    elif method == "server/discover":
        require(result.get("resultType") == "complete", f"{label}: server/discover resultType is not complete")
        supported = result.get("supportedVersions")
        require(isinstance(supported, list) and "2026-07-28" in supported, f"{label}: server/discover omitted the modern protocol version")
    elif method == "ping":
        require(result == {}, f"{label}: ping result is not an empty object")
    elif method == "tools/call":
        if result.get("resultType") == "input_required":
            require(isinstance(result.get("inputRequests"), dict), f"{label}: input_required result has no inputRequests")
            require("requestState" in result, f"{label}: input_required result has no opaque requestState")
        else:
            content = result.get("content")
            require(isinstance(content, list) and content, f"{label}: tools/call did not return content")
    else:
        raise CheckFailure(f"{label}: unexpected modern fixture method {method!r}")


def modern_request(request_id, method, *, log_level=None, extra_params=None):
    metadata = {
        "io.modelcontextprotocol/protocolVersion": "2026-07-28",
        "io.modelcontextprotocol/clientCapabilities": {},
    }
    if log_level is not None:
        metadata["io.modelcontextprotocol/logLevel"] = log_level
    params = {"_meta": metadata}
    if extra_params:
        params.update(extra_params)
    return {
        "jsonrpc": "2.0",
        "id": request_id,
        "method": method,
        "params": params,
    }


def encoded_request(value):
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def run_modern_once(label, raw_opening, *, mode="", extra_env=None):
    try:
        opening = json.loads(raw_opening.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise CheckFailure(f"{label}: modern opening is not JSON") from error
    require(isinstance(opening, dict) and "id" in opening, f"{label}: modern opening has no request ID")
    session, capture_path = begin_live_modern(label, raw_opening, mode=mode, extra_env=extra_env)
    try:
        terminal = session.wait_for(
            lambda frame: frame.get("id") == opening["id"] and ("result" in frame or "error" in frame)
        )
        require(terminal.get("jsonrpc") == "2.0", f"{label}: host response is not JSON-RPC 2.0")
        require("result" in terminal and "error" not in terminal, f"{label}: host did not receive a native result before cleanup")
        assert_native_result(opening, terminal, label)
        capture = exact_capture(capture_path, raw_opening, label)
    except Exception:
        if session.process.poll() is None:
            try:
                session.finish(timeout=0.1)
                stop_daemon(f"{label}-failure-stop", strict=False)
            except CheckFailure:
                pass
        raise
    return {"session": session, "opening": opening, "capture": capture, "terminal": terminal}


def isolated_modern_once(label, raw_opening, *, mode="", extra_env=None, status_probe=None):
    before = stop_daemon(f"{label}-before")
    session = None
    after = None
    active_status = None
    try:
        live = run_modern_once(label, raw_opening, mode=mode, extra_env=extra_env)
        session = live["session"]
        if status_probe is not None:
            active_status = status_probe(label)
        result = session.finish(timeout=0.1)
        after = stop_daemon(f"{label}-after")
        return {
            "opening": {
                "sha256": sha256_bytes(raw_opening),
                "base64": base64.b64encode(raw_opening).decode("ascii"),
            },
            "capture": live["capture"],
            "process": command_record(result),
            "frames": parse_ndjson(result["stdout"], label),
            "active_status": active_status,
            "stop_before": before,
            "stop_after": after,
        }
    finally:
        if session is not None and session.process.poll() is None:
            try:
                session.finish(timeout=0.1)
            except CheckFailure:
                pass
        if after is None:
            stop_daemon(f"{label}-after")


class LiveSession:
    def __init__(self, label, argv, extra_env=None):
        self.label = label
        self.argv = [str(item) for item in argv]
        try:
            self.process = subprocess.Popen(
                self.argv,
                cwd=str(RUNTIME_DIR),
                env=product_environment(extra_env),
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                bufsize=0,
            )
        except OSError as error:
            raise CheckFailure(f"{label}: could not start live product session: {error}") from error
        self.selector = selectors.DefaultSelector()
        self.selector.register(self.process.stdout, selectors.EVENT_READ, "stdout")
        self.selector.register(self.process.stderr, selectors.EVENT_READ, "stderr")
        self.stdout_buffer = b""
        self.stdout_lines = []
        self.stderr = bytearray()
        self.stdin_closed = False

    def send(self, raw_bytes):
        require(not self.stdin_closed and self.process.stdin is not None, f"{self.label}: stdin is already closed")
        try:
            self.process.stdin.write(raw_bytes)
            self.process.stdin.flush()
        except OSError as error:
            raise CheckFailure(f"{self.label}: could not write host frame: {error}") from error

    def _consume(self, kind, chunk):
        if kind == "stdout":
            self.stdout_buffer += chunk
            while b"\n" in self.stdout_buffer:
                line, self.stdout_buffer = self.stdout_buffer.split(b"\n", 1)
                self.stdout_lines.append(line)
        else:
            self.stderr.extend(chunk)

    def pump(self, timeout):
        for key, _ in self.selector.select(timeout):
            try:
                chunk = os.read(key.fileobj.fileno(), 65536)
            except OSError as error:
                raise CheckFailure(f"{self.label}: could not read {key.data}: {error}") from error
            if not chunk:
                try:
                    self.selector.unregister(key.fileobj)
                except KeyError:
                    pass
                continue
            self._consume(key.data, chunk)

    def wait_for(self, predicate, timeout=TIMEOUT_SECONDS):
        deadline = time.monotonic() + timeout
        index = 0
        while True:
            while index < len(self.stdout_lines):
                raw_line = self.stdout_lines[index]
                index += 1
                if not raw_line.strip():
                    continue
                try:
                    frame = json.loads(raw_line.decode("utf-8"))
                except (UnicodeDecodeError, json.JSONDecodeError) as error:
                    raise CheckFailure(f"{self.label}: live stdout is not JSON-RPC NDJSON") from error
                require(isinstance(frame, dict), f"{self.label}: live stdout JSON is not an object")
                if predicate(frame):
                    return frame
            if self.process.poll() is not None:
                self.pump(0)
                while index < len(self.stdout_lines):
                    raw_line = self.stdout_lines[index]
                    index += 1
                    if not raw_line.strip():
                        continue
                    try:
                        frame = json.loads(raw_line.decode("utf-8"))
                    except (UnicodeDecodeError, json.JSONDecodeError) as error:
                        raise CheckFailure(f"{self.label}: exited with non-JSON stdout") from error
                    if predicate(frame):
                        return frame
                raise CheckFailure(f"{self.label}: process exited before the expected response; stderr={utf8(bytes(self.stderr))!r}")
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise CheckFailure(f"{self.label}: timed out waiting for the expected response")
            self.pump(min(remaining, 0.2))

    def finish(self, timeout=TIMEOUT_SECONDS):
        if not self.stdin_closed and self.process.stdin is not None:
            self.process.stdin.close()
            self.process.stdin = None
            self.stdin_closed = True
        deadline = time.monotonic() + timeout
        while self.process.poll() is None and time.monotonic() < deadline:
            self.pump(min(0.1, max(0.0, deadline - time.monotonic())))
        terminated_after_timeout = False
        if self.process.poll() is None:
            terminated_after_timeout = True
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        while self.selector.get_map():
            self.pump(0.05)
        if self.stdout_buffer:
            self.stdout_lines.append(self.stdout_buffer)
            self.stdout_buffer = b""
        stdout = b"".join(line + b"\n" for line in self.stdout_lines)
        return {
            "label": self.label,
            "argv": self.argv,
            "exit_code": self.process.returncode,
            "stdout": stdout,
            "stderr": bytes(self.stderr),
            "terminated_after_timeout": terminated_after_timeout,
        }


def begin_live_modern(label, raw_opening, *, mode="", extra_env=None):
    capture_path = ARTIFACTS_DIR / "captures" / f"{label}.ndjson"
    capture_path.parent.mkdir(parents=True, exist_ok=True)
    require(not capture_path.exists(), f"{label}: capture path unexpectedly already exists")
    environment = {
        "MCP_MUX_MODERN_CAPTURE_FILE": str(capture_path),
        "MCP_MUX_MODERN_MODE": mode,
    }
    if extra_env:
        environment.update(extra_env)
    session = LiveSession(label, [MUX_BIN, POLICY, MODERN_FIXTURE_BIN], environment)
    session.send(raw_opening + b"\n")
    return session, capture_path


def select_modern_status(status_result, label):
    _, servers = decode_status(status_result, label)
    modern = [server for server in servers if isinstance(server, dict) and server.get("protocol_era") == "2026-07-28"]
    require(len(modern) == 1, f"{label}: expected exactly one active modern owner, found {len(modern)}")
    owner = modern[0]
    for key, value in MODERN_FACTS.items():
        require(owner.get(key) == value, f"{label}: {key} is {owner.get(key)!r}, expected {value!r}")
    require(owner.get("upstream_live") is True, f"{label}: modern owner readiness upstream_live is not true")
    require(isinstance(owner.get("session_count"), int) and owner["session_count"] >= 1, f"{label}: modern owner does not report an active session")
    prohibited = sorted(key for key in MODERN_PROHIBITED_KEYS if key in owner)
    require(not prohibited, f"{label}: modern status exposes prohibited keys: {', '.join(prohibited)}")
    return owner


def active_modern_readback(label):
    status = run_status(f"{label}-status")
    owner = select_modern_status(status, f"{label}-status")
    server_id = owner.get("server_id")
    require(isinstance(server_id, str) and server_id, f"{label}-status: modern owner has no server_id")
    return {"status": command_record(status), "owner": owner}


def await_no_active_owner(label):
    observations = []
    deadline = time.monotonic() + 10
    while True:
        status_result = run_status(label)
        parsed, servers = decode_status(status_result, label)
        observations.append(command_record(status_result))
        modern = [server for server in servers if isinstance(server, dict) and server.get("protocol_era") == "2026-07-28"]
        if not servers and not modern:
            return observations
        if time.monotonic() >= deadline:
            raise CheckFailure(f"{label}: owner state remained after stop: {json.dumps(parsed, sort_keys=True)}")
        time.sleep(0.1)


def assert_no_modern_after_refusal(status_result, label):
    parsed, servers = decode_status(status_result, label)
    require(not servers, f"{label}: refused modern admission left active status entries: {json.dumps(parsed, sort_keys=True)}")
    require("protocol_era" not in utf8(status_result["stdout"]), f"{label}: refusal status exposes a modern owner")


def corpus_frames():
    raw_lines = CORPUS_PATH.read_bytes().splitlines(keepends=True)
    frames = []
    for number, physical_line in enumerate(raw_lines, start=1):
        if physical_line.endswith(b"\n"):
            raw = physical_line[:-1]
        else:
            raw = physical_line
        require(raw.strip(), f"corpus line {number} is blank")
        try:
            decoded = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise CheckFailure(f"corpus line {number} is not valid JSON") from error
        require(isinstance(decoded, dict), f"corpus line {number} is not an object")
        require(decoded.get("jsonrpc") == "2.0", f"corpus line {number} lacks jsonrpc 2.0")
        require("id" in decoded and "method" in decoded, f"corpus line {number} lacks request id or method")
        params = decoded.get("params")
        require(isinstance(params, dict), f"corpus line {number} has non-object params")
        meta = params.get("_meta")
        require(isinstance(meta, dict), f"corpus line {number} has non-object modern metadata")
        require(meta.get("io.modelcontextprotocol/protocolVersion") == "2026-07-28", f"corpus line {number} has wrong modern version")
        require(isinstance(meta.get("io.modelcontextprotocol/clientCapabilities"), dict), f"corpus line {number} has non-object client capabilities")
        method = decoded["method"]
        require(method in {"tools/list", "server/discover", "ping"}, f"corpus line {number} has unsupported fixture method {method!r}")
        frames.append(
            {
                "line": number,
                "raw": raw,
                "request": decoded,
                "kind": "discover" if method == "server/discover" else "direct",
                "client_info": "io.modelcontextprotocol/clientInfo" in meta,
            }
        )
    require(len(frames) == 100, f"corpus denominator is {len(frames)}, expected exactly 100")
    return frames


def compact_corpus_observation(frame, observation, response):
    return {
        "corpus_line": frame["line"],
        "method": frame["request"]["method"],
        "request_id": frame["request"]["id"],
        "client_info_present": frame["client_info"],
        "fresh_daemon_owner": True,
        "fresh_owner_server_id": observation["active_status"]["owner"]["server_id"],
        "status": observation["active_status"]["status"],
        "capture": observation["capture"],
        "response": response,
        "process": observation["process"],
        "stop_before": observation["stop_before"],
        "stop_after": observation["stop_after"],
    }


def run_corpus_partition(frames, scenario_label, seen_owner_ids):
    observations = []
    for frame in frames:
        label = f"{scenario_label}-corpus-{frame['line']:03d}"
        observation = isolated_modern_once(label, frame["raw"], status_probe=active_modern_readback)
        owner_id = observation["active_status"]["owner"]["server_id"]
        require(owner_id not in seen_owner_ids, f"{label}: modern owner was reused across fresh corpus frames")
        seen_owner_ids.add(owner_id)
        response = response_for(observation["frames"], frame["request"]["id"], label)
        require(len(observation["frames"]) == 1, f"{label}: expected one native response and no generated frames")
        assert_native_result(frame["request"], response, label)
        observations.append(compact_corpus_observation(frame, observation, response))
    return observations


def scenario_three_admission():
    cases = [
        (
            "missing-meta",
            {
                "jsonrpc": "2.0",
                "id": "missing-meta",
                "method": "tools/list",
                "params": {"private": "admission-private-sentinel"},
            },
            -32602,
            "Invalid params",
            None,
        ),
        (
            "null-meta",
            {
                "jsonrpc": "2.0",
                "id": "null-meta",
                "method": "tools/list",
                "params": {"_meta": None, "private": "admission-private-sentinel"},
            },
            -32602,
            "Invalid params",
            None,
        ),
        (
            "unsupported-version",
            {
                "jsonrpc": "2.0",
                "id": "unsupported-version",
                "method": "tools/list",
                "params": {
                    "_meta": {
                        "io.modelcontextprotocol/protocolVersion": "2030-01-01",
                        "io.modelcontextprotocol/clientCapabilities": {},
                    },
                    "private": "admission-private-sentinel",
                },
            },
            -32022,
            "Unsupported protocol version",
            "2030-01-01",
        ),
    ]
    observations = []
    for name, request, code, message, requested_version in cases:
        label = f"scenario-3-{name}"
        raw = encoded_request(request)
        capture_path = ARTIFACTS_DIR / "captures" / f"{label}.ndjson"
        before = stop_daemon(f"{label}-before")
        after = None
        try:
            result = execute(
                label,
                [MUX_BIN, POLICY, MODERN_FIXTURE_BIN],
                input_bytes=raw + b"\n",
                extra_env={
                    "MCP_MUX_MODERN_CAPTURE_FILE": str(capture_path),
                    "MCP_MUX_MODERN_MODE": "",
                },
            )
            require(result["exit_code"] != 0, f"{label}: unsafe modern admission unexpectedly exited successfully")
            frames = parse_ndjson(result["stdout"], label)
            require(len(frames) == 1, f"{label}: expected exactly one JSON-RPC admission error")
            response = frames[0]
            require(response.get("jsonrpc") == "2.0", f"{label}: refusal does not use JSON-RPC 2.0")
            require(response.get("id") == request["id"], f"{label}: refusal did not retain the request ID")
            error = response.get("error")
            require(isinstance(error, dict), f"{label}: refusal has no JSON-RPC error object")
            require(error.get("code") == code and error.get("message") == message, f"{label}: error class is not {code} {message!r}")
            if requested_version is not None:
                data = error.get("data")
                require(isinstance(data, dict), f"{label}: unsupported-version error has no data object")
                require(data.get("supported") == ["2026-07-28"], f"{label}: unsupported-version supported set is wrong")
                require(data.get("requested") == requested_version, f"{label}: unsupported-version requested value is wrong")
            combined = result["stdout"] + result["stderr"]
            require(b"admission-private-sentinel" not in combined, f"{label}: refusal leaked request payload content")
            require(not capture_path.exists(), f"{label}: invalid opening launched the modern fixture")
            status = run_status(f"{label}-status")
            assert_no_modern_after_refusal(status, label)
            observations.append(
                {
                    "case": name,
                    "expected_error": {"code": code, "message": message},
                    "response": response,
                    "process": command_record(result),
                    "capture_created": False,
                    "status": command_record(status),
                    "stop_before": before,
                }
            )
        finally:
            after = stop_daemon(f"{label}-after")
        observations[-1]["stop_after"] = after
    return observations


def scenario_four_native_behavior():
    input_request = modern_request(
        "input-required-original",
        "tools/call",
        extra_params={"name": "modern_echo", "arguments": {"message": "initial"}},
    )
    input_observation = isolated_modern_once(
        "scenario-4-input-required",
        encoded_request(input_request),
        mode="input_required",
    )
    input_response = response_for(input_observation["frames"], input_request["id"], "scenario-4-input-required")
    input_result = input_response["result"]
    require(input_result.get("resultType") == "input_required", "scenario-4-input-required: resultType is not input_required")
    require("fixture_confirmation" in input_result.get("inputRequests", {}), "scenario-4-input-required: inputRequests are not native")
    require(input_result.get("requestState") == "fixture-opaque-request-state-v1", "scenario-4-input-required: opaque requestState changed")

    retry_request = modern_request(
        "input-required-fresh-retry",
        "tools/call",
        extra_params={
            "name": "modern_echo",
            "arguments": {"message": "fresh-retry"},
            "inputResponses": {"fixture_confirmation": {"confirmed": True}},
            "requestState": "fixture-opaque-request-state-v1",
        },
    )
    retry_observation = isolated_modern_once("scenario-4-fresh-retry", encoded_request(retry_request))
    retry_response = response_for(retry_observation["frames"], retry_request["id"], "scenario-4-fresh-retry")
    assert_native_result(retry_request, retry_response, "scenario-4-fresh-retry")
    require(
        input_observation["capture"]["opening_sha256"] != retry_observation["capture"]["opening_sha256"],
        "scenario-4-fresh-retry: new-ID retry did not reach a fresh native request path",
    )

    log_request = modern_request("request-log-opt-in", "tools/list", log_level="info")
    log_observation = isolated_modern_once("scenario-4-request-log", encoded_request(log_request), mode="request_log")
    logs = [
        frame
        for frame in log_observation["frames"]
        if frame.get("method") == "notifications/message"
        and isinstance(frame.get("params"), dict)
        and frame["params"].get("level") == "info"
        and frame["params"].get("logger") == "mock-modern-server"
        and frame["params"].get("data") == "request-scoped fixture log"
    ]
    require(len(logs) == 1, f"scenario-4-request-log: opted-in log count is {len(logs)}, expected one")
    log_response = response_for(log_observation["frames"], log_request["id"], "scenario-4-request-log")
    assert_native_result(log_request, log_response, "scenario-4-request-log")

    server_request = modern_request(
        "contained-upstream-request",
        "tools/call",
        extra_params={"name": "modern_echo", "arguments": {"message": "contain"}},
    )
    contained_observation = isolated_modern_once(
        "scenario-4-server-request",
        encoded_request(server_request),
        mode="server_request",
    )
    require(
        not any(
            frame.get("method") == "sampling/createMessage" or frame.get("id") == "fixture-server-request-1"
            for frame in contained_observation["frames"]
        ),
        "scenario-4-server-request: upstream JSON-RPC request reached the downstream host",
    )
    contained_response = response_for(contained_observation["frames"], server_request["id"], "scenario-4-server-request")
    assert_native_result(server_request, contained_response, "scenario-4-server-request")

    return {
        "input_required": {"request": input_request, "response": input_response, "observation": input_observation},
        "fresh_retry": {"request": retry_request, "response": retry_response, "observation": retry_observation},
        "request_log": {"request": log_request, "response": log_response, "observation": log_observation},
        "contained_server_request": {
            "request": server_request,
            "response": contained_response,
            "observation": contained_observation,
        },
    }


def scenario_five_lifecycle():
    loss_request = modern_request(
        "loss-after-result",
        "tools/call",
        extra_params={"name": "modern_echo", "arguments": {"message": "loss"}},
    )
    fresh_request = modern_request(
        "fresh-after-loss",
        "tools/call",
        extra_params={"name": "modern_echo", "arguments": {"message": "fresh-after-loss"}},
    )
    before = stop_daemon("scenario-5-before")
    session = None
    stop_command = None
    try:
        loss_raw = encoded_request(loss_request)
        session, capture_path = begin_live_modern(
            "scenario-5-loss-replacement",
            loss_raw,
            mode="loss_after_result",
        )
        loss_response = session.wait_for(
            lambda frame: frame.get("id") == loss_request["id"] and "result" in frame
        )
        assert_native_result(loss_request, loss_response, "scenario-5-loss-replacement")

        deadline = time.monotonic() + 15
        capture_reset = False
        while time.monotonic() < deadline:
            if capture_path.is_file() and capture_path.stat().st_size == 0:
                capture_reset = True
                break
            time.sleep(0.025)
        require(capture_reset, "scenario-5-loss-replacement: replacement capture did not reset before fresh host traffic")

        replacement_status = run_status("scenario-5-replacement-status")
        replacement_owner = select_modern_status(replacement_status, "scenario-5-replacement-status")
        require(capture_path.stat().st_size == 0, "scenario-5-replacement-status: status caused bootstrap or replay traffic")

        fresh_raw = encoded_request(fresh_request)
        session.send(fresh_raw + b"\n")
        fresh_response = session.wait_for(
            lambda frame: frame.get("id") == fresh_request["id"] and "result" in frame
        )
        assert_native_result(fresh_request, fresh_response, "scenario-5-fresh-after-loss")
        fresh_reset_deadline = time.monotonic() + 15
        fresh_capture_reset = False
        while time.monotonic() < fresh_reset_deadline:
            if capture_path.is_file() and capture_path.stat().st_size == 0:
                fresh_capture_reset = True
                break
            time.sleep(0.025)
        require(fresh_capture_reset, "scenario-5-fresh-after-loss: next replacement did not quarantine the completed request capture")
        fresh_capture = {
            "path": relative(capture_path),
            "state": "reset_to_empty_after_terminal_result",
            "sha256": sha256_bytes(b""),
        }

        live_process = session.finish(timeout=0.1)
        stop_command = stop_daemon("scenario-5-quarantine-stop")
        status_after = await_no_active_owner("scenario-5-status-after")
    finally:
        if session is not None and session.process.poll() is None:
            try:
                session.finish(timeout=0.1)
            except CheckFailure:
                pass
        if stop_command is None:
            stop_daemon("scenario-5-stop-recovery", strict=False)
        stop_after = stop_daemon("scenario-5-after")

    return {
        "no_legacy_bootstrap_or_replay": True,
        "loss_after_result": {
            "request": loss_request,
            "response": loss_response,
            "replacement_capture_empty_before_fresh_request": capture_reset,
        },
        "fresh_after_loss": {
            "request": fresh_request,
            "response": fresh_response,
            "capture": fresh_capture,
        },
        "quarantine": {
            "replacement_status": command_record(replacement_status),
            "modern_owner_before_stop": replacement_owner,
            "stop_before": before,
            "stop_command": stop_command,
            "live_process": command_record(live_process),
            "status_after": status_after,
            "stop_after": stop_after,
        },
    }


def scenario_six_readback():
    sentinels = {
        "request": "r1-request-payload-sentinel",
        "opaque": "r1-opaque-state-sentinel",
        "credential": "r1-credential-sentinel",
        "environment": "r1-environment-sentinel",
        "progress": "r1-progress-token-sentinel",
        "subscription": "r1-subscription-sentinel",
        "compatibility": "r1-compatibility-key-sentinel",
    }
    request = modern_request(
        "readback-active",
        "tools/list",
        extra_params={
            "private": sentinels["request"],
            "requestState": sentinels["opaque"],
            "credential": sentinels["credential"],
            "progressToken": sentinels["progress"],
            "subscriptionId": sentinels["subscription"],
            "compatibilityKey": sentinels["compatibility"],
        },
    )
    before = stop_daemon("scenario-6-before")
    session = None
    try:
        session, capture_path = begin_live_modern(
            "scenario-6-live",
            encoded_request(request),
            extra_env={"MCP_MUX_R1_ENV_SENTINEL": sentinels["environment"]},
        )
        response = session.wait_for(lambda frame: frame.get("id") == request["id"] and "result" in frame)
        assert_native_result(request, response, "scenario-6-live")
        capture = exact_capture(capture_path, encoded_request(request), "scenario-6-live")
        status = run_status("scenario-6-status")
        modern_owner = select_modern_status(status, "scenario-6-status")
        status_text = utf8(status["stdout"])
        leaked = sorted(name for name, value in sentinels.items() if value in status_text)
        require(not leaked, f"scenario-6-status: redacted status leaked sentinels: {', '.join(leaked)}")
        live_process = session.finish(timeout=0.1)
        stop_command = stop_daemon("scenario-6-stop")
    finally:
        if session is not None and session.process.poll() is None:
            try:
                session.finish(timeout=0.1)
                stop_daemon("scenario-6-stop-recovery", strict=False)
            except CheckFailure:
                pass
        after = stop_daemon("scenario-6-after")

    return {
        "request": request,
        "response": response,
        "capture": capture,
        "status": command_record(status),
        "modern_owner": modern_owner,
        "redaction_sentinels": sorted(sentinels),
        "live_process": command_record(live_process),
        "stop_before": before,
        "stop_command": stop_command,
        "stop_after": after,
    }


def scenario_seven_legacy():
    init_request = {
        "jsonrpc": "2.0",
        "id": "legacy-initialize",
        "method": "initialize",
        "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "legacy-proof", "version": "1.0"}},
    }
    initialized = {"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}}
    list_request = {"jsonrpc": "2.0", "id": "legacy-tools-list", "method": "tools/list", "params": {}}
    call_request = {
        "jsonrpc": "2.0",
        "id": "legacy-tools-call",
        "method": "tools/call",
        "params": {"name": "echo", "arguments": {"message": "legacy-customer-proof"}},
    }
    before = stop_daemon("scenario-7-before")
    session = None
    try:
        session = LiveSession("scenario-7-live", [MUX_BIN, LEGACY_FIXTURE_BIN])
        session.send(encoded_request(init_request) + b"\n")
        init_response = session.wait_for(lambda frame: frame.get("id") == init_request["id"] and "result" in frame)
        require(init_response.get("result", {}).get("serverInfo", {}).get("name") == "mock-server", "scenario-7-live: legacy initialize did not reach mock-server")
        session.send(encoded_request(initialized) + b"\n")
        session.send(encoded_request(list_request) + b"\n")
        list_response = session.wait_for(lambda frame: frame.get("id") == list_request["id"] and "result" in frame)
        legacy_tools = list_response.get("result", {}).get("tools", [])
        require(any(tool.get("name") == "echo" for tool in legacy_tools if isinstance(tool, dict)), "scenario-7-live: legacy tools/list omitted echo")
        session.send(encoded_request(call_request) + b"\n")
        call_response = session.wait_for(lambda frame: frame.get("id") == call_request["id"] and "result" in frame)
        content = call_response.get("result", {}).get("content", [])
        require(
            any("legacy-customer-proof" in item.get("text", "") for item in content if isinstance(item, dict)),
            "scenario-7-live: legacy tools/call did not return fixture output",
        )
        status = run_status("scenario-7-status")
        _, servers = decode_status(status, "scenario-7-status")
        legacy_servers = [server for server in servers if isinstance(server, dict) and "protocol_era" not in server]
        require(len(legacy_servers) == 1, f"scenario-7-status: expected exactly one legacy owner, found {len(legacy_servers)}")
        legacy_owner = legacy_servers[0]
        for key in MODERN_FACTS:
            require(key not in legacy_owner, f"scenario-7-status: legacy owner exposes modern field {key}")
        server_id = legacy_owner.get("server_id")
        require(isinstance(server_id, str) and server_id, "scenario-7-status: legacy owner has no deterministic server_id")
        require("protocol_era" not in utf8(status["stdout"]), "scenario-7-status: no-selector legacy status exposed modern policy")
        live_process = session.finish(timeout=0.1)
        stop_command = stop_daemon("scenario-7-stop")
    finally:
        if session is not None and session.process.poll() is None:
            try:
                session.finish(timeout=0.1)
                stop_daemon("scenario-7-stop-recovery", strict=False)
            except CheckFailure:
                pass
        after = stop_daemon("scenario-7-after")

    return {
        "policy_selector": None,
        "requests": [init_request, initialized, list_request, call_request],
        "responses": [init_response, list_response, call_response],
        "status": command_record(status),
        "legacy_identity": {
            "server_id": server_id,
            "ipc_path": legacy_owner.get("ipc_path"),
            "command": legacy_owner.get("command"),
            "args": legacy_owner.get("args"),
            "output_sha256": sha256_bytes(live_process["stdout"]),
        },
        "live_process": command_record(live_process),
        "stop_command": stop_command,
        "stop_before": before,
        "stop_after": after,
    }


def scenario_eight_rollback():
    request = modern_request("rollback-modern-owner", "tools/list")
    before = stop_daemon("scenario-8-before")
    session = None
    try:
        session, capture_path = begin_live_modern("scenario-8-live", encoded_request(request))
        response = session.wait_for(lambda frame: frame.get("id") == request["id"] and "result" in frame)
        assert_native_result(request, response, "scenario-8-live")
        capture = exact_capture(capture_path, encoded_request(request), "scenario-8-live")
        status_before = run_status("scenario-8-status-identify")
        modern_owner = select_modern_status(status_before, "scenario-8-status-identify")
        modern_server_id = modern_owner.get("server_id")
        require(isinstance(modern_server_id, str) and modern_server_id, "scenario-8-status-identify: modern owner has no server_id")
        live_process = session.finish(timeout=0.1)
        stop_command = stop_daemon("scenario-8-stop-force")
        status_after = await_no_active_owner("scenario-8-status-after")
    finally:
        if session is not None and session.process.poll() is None:
            try:
                session.finish(timeout=0.1)
                stop_daemon("scenario-8-stop-recovery", strict=False)
            except CheckFailure:
                pass
        after = stop_daemon("scenario-8-after")

    return {
        "new_modern_admissions_after_identification": "not attempted",
        "request": request,
        "response": response,
        "capture": capture,
        "modern_owner_before_stop": modern_owner,
        "modern_server_id": modern_server_id,
        "no_downgrade_or_replay": True,
        "status_before": command_record(status_before),
        "stop_before": before,
        "stop_command": stop_command,
        "live_process": command_record(live_process),
        "status_after": status_after,
        "stop_after": after,
    }


def transcript_record(identifier, expected, observed, artifact_path, commands):
    append_transcript(
        {
            "schema_version": 1,
            "scenario_id": str(identifier),
            "expected": expected,
            "observed": observed,
            "verdict": "PASS",
            "command_references": commands,
            "artifact_references": [artifact_path],
        }
    )


def main():
    for required in (SOURCE_ROOT, OUTPUT_DIR, BASE_DIR, RUNTIME_DIR, MUX_BIN, MODERN_FIXTURE_BIN, LEGACY_FIXTURE_BIN, CORPUS_PATH):
        require(required.exists(), f"required runner path is missing: {required}")
    require(len(SOURCE_SHA) >= 7 and all(character in "0123456789abcdefABCDEF" for character in SOURCE_SHA), "source SHA is not hexadecimal")

    completed = False
    try:
        corpus = corpus_frames()
        direct = [frame for frame in corpus if frame["kind"] == "direct"]
        discover = [frame for frame in corpus if frame["kind"] == "discover"]
        counts = {
            "total": len(corpus),
            "direct": len(direct),
            "discover": len(discover),
            "client_info_present": sum(1 for frame in corpus if frame["client_info"]),
            "client_info_absent": sum(1 for frame in corpus if not frame["client_info"]),
        }
        require(all(counts[name] > 0 for name in ("direct", "discover", "client_info_present", "client_info_absent")), "corpus does not cover direct/discover and both clientInfo variants")

        seen_corpus_owners = set()
        direct_observations = run_corpus_partition(direct, "scenario-1", seen_corpus_owners)
        discover_observations = run_corpus_partition(discover, "scenario-2", seen_corpus_owners)
        variation_direct = direct_observations[0]["response"]
        variation_discover = discover_observations[0]["response"]
        require(variation_direct.get("id") != variation_discover.get("id"), "corpus I/O variation did not yield distinct response IDs")
        require(variation_direct.get("result") != variation_discover.get("result"), "corpus I/O variation did not yield distinct native results")
        modern_artifact = ARTIFACTS_DIR / "modern.json"
        write_json(
            modern_artifact,
            {
                "corpus": {
                    "path": evidence_or_absolute(CORPUS_PATH),
                    "sha256": sha256_file(CORPUS_PATH),
                    "counts": counts,
                    "passed": len(direct_observations) + len(discover_observations),
                },
                "direct_openers": direct_observations,
                "discover_openers": discover_observations,
                "io_variation": {
                    "direct_response": variation_direct,
                    "discover_response": variation_discover,
                },
            },
        )
        modern_ref = relative(modern_artifact)
        transcript_record(
            1,
            {"all_direct_openers": len(direct), "byte_exact_first_upstream_line": True, "native_result": True},
            {"passed": len(direct_observations), "client_info_present": sum(1 for item in direct_observations if item["client_info_present"]), "client_info_absent": sum(1 for item in direct_observations if not item["client_info_present"])},
            modern_ref,
            [[str(MUX_BIN), POLICY, str(MODERN_FIXTURE_BIN)], [str(MUX_BIN), "status"], [str(MUX_BIN), "stop", "--force"]],
        )
        transcript_record(
            2,
            {"all_host_sent_discover_openers": len(discover), "byte_exact_first_upstream_line": True, "native_result": True},
            {"passed": len(discover_observations), "client_info_present": sum(1 for item in discover_observations if item["client_info_present"]), "client_info_absent": sum(1 for item in discover_observations if not item["client_info_present"])},
            modern_ref,
            [[str(MUX_BIN), POLICY, str(MODERN_FIXTURE_BIN)], [str(MUX_BIN), "stop", "--force"]],
        )

        admission = scenario_three_admission()
        admission_artifact = ARTIFACTS_DIR / "admission.json"
        write_json(admission_artifact, {"cases": admission})
        transcript_record(
            3,
            {"invalid_params": -32602, "unsupported_version": -32022, "no_upstream_or_fallback": True},
            {"cases": len(admission), "capture_files_created": 0},
            relative(admission_artifact),
            [[str(MUX_BIN), POLICY, str(MODERN_FIXTURE_BIN)], [str(MUX_BIN), "status"], [str(MUX_BIN), "stop", "--force"]],
        )

        native_behavior = scenario_four_native_behavior()
        lifecycle_artifact = ARTIFACTS_DIR / "lifecycle.json"
        write_json(lifecycle_artifact, {"native_directionality": native_behavior})
        transcript_record(
            4,
            {"input_required_native": True, "fresh_retry": True, "one_opted_log": True, "upstream_request_contained": True},
            {"input_required": "PASS", "fresh_retry": "PASS", "request_log": "PASS", "server_request": "PASS"},
            relative(lifecycle_artifact),
            [[str(MUX_BIN), POLICY, str(MODERN_FIXTURE_BIN)], [str(MUX_BIN), "stop", "--force"]],
        )

        lifecycle = scenario_five_lifecycle()
        write_json(lifecycle_artifact, {"native_directionality": native_behavior, "quarantine": lifecycle})
        transcript_record(
            5,
            {"loss_after_result": "native result then fresh exact-era request", "status_stop_quarantine": True, "no_legacy_bootstrap_or_replay": True},
            {"loss_after_result": "PASS", "fresh_after_loss": "PASS", "stop_removal": "PASS"},
            relative(lifecycle_artifact),
            [[str(MUX_BIN), POLICY, str(MODERN_FIXTURE_BIN)], [str(MUX_BIN), "status"], [str(MUX_BIN), "stop", "--force"]],
        )

        readback = scenario_six_readback()
        readback_artifact = ARTIFACTS_DIR / "readback.json"
        write_json(readback_artifact, readback)
        transcript_record(
            6,
            {"policy_facts": MODERN_FACTS, "readiness": "upstream_live with active session", "redaction": True, "no_r3_keys": True},
            {"policy_facts": MODERN_FACTS, "redaction_sentinels_absent": True},
            relative(readback_artifact),
            [[str(MUX_BIN), POLICY, str(MODERN_FIXTURE_BIN)], [str(MUX_BIN), "status"], [str(MUX_BIN), "stop", "--force"]],
        )

        legacy = scenario_seven_legacy()
        legacy_artifact = ARTIFACTS_DIR / "legacy.json"
        write_json(legacy_artifact, legacy)
        transcript_record(
            7,
            {"no_modern_selector": True, "initialize_discovery_tool_flow": True, "no_modern_policy_fields": True},
            {"legacy_identity": legacy["legacy_identity"], "response_count": len(legacy["responses"])},
            relative(legacy_artifact),
            [[str(MUX_BIN), str(LEGACY_FIXTURE_BIN)], [str(MUX_BIN), "status"], [str(MUX_BIN), "stop", "--force"]],
        )

        rollback = scenario_eight_rollback()
        rollback_artifact = ARTIFACTS_DIR / "rollback.json"
        write_json(rollback_artifact, rollback)
        transcript_record(
            8,
            {"identify_active_modern_owner": True, "force_stop": True, "no_modern_owner_afterward": True, "no_downgrade_or_replay": True},
            {"modern_server_id": rollback["modern_server_id"], "post_stop_owner_state": "empty"},
            relative(rollback_artifact),
            [[str(MUX_BIN), POLICY, str(MODERN_FIXTURE_BIN)], [str(MUX_BIN), "status"], [str(MUX_BIN), "stop", "--force"]],
        )

        final_cleanup = stop_daemon("final-cleanup")
        write_json(ARTIFACTS_DIR / "cleanup.json", {"final_cleanup": final_cleanup})

        artifact_paths = {
            "modern": modern_artifact,
            "admission": admission_artifact,
            "lifecycle": lifecycle_artifact,
            "readback": readback_artifact,
            "legacy": legacy_artifact,
            "rollback": rollback_artifact,
        }
        artifact_hashes = {name: sha256_file(path) for name, path in artifact_paths.items()}
        transcript_sha256 = sha256_file(TRANSCRIPT_PATH)

        summary = {
            "schema_version": 1,
            "result": "PASS",
            "platform_id": f"{platform.system().lower()}-{platform.machine().lower()}",
            "source_sha": SOURCE_SHA.lower(),
            "binary_sha256": sha256_file(MUX_BIN),
            "fixture_sha256": sha256_file(MODERN_FIXTURE_BIN),
            "corpus_sha256": sha256_file(CORPUS_PATH),
            "corpus_total": 100,
            "corpus_passed": 100,
            "policy": POLICY,
            "scenario_results": {str(identifier): "PASS" for identifier in range(1, 9)},
            "artifacts": {name: relative(path) for name, path in artifact_paths.items()},
            "artifacts_sha256": artifact_hashes,
            "transcript_sha256": transcript_sha256,
            "base_dir": str(BASE_DIR),
            "base_preserved": KEEP_BASE_DIR,
            "binary_path": str(MUX_BIN),
            "modern_fixture_path": str(MODERN_FIXTURE_BIN),
            "legacy_fixture_path": str(LEGACY_FIXTURE_BIN),
            "legacy_fixture_sha256": sha256_file(LEGACY_FIXTURE_BIN),
            "go_version": GO_VERSION,
            "os": {
                "system": platform.system(),
                "release": platform.release(),
                "machine": platform.machine(),
                "python": platform.python_version(),
            },
        }
        write_json(OUTPUT_DIR / "summary.json", summary)
        completed = True
        return 0
    except Exception as error:
        failure = {
            "result": "FAIL",
            "error": str(error),
            "exception_type": type(error).__name__,
            "traceback": traceback.format_exc(),
        }
        try:
            write_json(ARTIFACTS_DIR / "failure.json", failure)
        except OSError:
            pass
        print(f"[{POLICY}] FAIL: {error}", file=sys.stderr)
        return 1
    finally:
        if not completed:
            try:
                cleanup = stop_daemon("failure-cleanup", strict=False)
                write_json(ARTIFACTS_DIR / "failure-cleanup.json", {"cleanup": cleanup})
            except Exception:
                pass


if __name__ == "__main__":
    sys.exit(main())
PY
}

log 'running the native R1 evidence harness'
if run_harness; then
  evidence_durable=1
  log "PASS: wrote R1 native-isolation evidence to $output_dir"
else
  runner_status=$?
  exit "$runner_status"
fi
