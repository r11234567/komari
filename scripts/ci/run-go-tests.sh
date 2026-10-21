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
go test -json -count=1 -timeout=12m -p=2 "$@" 2> >(tee "$log_dir/build-stderr.log" >&2) | tee "$log_dir/tests.jsonl"
status=${PIPESTATUS[0]}
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
