"""Generate exact RPC maps from Komari's production authorization policy."""
import hashlib
import json
import re
from pathlib import Path

root = Path(__file__).resolve().parent
backend = root.parents[1]
if not (backend / 'web/connect/interceptor.go').is_file():
    backend = backend / 'komari'
policy = backend / 'web/connect/interceptor.go'
text = policy.read_text(encoding='utf-8')
prefixes = dict(re.findall(r'(\w+)\s*=\s*"(/komari\.[^"]+/)"', text))
known = set()
machine = set()
calls = re.findall(r'add\(("[^"]+"|\w+),\s*rpc\.(RoleClient|RoleAdmin|RoleGuest),\s*[^,]+,\s*(.*?)\)', text, re.S)
for prefix, role, methods in calls:
    prefix = prefix.strip('"') if prefix.startswith('"') else prefixes[prefix]
    for method in re.findall(r'"([^"]+)"', methods):
        path = prefix + method
        known.add(path)
        if role == 'RoleClient':
            machine.add(path)
assert len(known) > 100 and len(machine) > 20
# Credential refresh validates its own signed refresh credential. TrustBundle
# contains public trust material needed to re-establish an enrolled connection.
machine.update('/komari.enrollment.v1.EnrollmentService/' + name for name in ['RefreshCredentials', 'GetTrustBundle'])
assert machine <= known
lines = ['# Generated from the backend procedure policy. Exact matches only.', 'map $uri $komari_machine_route {', '    default 0;']
lines += [f'    "{path}" 1;' for path in sorted(machine)]
lines += ['    ~^/api/clients/transfer/[A-Za-z0-9_-]+$ 1;', '}', '', 'map $uri $komari_route_class {', '    default web;']
lines += [f'    "{path}" rpc;' for path in sorted(known)]
lines += ['    ~^/api/clients/transfer/[A-Za-z0-9_-]+$ rpc;', '}', '']
(root / 'nginx/rpc-map.conf').write_text('\n'.join(lines), encoding='utf-8')
(root / 'manifest.json').write_text(json.dumps({'policy_sha256': hashlib.sha256(text.encode('utf-8')).hexdigest(), 'registered_procedures': len(known), 'machine_procedures': len(machine), 'machine_routes': sorted(machine)}, indent=2) + '\n', encoding='utf-8')
print(f'Generated {len(known)} exact procedures and {len(machine)} machine procedures')
