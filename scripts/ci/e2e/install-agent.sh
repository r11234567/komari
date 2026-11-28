#!/usr/bin/env bash
source "$(dirname "$0")/guard.sh"
umask 077
runner_user=${KOMARI_E2E_USER:-$(id -un)}
sudo install -d /usr/lib/komari/komari-e2e
sudo install -m0755 "$E2E_ROOT/bin/komari-agent" /usr/lib/komari/komari-e2e/komari-agent
sudo install -m0755 "$E2E_ROOT/bin/komari-host" /usr/lib/komari/komari-e2e/komari-host
sudo /usr/lib/komari/komari-e2e/komari-host install \
  --service komari-e2e --user "$runner_user" --binary-dir /usr/lib/komari/komari-e2e \
  -- --endpoint "$E2E_URL" --disable-auto-update
sudo install -d /etc/systemd/system/komari-e2e.service.d
# The real probe fixture targets the panel's loopback listener. Apply the
# documented per-node allowance to the test Agent service alone.
printf '[Service]\nEnvironment=KOMARI_PING_PROBE_ALLOW_IPS=127.0.0.1\n' |
  sudo tee /etc/systemd/system/komari-e2e.service.d/probe-fixture.conf >/dev/null
if [[ "${KOMARI_E2E_LOCAL:-}" = 1 ]]; then
  printf '[Service]\nBindPaths=%s/agent-state:/var/lib/komari\n' "$RUNNER_TEMP" > /etc/systemd/system/komari-e2e.service.d/local-fixture.conf
fi
sudo systemctl daemon-reload
printf 'editor fixture\n' > "$E2E_ROOT/files/editor.txt"
python3 - "$E2E_ROOT/files/large.log" <<'LARGE_LOG'
import pathlib
import sys

first_page = b"PAGE-ONE\n".ljust(128 * 1024, b" ")
records = b"synthetic service log record for byte range testing\n" * 43_000
pathlib.Path(sys.argv[1]).write_bytes(first_page + b"PAGE-TWO\n" + records + b"TAIL-BEFORE\n")
LARGE_LOG
printf 'destination must survive\n' > "$E2E_ROOT/files/existing.txt"
mkdir -p "$E2E_ROOT/files/nonempty"
printf 'keep\n' > "$E2E_ROOT/files/nonempty/keep.txt"
if [[ "${KOMARI_E2E_LOCAL:-}" = 1 ]]; then
  chown -R "$runner_user:$(id -gn "$runner_user")" "$E2E_ROOT/files"
fi
