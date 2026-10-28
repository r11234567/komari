#!/usr/bin/env python3
"""Require real, non-skipped signed-stream regressions from the pinned Agent."""
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
expected = {
    'TestRescueHeartbeatRejectsReplayDelayRegressionAndTampering',
    'TestRescueReceivesRealSignedStreamAndReportsDisconnect',
    'TestIdleManagedHelperCannotLeaseOrExecute',
}
passed, rejected = set(), []
for line in (root / 'tests.jsonl').read_text().splitlines():
    event = json.loads(line)
    if event.get('Action') == 'fail':
        rejected.append(event)
    if event.get('Package') != 'github.com/komari-monitor/komari-agent/rescue':
        continue
    if event.get('Test') not in expected:
        continue
    if event.get('Action') == 'pass':
        passed.add(event['Test'])
    elif event.get('Action') == 'skip':
        rejected.append(event)
result = {'required_passed': sorted(passed), 'missing': sorted(expected - passed),
          'failed_or_skipped': rejected}
(root / 'required-evidence.json').write_text(json.dumps(result, indent=2) + '\n')
print(json.dumps(result, indent=2))
if result['missing'] or rejected:
    raise SystemExit('Pinned Agent signed-stream evidence is missing, skipped or failed')
