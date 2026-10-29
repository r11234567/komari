#!/usr/bin/env bash
set -euo pipefail
[[ "${GITHUB_ACTIONS:-}" == true ]] || { echo 'CI only' >&2; exit 2; }
backup=$(mktemp)
cp database/enrollment/rotation.go "$backup"
trap 'cp "$backup" database/enrollment/rotation.go; rm -f "$backup"' EXIT
python3 - <<'PY'
from pathlib import Path
p = Path('database/enrollment/rotation.go')
s = p.read_text()
needle = 'now.Before(*cur.RefreshRecoveryUntil)'
assert s.count(needle) == 1
p.write_text(s.replace(needle, '(false && now.Before(*cur.RefreshRecoveryUntil))'))
PY
set +e
bash scripts/ci/run-audit-check.sh credential-broken-recovery bash scripts/ci/run-go-tests.sh -race -run '^TestCredentialRotationRecoversAfterDatabaseReopen$' ./database/enrollment
status=$?
set -e
[[ "$status" -ne 0 ]] || { echo 'Broken recovery unexpectedly passed'; exit 1; }
python3 - <<'PY'
import json
from pathlib import Path
p=Path('ci-artifacts/credential-broken-recovery-go/tests.jsonl')
events=[json.loads(line) for line in p.read_text().splitlines() if line.strip()]
assert any(e.get('Action')=='fail' and e.get('Test')=='TestCredentialRotationRecoversAfterDatabaseReopen' for e in events), 'Compile errors do not count as a negative control'
assert 'consumed refresh credential reused' in ''.join(e.get('Output','') for e in events)
print('Broken recovery produced the expected assertion failure; complete logs retained.')
PY
