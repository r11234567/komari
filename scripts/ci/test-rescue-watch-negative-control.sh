#!/usr/bin/env bash
# CI only: remove the exact route to expose the deliberately short default.
set -euo pipefail
[[ "${GITHUB_ACTIONS:-}" == true ]] || exit 2
original=$(mktemp)
cp deploy/nginx/workbench-locations.conf "$original"
trap 'cp "$original" deploy/nginx/workbench-locations.conf; rm -f "$original"' EXIT
python3 - <<'PY'
from pathlib import Path
path = Path('deploy/nginx/workbench-locations.conf')
text = path.read_text()
old = 'location = /komari.config.v1.PrivilegedDeliveryService/WatchPrivilegedDelivery {'
assert text.count(old) == 1, 'Nginx watch route changed; review negative control'
path.write_text(text.replace(old, 'location = /negative-control-unused-watch {'))
PY
set +e
bash scripts/ci/run-go-tests.sh -race -run '^TestRescueWatchNginxSurvivesThirtySecondsAndRevokes$' ./web/connect
status=$?
set -e
python3 - "$KOMARI_TEST_LOG_DIR" "$status" <<'PY'
import json, sys
from pathlib import Path
root = Path(sys.argv[1])
events = [json.loads(line) for line in (root / 'tests.jsonl').read_text().splitlines()]
name = 'TestRescueWatchNginxSurvivesThirtySecondsAndRevokes'
failed = [e for e in events if e.get('Test') == name and e.get('Action') == 'fail']
output = ''.join(e.get('Output', '') for e in events if e.get('Test') == name)
assert int(sys.argv[2]) != 0 and failed, 'Missing route did not fail the runtime test (build failure is insufficient)'
assert 'Nginx stream closed at' in output and 'upstream timed out' in output, output
print('Negative control detected the inherited proxy read timeout; complete Nginx and Go failure logs retained.')
PY
