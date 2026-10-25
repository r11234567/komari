#!/usr/bin/env bash
# CI only: never run dependency downloads, builds or tests on the panel VPS.
set -uo pipefail
if [ "${GITHUB_ACTIONS:-}" != true ]; then
  echo "Feature audit commands must run in GitHub Actions, not on the VPS." >&2
  exit 2
fi
if [ "$#" -lt 2 ] || [[ ! "$1" =~ ^[a-zA-Z0-9_-]+$ ]]; then
  echo "Usage: run-audit-check.sh CHECK_NAME COMMAND [ARG ...]" >&2
  exit 2
fi
name="$1"
shift
log_dir="${KOMARI_AUDIT_LOG_DIR:-ci-artifacts}"
mkdir -p "$log_dir"
export KOMARI_TEST_LOG_DIR="$PWD/$log_dir/$name-go"
python3 - "$log_dir/$name-context.json" "$@" <<'CONTEXT'
import datetime, json, os, platform, subprocess, sys
from pathlib import Path
metadata = {
    "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "command": sys.argv[2:], "platform": platform.platform(),
    "repository": os.environ.get("GITHUB_REPOSITORY"),
    "run_id": os.environ.get("GITHUB_RUN_ID"),
    "run_attempt": os.environ.get("GITHUB_RUN_ATTEMPT"),
}
for key, command in {
    "commit": ["git", "rev-parse", "HEAD"],
    "submodules": ["git", "submodule", "status", "--recursive"],
}.items():
    result = subprocess.run(command, capture_output=True, text=True)
    metadata[key] = result.stdout.strip()
Path(sys.argv[1]).write_text(json.dumps(metadata, indent=2) + "\n")
CONTEXT
printf 'CHECK %s\nCOMMAND ' "$name"
printf '%q ' "$@"
printf '\n'
timeout --signal=TERM --kill-after=30s "${KOMARI_AUDIT_TIMEOUT:-20m}" "$@" 2>&1 | tee "$log_dir/$name.log"
statuses=("${PIPESTATUS[@]}")
status="${statuses[0]}"
if [ "$status" -eq 0 ] && [ "${statuses[1]}" -ne 0 ]; then status="${statuses[1]}"; fi
printf '%s\n' "$status" > "$log_dir/$name-exit-code.txt"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    printf '### %s\n\nExit code: `%s`. Full logs are in the feature-audit artifact.\n\n' "$name" "$status"
    if [ "$status" -ne 0 ]; then
      printf '<details><summary>Last 80 log lines</summary>\n\n```text\n'
      tail -n 80 "$log_dir/$name.log"
      printf '\n```\n</details>\n\n'
    fi
  } >> "$GITHUB_STEP_SUMMARY"
fi
exit "$status"
