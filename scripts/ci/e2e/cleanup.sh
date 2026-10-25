#!/usr/bin/env bash
source "$(dirname "$0")/guard.sh"
mkdir -p "$E2E_ARTIFACTS"
# Collection must not prevent the remaining cleanup after an earlier failure.
set +e
sudo journalctl -u komari-e2e.service --no-pager -o short-iso > "$E2E_ARTIFACTS/agent.log" 2>&1
sudo systemctl status komari-e2e.service --no-pager > "$E2E_ARTIFACTS/agent-status.txt" 2>&1
sudo systemctl stop komari-e2e.service
sudo systemctl disable komari-e2e.service
for name in login server; do
  if [[ -f "$E2E_ROOT/$name.pid" ]]; then kill "$(cat "$E2E_ROOT/$name.pid")" 2>/dev/null; fi
done
if [[ -f "$E2E_ROOT/nginx.pid" ]]; then
  sudo nginx -s quit -c "$E2E_ROOT/nginx.conf" -p "$E2E_ROOT/"
fi
certutil -D -n komari-e2e -d "sql:$HOME/.pki/nssdb"
sudo rm -f /usr/local/share/ca-certificates/komari-e2e.crt /etc/systemd/system/komari-e2e.service
sudo update-ca-certificates
sudo systemctl daemon-reload
sudo rm -rf /usr/lib/komari/komari-e2e /var/lib/komari/komari-e2e
sudo rm -f /etc/komari/services/komari-e2e.json /etc/komari/services/komari-e2e.policy.json /etc/komari/services/komari-e2e.trust.json
sudo rm -f /etc/komari/services/komari-e2e.applied.json
# No databases, credentials, state files, TLS private keys or signing keys are
# included in artifacts. Browser evidence contains disposable CI session data.
sudo rm -rf "$E2E_ROOT"
exit 0
