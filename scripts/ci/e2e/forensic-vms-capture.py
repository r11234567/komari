import json
from pathlib import Path
import socket
import sys

import pexpect


def start(root, command, password):
    arguments = [
        '-tt', '-p', '29482', '-i', str(root / 'client'),
        '-o', f'UserKnownHostsFile={root / "known_hosts"}',
        '-o', 'StrictHostKeyChecking=yes', '-o', 'BatchMode=yes',
        '-o', 'LogLevel=ERROR', 'root@127.0.0.1',
        'loginctl --no-pager show-session self -p Remote -p RemoteHost -p Service -p Type -p TTY -p User && exec ' + command,
    ]
    session = pexpect.spawn('ssh', arguments, encoding='utf-8', timeout=15, echo=False)
    try:
        state = session.expect(['(?i)password:', pexpect.EOF])
        if state == 1:
            raise RuntimeError(f'root SSH rejected before PAM: {session.before.strip()}')
        session.sendline(password)
        return session
    except BaseException:
        session.close(force=True)
        raise


def observe_fixture(root):
    with socket.socket(socket.AF_UNIX) as channel:
        channel.settimeout(10)
        channel.connect(str(root / 'qga.sock'))
        with channel.makefile('rb') as returned:
            def qga(command, arguments=None, sync=False):
                request = {'execute': command}
                if arguments is not None:
                    request['arguments'] = arguments
                channel.sendall((b'\xff' if sync else b'') + json.dumps(request).encode() + b'\n')
                if sync:
                    for _ in range(16):
                        line = returned.readline()
                        if line.startswith(b'\xff'):
                            data = json.loads(line.lstrip(b'\xff'))
                            if data.get('return') == arguments['id']:
                                return data['return']
                    raise RuntimeError('QGA never acknowledged the fixture synchronization marker')
                data = json.loads(returned.readline())
                if 'return' not in data:
                    raise RuntimeError(f'QGA fixture failed at {command}')
                return data['return']

            assert qga('guest-sync-delimited', {'id': 91427}, sync=True) == 91427
            qga('guest-ping')
            handle = qga('guest-file-open', {'path': '/root/requested', 'mode': 'r'})
            assert qga('guest-file-read', {'handle': handle, 'count': 4096})['count'] == (root / 'requested').stat().st_size
            qga('guest-file-close', {'handle': handle})


root = Path(sys.argv[1])
with start(root, sys.argv[2], 'incorrect-synthetic-password') as rejected:
    rejected.expect('fresh PAM authentication or account validation failed')
    rejected.expect(pexpect.EOF)
if len(sys.argv) == 4 and sys.argv[3] == 'root-approval':
    if rejected.exitstatus != 1:
        raise RuntimeError(f'invalid PAM unexpectedly returned {rejected.exitstatus}')
    with start(root, sys.argv[2], (root / 'root-password').read_text().strip()) as accepted:
        accepted.expect(pexpect.EOF, timeout=55)
    if accepted.exitstatus != 0:
        raise RuntimeError(f'root authorization exited {accepted.exitstatus}: {accepted.before.strip()}')
    sys.exit(0)
with start(root, sys.argv[2], (root / 'root-password').read_text().strip()) as accepted:
    state = accepted.expect(['QGA forensic capture active in this SSH session', pexpect.EOF])
    if state == 1:
        raise RuntimeError(accepted.before.strip())
    observe_fixture(root)
    state = accepted.expect([r'forensic upload sequence \d+ failed \([^)]+\)', 'invalid diagnostic receipt', pexpect.EOF], timeout=50)
    if state != 2:
        raise RuntimeError(accepted.after)
accepted.close()
if accepted.exitstatus != 0:
    raise RuntimeError(f'forensic process exited {accepted.exitstatus}: {accepted.before.strip()}')
