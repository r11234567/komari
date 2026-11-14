#!/usr/bin/env python3
"""Finite archive decoder experiment; each case gets a fresh cgroup/process."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise SystemExit('Run with sudo: this experiment requires systemd cgroup controls.')
    binary = args.binary.resolve(strict=True)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='komari-archive-independent-') as temporary:
        root = Path(temporary)
        env = dict(os.environ, GOMAXPROCS='1', GOMEMLIMIT='96MiB',
                   KOMARI_TELEMETRY_ARCHIVE_PREPARE=str(root))
        prepared = subprocess.run([str(binary), '-test.run', '^TestTelemetryIndependentArchivePrepare$',
                                   '-test.timeout', '5m', '-test.v'], env=env, text=True,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=330)
        (output / 'prepare.log').write_text(prepared.stdout)
        prepared.check_returncode()
        reports = []
        for case in json.loads((root / 'cases.json').read_text()):
            name = case['Name']
            unit = 'komari-archive-independent-' + uuid.uuid4().hex[:12]
            cmd = ['systemd-run', '--unit=' + unit, '--wait', '--pipe', '--collect',
                   '--property=CPUQuota=10%', '--property=CPUQuotaPeriodSec=100ms',
                   '--property=MemoryMax=128M', '--property=MemorySwapMax=0', '--property=TasksMax=32',
                   '/usr/bin/env', 'GOMAXPROCS=1', 'GOMEMLIMIT=96MiB',
                   'KOMARI_TELEMETRY_ARCHIVE_READ=' + str(root),
                   'KOMARI_TELEMETRY_ARCHIVE_CASE=' + name, str(binary),
                   '-test.run', '^TestTelemetryIndependentArchiveRead$', '-test.timeout', '5m', '-test.v']
            try:
                measured = subprocess.run(cmd, text=True, stdout=subprocess.PIPE,
                                          stderr=subprocess.STDOUT, timeout=330)
                (output / (name + '.log')).write_text(measured.stdout)
                measured.check_returncode()
            finally:
                # Only this unique experimental unit; never global services.
                subprocess.run(['systemctl', 'stop', unit], check=False, capture_output=True)
            report = json.loads((root / (name + '-result.json')).read_text())
            assert report['cpu.max'] == '10000 100000', report
            assert report['memory.max'] == str(128 * 1024**2), report
            assert report['memory.swap.max'] == '0', report
            assert report['process_maxrss_kib'] * 1024 < 128 * 1024**2, report
            assert report['decoded_records'] == report['points'] and report['queries'] == 2, report
            report['fresh_process'] = True
            report['page_cache'] = 'warm/unspecified; not a cold disk benchmark'
            reports.append(report)
            print(json.dumps(report), flush=True)
        (output / 'archive-resource-results.json').write_text(json.dumps({
            'synthetic': True, 'preparation_measured_separately': True,
            'measurement': 'production VisitExportInputProgress; decode, scope, checksum, streaming callback',
            'cases': reports,
        }, indent=2) + '\n')


if __name__ == '__main__':
    main()
