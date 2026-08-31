#!/usr/bin/env bash
set -euo pipefail

readonly POLICY='--mcp-protocol=2026-07-28'
readonly SCRIPT_NAME='verify-r1-native-isolation.contract'

log() {
  printf '[%s] %s\n' "$SCRIPT_NAME" "$*" >&2
}

die() {
  log "FATAL: $*"
  exit 2
}

fail() {
  log "FAIL: $*"
  exit 1
}

usage() {
  cat <<'USAGE'
Usage: scripts/verify-r1-native-isolation.contract.sh [options]

Options:
  --source-root DIR  Candidate source root. Defaults to the repository containing this script.
  --output-dir DIR   Fresh directory for runner evidence. It must be empty when it already exists.
  --keep-base-dir    Preserve the runner's fresh BaseDir for inspection.
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

validate_evidence() {
  local evidence_dir=$1
  local scratch_dir=$2

  "$PYTHON_BIN" - "$evidence_dir" "$scratch_dir" <<'PY'
import json
import os
import re
import sys
from pathlib import Path

output_dir = Path(sys.argv[1]).resolve()
scratch_dir = Path(sys.argv[2]).resolve()


def invalid(message: str) -> None:
    raise ValueError(message)


def require_string(value: object, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        invalid(f"{label} must be a non-empty string")
    return value


def require_int(value: object, label: str) -> int:
    if type(value) is not int:
        invalid(f"{label} must be an integer")
    return value


def require_sha256(value: object, label: str) -> None:
    value = require_string(value, label)
    if re.fullmatch(r"[0-9a-fA-F]{64}", value) is None:
        invalid(f"{label} must be a SHA-256 hex digest")


def require_source_sha(value: object) -> None:
    value = require_string(value, "source_sha")
    if re.fullmatch(r"[0-9a-fA-F]{7,64}", value) is None:
        invalid("source_sha must be a hexadecimal Git object ID")


def require_nonempty_file(path: Path, label: str) -> None:
    if not path.is_file():
        invalid(f"{label} is missing or is not a regular file: {path}")
    if path.stat().st_size == 0:
        invalid(f"{label} is empty: {path}")


def under(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
    except ValueError:
        return False
    return True


def resolve_artifact(raw_path: object, label: str) -> Path:
    raw_path = require_string(raw_path, f"artifacts.{label}")
    candidate = Path(raw_path)
    if not candidate.is_absolute():
        candidate = output_dir / candidate
    candidate = candidate.resolve()
    if not (under(candidate, output_dir) or under(candidate, scratch_dir)):
        invalid(f"artifacts.{label} escapes the fresh output or temporary directory")
    require_nonempty_file(candidate, f"artifacts.{label}")
    return candidate


summary_path = output_dir / "summary.json"
transcript_path = output_dir / "transcript.ndjson"
require_nonempty_file(summary_path, "summary.json")
require_nonempty_file(transcript_path, "transcript.ndjson")

try:
    with summary_path.open(encoding="utf-8") as summary_file:
        summary = json.load(summary_file)
except (OSError, json.JSONDecodeError) as error:
    invalid(f"summary.json is not valid JSON: {error}")

if not isinstance(summary, dict):
    invalid("summary.json must contain a JSON object")

if require_int(summary.get("schema_version"), "schema_version") != 1:
    invalid("schema_version must equal 1")
if summary.get("result") != "PASS":
    invalid("result must equal PASS")
require_string(summary.get("platform_id"), "platform_id")
require_source_sha(summary.get("source_sha"))
require_sha256(summary.get("binary_sha256"), "binary_sha256")
require_sha256(summary.get("fixture_sha256"), "fixture_sha256")
require_sha256(summary.get("corpus_sha256"), "corpus_sha256")
if require_int(summary.get("corpus_total"), "corpus_total") != 100:
    invalid("corpus_total must equal 100")
if require_int(summary.get("corpus_passed"), "corpus_passed") != 100:
    invalid("corpus_passed must equal 100")
if summary.get("policy") != "--mcp-protocol=2026-07-28":
    invalid("policy must equal --mcp-protocol=2026-07-28")

scenario_results = summary.get("scenario_results")
if not isinstance(scenario_results, dict):
    invalid("scenario_results must be an object")
for scenario_id in map(str, range(1, 9)):
    if scenario_results.get(scenario_id) != "PASS":
        invalid(f"scenario_results.{scenario_id} must equal PASS")

artifacts = summary.get("artifacts")
if not isinstance(artifacts, dict):
    invalid("artifacts must be an object")
for artifact_name in ("modern", "admission", "lifecycle", "readback", "legacy", "rollback"):
    resolve_artifact(artifacts.get(artifact_name), artifact_name)

line_number = 0
transcript_count = 0
try:
    with transcript_path.open(encoding="utf-8") as transcript_file:
        for line_number, line in enumerate(transcript_file, start=1):
            if not line.strip():
                continue
            json.loads(line)
            transcript_count += 1
except (OSError, json.JSONDecodeError) as error:
    invalid(f"transcript.ndjson has invalid JSON at line {line_number}: {error}")

if transcript_count == 0:
    invalid("transcript.ndjson must contain at least one JSON record")
PY
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
      [[ -n "$source_root_arg" ]] || die "missing value for --source-root"
      shift
      ;;
    --output-dir)
      require_option_value "$1" "$#"
      output_dir_arg=$2
      shift 2
      ;;
    --output-dir=*)
      output_dir_arg=${1#*=}
      [[ -n "$output_dir_arg" ]] || die "missing value for --output-dir"
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

for required_input in go.mod testdata/mock_modern_server.go testdata/modern_opening_corpus.ndjson; do
  [[ -f "$source_root/$required_input" ]] || die "source root is missing $required_input: $source_root"
done

PYTHON_BIN="$(find_python)" || die "Python 3 is required to validate JSON evidence"
if [[ -n "${MCP_MUX_R1_GO:-}" ]]; then
  GO_BIN="$MCP_MUX_R1_GO"
else
  GO_BIN="$(command -v go)" || die "Go is required to run the candidate runner"
fi
[[ -x "$GO_BIN" ]] || die "Go executable is unavailable: $GO_BIN"
GOTOOLCHAIN=local "$GO_BIN" version >/dev/null 2>&1 || die "the selected Go executable cannot run with GOTOOLCHAIN=local"
ambient_gomodcache="$(GOTOOLCHAIN=local "$GO_BIN" env GOMODCACHE)" || die "failed to resolve the existing Go module cache"
[[ -d "$ambient_gomodcache" ]] || die "existing Go module cache is unavailable: $ambient_gomodcache"

scratch_parent=${TMPDIR:-/tmp}
scratch_dir="$(mktemp -d "$scratch_parent/mcp-r1-native-isolation-contract.XXXXXX")" || die "failed to create fresh temporary directory"

cleanup() {
  local status=$?
  trap - EXIT
  rm -rf -- "$scratch_dir" || true
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ -n "$output_dir_arg" ]]; then
  [[ ! -L "$output_dir_arg" ]] || die "output directory must not be a symbolic link: $output_dir_arg"
  if [[ -e "$output_dir_arg" ]]; then
    [[ -d "$output_dir_arg" ]] || die "output path is not a directory: $output_dir_arg"
    [[ -z "$(find "$output_dir_arg" -mindepth 1 -maxdepth 1 -print -quit)" ]] || die "output directory must be empty: $output_dir_arg"
  else
    output_parent="$(cd -- "$(dirname -- "$output_dir_arg")" && pwd -P)" || die "output directory parent does not exist: $output_dir_arg"
    output_leaf=$(basename -- "$output_dir_arg")
    [[ "$output_leaf" != '.' && "$output_leaf" != '..' ]] || die "output directory must name a fresh directory: $output_dir_arg"
    mkdir -- "$output_parent/$output_leaf" || die "failed to create output directory: $output_dir_arg"
  fi
  output_dir="$(cd -- "$output_dir_arg" && pwd -P)"
else
  output_dir="$scratch_dir/output"
  mkdir -- "$output_dir"
fi

incomplete_dir="$scratch_dir/incomplete"
mkdir -- "$incomplete_dir"
printf '%s\n' '{"schema_version":1,"result":"PASS","platform_id":"incomplete"}' > "$incomplete_dir/summary.json"
printf '%s\n' '{}' > "$incomplete_dir/transcript.ndjson"

log "proving the validator rejects incomplete evidence"
if validate_evidence "$incomplete_dir" "$scratch_dir" >/dev/null 2>&1; then
  fail "validator accepted deliberately incomplete evidence"
fi
log "validator rejected incomplete evidence"

runner="$source_root/scripts/verify-r1-native-isolation.sh"
[[ -f "$runner" && -s "$runner" ]] || die "runner is required but missing or empty: $runner"

runner_temp="$scratch_dir/runner-temp"
mkdir -- "$runner_temp" "$runner_temp/go-cache" "$runner_temp/go-mod-cache" "$runner_temp/go-tmp" "$runner_temp/home" "$runner_temp/xdg-cache" "$runner_temp/xdg-config" "$runner_temp/xdg-data"
runner_args=(--source-root "$source_root" --output-dir "$output_dir")
if (( keep_base_dir )); then
  runner_args+=(--keep-base-dir)
fi

log "invoking the runner with fresh temporary and output directories"
if (
  export TMPDIR="$runner_temp"
  export TMP="$runner_temp"
  export TEMP="$runner_temp"
  export GOCACHE="$runner_temp/go-cache"
  export MCP_MUX_R1_GOMODCACHE="$ambient_gomodcache"
  export MCP_MUX_R1_GO="$GO_BIN"
  export GOTOOLCHAIN=local
  export GOTMPDIR="$runner_temp/go-tmp"
  export HOME="$runner_temp/home"
  export XDG_CACHE_HOME="$runner_temp/xdg-cache"
  export XDG_CONFIG_HOME="$runner_temp/xdg-config"
  export XDG_DATA_HOME="$runner_temp/xdg-data"
  bash "$runner" "${runner_args[@]}"
); then
  :
else
  runner_status=$?
  fail "runner exited with status $runner_status"
fi

log "validating runner evidence"
validate_evidence "$output_dir" "$scratch_dir"
log "PASS: validated R1 native-isolation evidence in $output_dir"

if [[ -z "$output_dir_arg" ]]; then
  log "the default output directory is temporary; use --output-dir DIR to retain it"
fi
