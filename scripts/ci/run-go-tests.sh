#!/usr/bin/env bash
# Full test/build output is retained even when go test fails or times out.
set -uo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
log_dir="${KOMARI_TEST_LOG_DIR:-$root/ci-artifacts}"
mkdir -p "$log_dir"
{ date -u; git rev-parse HEAD; go version; go env GOOS GOARCH GOTOOLCHAIN; } > "$log_dir/context.log" 2>&1
if [ "$#" -eq 0 ]; then set -- ./...; fi
export GOMAXPROCS="${GOMAXPROCS:-2}"
# Wait for the stderr logger as well as stdout, and propagate either logger's
# failure. Otherwise a full disk can produce a green check with missing logs.
exec 3> >(tee "$log_dir/build-stderr.log" >&2)
stderr_logger=$!
go test -json -count=1 -timeout=12m -p=2 "$@" 2>&3 | tee "$log_dir/tests.jsonl"
statuses=("${PIPESTATUS[@]}")
status="${statuses[0]}"
if [ "$status" -eq 0 ] && [ "${statuses[1]}" -ne 0 ]; then status="${statuses[1]}"; fi
exec 3>&-
wait "$stderr_logger"
stderr_status=$?
if [ "$status" -eq 0 ] && [ "$stderr_status" -ne 0 ]; then status="$stderr_status"; fi
printf '%s\n' "$status" > "$log_dir/exit-code.txt"
python3 - "$log_dir" <<'REPORT'
import json,sys
from pathlib import Path
p=Path(sys.argv[1]);failed=[]
with (p/'tests.log').open('w') as human:
 for line in (p/'tests.jsonl').read_text().splitlines():
  try:e=json.loads(line)
  except json.JSONDecodeError:human.write(line+'\n');continue
  human.write(e.get('Output',''))
  if e.get('Action')=='fail':failed.append(dict(package=e.get('Package'),test=e.get('Test'),elapsed=e.get('Elapsed')))
(p/'failures.json').write_text(json.dumps(failed,indent=2)+'\n')
print('Failed tests/packages:',json.dumps(failed,ensure_ascii=False))
REPORT
report_status=$?
if [ "$status" -ne 0 ]; then exit "$status"; fi
exit "$report_status"
