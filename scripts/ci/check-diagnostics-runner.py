#!/usr/bin/env python3
"""Negative controls: prove CI rejects failures and retains untruncated logs."""
import json
import os
import subprocess
import tempfile
from pathlib import Path

if os.environ.get('GITHUB_ACTIONS') != 'true':
    raise SystemExit('Runner contract checks run in GitHub Actions only')
root = Path(__file__).resolve().parents[2]
artifacts = root / 'ci-artifacts' / 'diagnostics-negative-controls'
artifacts.mkdir(parents=True, exist_ok=True)
env = dict(os.environ, KOMARI_AUDIT_LOG_DIR=str(artifacts.relative_to(root)))
audit = root / 'scripts/ci/run-audit-check.sh'
command = ['python3', '-c', 'import sys;print("STDOUT-FIRST");[print("fixture-line-%d"%i) for i in range(250)];print("STDERR-LAST",file=sys.stderr);sys.exit(23)']
first = subprocess.run(['bash', str(audit), 'expected-command-failure', *command], cwd=root, env=env)
if first.returncode != 23:
    raise SystemExit(f'Command failure was swallowed: {first.returncode}')
log = (artifacts / 'expected-command-failure.log').read_text()
if not all(part in log for part in ('STDOUT-FIRST', 'fixture-line-0\n', 'fixture-line-249\n', 'STDERR-LAST')):
    raise SystemExit('Full stdout/stderr was not retained')
if (artifacts / 'expected-command-failure-exit-code.txt').read_text().strip() != '23':
    raise SystemExit('Original command exit code was not retained')

# A genuine failing Go assertion, not merely a malformed go-test command.
# Only this isolated synthetic package is passed to the real test runner.
with tempfile.TemporaryDirectory(prefix='diagnostics-negative-', dir=root / 'scripts/ci') as directory:
    fixture = Path(directory)
    (fixture / 'failure_test.go').write_text('package fixture\nimport "testing"\nfunc TestSyntheticFailure(t *testing.T) { t.Log("ASSERTION-FIRST"); t.Fatal("ASSERTION-LAST") }\n')
    package = './' + str(fixture.relative_to(root))
    second = subprocess.run(['bash', str(audit), 'expected-test-failure', 'bash', str(root / 'scripts/ci/run-go-tests.sh'), '-run', '^TestSyntheticFailure$', package], cwd=root, env=env)
if second.returncode == 0:
    raise SystemExit('A genuinely failing Go test was reported as successful')
go_logs = artifacts / 'expected-test-failure-go'
events = [json.loads(line) for line in (go_logs / 'tests.jsonl').read_text().splitlines()]
if not any(e.get('Action') == 'fail' and e.get('Test') == 'TestSyntheticFailure' for e in events):
    raise SystemExit('No real failed assertion found in test JSON')
if not all(marker in (go_logs / 'tests.log').read_text() for marker in ('ASSERTION-FIRST', 'ASSERTION-LAST')):
    raise SystemExit('Assertion output was truncated')
for filename in ('build-stderr.log', 'exit-code.txt', 'context.log', 'failures.json'):
    if not (go_logs / filename).is_file():
        raise SystemExit(f'Missing failure artifact {filename}')
result = {'command_exit_code': first.returncode, 'failing_test_exit_code': second.returncode,
          'complete_stdout_stderr': True, 'genuine_assertion_failed': True}
(artifacts / 'runner-contract.json').write_text(json.dumps(result, indent=2) + '\n')
print(json.dumps(result))
