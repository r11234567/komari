"""Check cscli explain --verbose output against synthetic routing fixtures."""
import re
import sys
from pathlib import Path

report = Path(sys.argv[1]).read_text(encoding='utf-8-sig')
blocks = re.split(r'(?m)^line: ', report)[1:]
assert len(blocks) == 7, f'Expected seven fixtures, got {len(blocks)}'
for block in blocks:
    first_line = block.splitlines()[0]
    assert 'parser failure' not in block, first_line
    status = int(re.search(r'HTTP/[\d.]+" (\d+)', first_line)[1])
    rpc = '"rpc"' in first_line
    authenticated = '"user"' in first_line
    expected_whitelist = rpc or status == 429 or authenticated
    assert ('[whitelisted]' in block) == expected_whitelist, first_line
    if not expected_whitelist:
        assert 'Scenarios' in block, first_line
    print('PASS', first_line.split('"')[1], status, 'whitelist' if expected_whitelist else 'scenario')
