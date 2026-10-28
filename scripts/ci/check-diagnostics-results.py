#!/usr/bin/env python3
"""Require real, non-skipped test evidence and bounded measured overhead."""
import json
import re
import statistics
import sys
from pathlib import Path

root = Path(sys.argv[1])
expected = {
    'internal/diagnostics': {
        'TestDiagnosticsMeasuredSlowHTTPAndPrivacy',
        'TestDiagnosticsInFlightAndClientCancellation',
        'TestDiagnosticsConnectStreamErrorAndFlush',
        'TestDiagnosticsLiveStreamDoesNotBuffer',
        'TestDiagnosticsPreservesWebSocketHijacking',
        'TestDiagnosticsServerDeadlineIsNotClientCancellation',
        'TestDiagnosticsUnknownURLsAndRecentHistoryAreBounded',
        'TestDiagnosticsConcurrentSnapshotsAndAccounting',
        'TestDiagnosticsRealSQLitePoolWaitAndQueryCancellation',
        'TestDiagnosticsQueryLoggerDoesNotRenderOrChangeLogMode',
    },
    'web/connect': {
        'TestDiagnosticsRealConnectAuthenticationAndRecovery',
        'TestDiagnosticsAdminEndpointAuthorizationAndNoSQL',
    },
    'database/metricstore': {'TestDiagnosticsReportQueueReflectsRealEnqueueAndOverflow'},
    'web/workspace': {'TestDiagnosticsWorkspaceSnapshotNeverValidatesOrWaits'},
    'internal/trustedcredential': {'TestDiagnosticsVerificationCountsAndBusySnapshot'},
}
passed, rejected = set(), []
for path in sorted(root.glob('diagnostics-*-go/tests.jsonl')):
    for line in path.read_text().splitlines():
        event = json.loads(line)
        package = event.get('Package', '').removeprefix('github.com/komari-monitor/komari/')
        test = event.get('Test', '')
        if package in expected and test in expected[package]:
            if event.get('Action') == 'pass':
                passed.add((package, test))
            if event.get('Action') in ('skip', 'fail'):
                rejected.append(event)
        elif event.get('Action') == 'fail':
            rejected.append(event)
missing = sorted((p, t) for p, names in expected.items() for t in names if (p, t) not in passed)
if missing or rejected:
    raise SystemExit(json.dumps({'missing_tests': missing, 'failed_or_skipped': rejected}, indent=2))

samples = {'false': [], 'true': []}
text = (root / 'diagnostics-benchmark.log').read_text()
pattern = r'BenchmarkDiagnosticsHTTP/(false|true)(?:-\d+)?\s+\d+\s+([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op'
for mode, ns, size, allocs in re.findall(pattern, text):
    samples[mode].append((float(ns), int(size), int(allocs)))
if any(len(values) < 3 for values in samples.values()):
    raise SystemExit('Missing repeated benchmark measurements; no success without benchmark evidence')
medians = {mode: [statistics.median(row[i] for row in values) for i in range(3)] for mode, values in samples.items()}
delta = [medians['true'][i] - medians['false'][i] for i in range(3)]
result = {'passed_required_tests': len(passed), 'benchmark_samples': samples,
          'median_ns_bytes_allocs': medians, 'instrumentation_delta': delta,
          'limits': {'added_ns': 25000, 'added_bytes': 2048, 'added_allocations': 12}}
(root / 'diagnostics-evidence.json').write_text(json.dumps(result, indent=2) + '\n')
print(json.dumps(result, indent=2))
if delta[0] > 25000 or delta[1] > 2048 or delta[2] > 12:
    raise SystemExit('Instrumentation exceeds the fixed per-request budget; inspect benchmark artifact')
