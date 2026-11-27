from pathlib import Path
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


root = Path(sys.argv[1])
with start(root, sys.argv[2], 'incorrect-synthetic-password') as rejected:
    rejected.expect('fresh PAM authentication or account validation failed')
    rejected.expect(pexpect.EOF)
if rejected.exitstatus != 1:
    raise RuntimeError(f'invalid PAM unexpectedly returned {rejected.exitstatus}')
with start(root, sys.argv[2], (root / 'root-password').read_text().strip()) as accepted:
    accepted.expect(pexpect.EOF, timeout=55)
if accepted.exitstatus != 0:
    raise RuntimeError(f'root authorization exited {accepted.exitstatus}: {accepted.before.strip()}')
