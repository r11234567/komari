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
if [[ "${KOMARI_E2E_LOCAL:-}" = 1 ]]; then
  mkdir -p /etc/systemd/system/komari-e2e.service.d
  printf '[Service]\nBindPaths=%s/agent-state:/var/lib/komari\n' "$RUNNER_TEMP" > /etc/systemd/system/komari-e2e.service.d/local-fixture.conf
  systemctl daemon-reload
fi
printf 'editor fixture\n' > "$E2E_ROOT/files/editor.txt"
printf 'destination must survive\n' > "$E2E_ROOT/files/existing.txt"
mkdir -p "$E2E_ROOT/files/nonempty"
printf 'keep\n' > "$E2E_ROOT/files/nonempty/keep.txt"
if [[ "${KOMARI_E2E_LOCAL:-}" = 1 ]]; then
  chown -R "$runner_user:$(id -gn "$runner_user")" "$E2E_ROOT/files"
fi
