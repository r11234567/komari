"""Fail if a production package includes the retired reference engine."""
import json
import sys
from pathlib import Path

package = json.loads(Path(sys.argv[1]).read_text())
files = package['GoFiles']
assert 'legacy_disabled.go' in files, files
assert not any(name.endswith('_legacy_fixture.go') or name == 'legacy_fixture_config.go' for name in files), files
print('Production metric package excludes all retired compression entry points.')
