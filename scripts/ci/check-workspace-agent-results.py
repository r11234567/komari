#!/usr/bin/env python3
"""Require real, non-skipped signed-stream and PTY-resumption regressions."""
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
expected = {
    'TestRescueHeartbeatRejectsReplayDelayRegressionAndTampering',
    'TestRescueReceivesRealSignedStreamAndReportsDisconnect',
    'TestIdleManagedHelperCannotLeaseOrExecute',
    'TestRescueAuthorizationWatchSurvivesThirtySeconds',
    'TestRescueAuthorizationWatchTransportBoundsAndRedirects',
    'TestRescueAuthorizationWatchContextCancelsStalledHeaders',
}
terminal_expected = {
    'TestActualPTYKeepsPIDAndWorkingDirectoryAcrossTransportLoss',
    'TestTerminalResumeReplaysUnconfirmedOutputAndPreservesCommandCursor',
    'TestTerminalResumeIsBoundedAndExpiryCancelsBlockedProducer',
    'TestTerminalResumeRejectsLostStateAndNeverRecreatesShell',
}
required = expected | terminal_expected
passed, rejected = set(), []
for line in (root / 'tests.jsonl').read_text().splitlines():
    event = json.loads(line)
    if event.get('Action') == 'fail':
        rejected.append(event)
    package_expected = {
        'github.com/komari-monitor/komari-agent/rescue': expected,
        'github.com/komari-monitor/komari-agent/clientcore': terminal_expected,
    }.get(event.get('Package'), set())
    if event.get('Test') not in package_expected:
        continue
    if event.get('Action') == 'pass':
        passed.add(event['Test'])
    elif event.get('Action') == 'skip':
        rejected.append(event)
result = {'required_passed': sorted(passed), 'missing': sorted(required - passed),
          'failed_or_skipped': rejected}
(root / 'required-evidence.json').write_text(json.dumps(result, indent=2) + '\n')
print(json.dumps(result, indent=2))
if result['missing'] or rejected:
    raise SystemExit('Pinned Agent signed-stream/PTY evidence is missing, skipped or failed')
