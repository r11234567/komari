#!/usr/bin/env bash
# Explicit local debugging, using fresh state and fixed loopback-only ports.
# Stages preserve state on failure; cleanup is a separate explicit action.
set -euo pipefail
if [[ $(uname -s) != Linux || $(id -u) != 0 ]]; then
  echo 'Run in a local Linux/WSL root shell (the installed test Agent is nonroot).' >&2
  exit 2
fi
export KOMARI_E2E_LOCAL=1
export RUNNER_TEMP=/var/tmp/komari-local-e2e-0
export E2E_ROOT=$RUNNER_TEMP/komari-e2e
export E2E_ARTIFACTS=$RUNNER_TEMP/artifacts
export E2E_TOOLS=$RUNNER_TEMP/tools
export KOMARI_E2E_USER=komari-e2e-debug
export GOMAXPROCS=2
export GOMEMLIMIT=3GiB
export NODE_PATH=$E2E_TOOLS/node_modules
export NODE_EXTRA_CA_CERTS=$E2E_ROOT/tls/ca.crt
export PATH=$RUNNER_TEMP/node-v24.11.0-linux-x64/bin:/usr/bin:/usr/sbin:/bin:/sbin
source_dir=$(cd "$(dirname "$0")/../../.." && pwd)
export GITHUB_WORKSPACE=$RUNNER_TEMP/repo
stage=${1:-help}
if [[ $stage == prepare ]]; then
  [[ ! -e $RUNNER_TEMP ]] || { echo "Fixture already exists: $RUNNER_TEMP" >&2; exit 2; }
  for unit in komari-e2e.service komari-e2e-panel.service komari-e2e-export.service; do
    [[ $(systemctl show "$unit" --property=LoadState --value) == not-found ]] || { echo "Existing unit: $unit" >&2; exit 2; }
  done
  ! ss -lntH | awk '{print $4}' | grep -Eq ':(29443|29444)$' || { echo 'Fixture port occupied' >&2; exit 2; }
  mkdir -m 755 "$RUNNER_TEMP"
  touch "$RUNNER_TEMP/.local-fixture"
  id "$KOMARI_E2E_USER" >/dev/null 2>&1 || useradd --system --create-home --shell /bin/bash "$KOMARI_E2E_USER"
  git clone --local --no-hardlinks "$source_dir" "$GITHUB_WORKSPACE"
  for sub in third_party/komari-proto third_party/komari-web third_party/komari-web/third_party/komari-proto; do
    git clone --local --no-hardlinks "$source_dir/$sub" "$GITHUB_WORKSPACE/$sub"
  done
  git clone --local --no-hardlinks "$source_dir/../komari-agent" "$GITHUB_WORKSPACE/e2e-agent"
  git clone --local --no-hardlinks "$source_dir/../komari-agent/third_party/komari-proto" "$GITHUB_WORKSPACE/e2e-agent/third_party/komari-proto"
  rsync -a "$source_dir/scripts/ci/" "$GITHUB_WORKSPACE/scripts/ci/"
  mkdir -p "$E2E_ROOT/bin" "$E2E_ARTIFACTS" "$E2E_TOOLS"
  chown -R "$KOMARI_E2E_USER:$(id -gn "$KOMARI_E2E_USER")" "$E2E_ROOT"
  curl --fail --location https://nodejs.org/dist/v24.11.0/node-v24.11.0-linux-x64.tar.xz -o "$RUNNER_TEMP/node.tar.xz"
  tar -xJf "$RUNNER_TEMP/node.tar.xz" -C "$RUNNER_TEMP"
  npm install --prefix "$E2E_TOOLS" --no-audit --no-fund --no-package-lock @playwright/test@1.56.1
  "$E2E_TOOLS/node_modules/.bin/playwright" install --with-deps chromium
  exit 0
fi
source "$source_dir/scripts/ci/e2e/guard.sh"
if [[ ${KOMARI_E2E_MOUNTED:-} != 1 ]]; then
  export KOMARI_E2E_MOUNTED=1
  exec unshare --mount --propagation private bash "$0" "$@"
fi
# The installed host tool requires a root-owned state parent. Bind a new
# private parent in this command's mount namespace; never chown an existing
# operator installation. The test Agent gets the same bind through its unit.
mkdir -p "$RUNNER_TEMP/agent-state" /var/lib/komari
mount --bind "$RUNNER_TEMP/agent-state" /var/lib/komari
cd "$GITHUB_WORKSPACE"
case $stage in
  sync) rsync -a "$source_dir/scripts/ci/" scripts/ci/; rsync -a "$source_dir/pkg/historyexport/" pkg/historyexport/ ;;
  build)
    (cd third_party/komari-web; npm ci --no-audit; npm run build:system)
    cp -a third_party/komari-web/dist/. web/public/systemUI/dist/
    (cd third_party/komari-web; npm run build)
    cp -a third_party/komari-web/dist/. web/public/bundledThemes/default/dist/
    bash scripts/ci/e2e/build.sh
    chown -R "$KOMARI_E2E_USER:$(id -gn "$KOMARI_E2E_USER")" "$E2E_ROOT"
    ;;
  setup) bash scripts/ci/e2e/setup.sh ;;
  agent) bash scripts/ci/e2e/install-agent.sh ;;
  backend)
    go build -trimpath -o "$E2E_ROOT/bin/komari-next" .
    mv "$E2E_ROOT/bin/komari-next" "$E2E_ROOT/bin/komari"
    ;;
  browser|resume)
    if [[ $stage == resume ]]; then export KOMARI_E2E_RESUME=1; fi
    "$E2E_TOOLS/node_modules/.bin/playwright" test --config scripts/ci/e2e/playwright.config.cjs
    ;;
  upgrade)
    go run ./scripts/ci/migration-fixture
    chown -R "$KOMARI_E2E_USER:$(id -gn "$KOMARI_E2E_USER")" "$E2E_ROOT/migration-panel"
    node scripts/ci/e2e/storage-upgrade.cjs
    ;;
  qualify-prepare)
    [[ ! -e $E2E_ROOT/qualification-manifest.json ]] || { echo 'Cold fixture already exists' >&2; exit 2; }
    systemctl stop komari-e2e-export.service komari-e2e-panel.service
    go run ./scripts/ci/export-fixture
    chown -R "$KOMARI_E2E_USER:$(id -gn "$KOMARI_E2E_USER")" "$E2E_ROOT/panel/data"
    cp "$E2E_ROOT/qualification-manifest.json" "$E2E_ARTIFACTS/qualification-manifest.json"
    systemd-run --unit=komari-e2e-panel --collect --slice=komari-e2e.slice \
      --property="User=$KOMARI_E2E_USER" --property="WorkingDirectory=$E2E_ROOT/panel" \
      --property=CPUQuota=100% --property=MemoryMax=1G \
      "$E2E_ROOT/bin/komari" server --listen 127.0.0.1:29444
    systemd-run --unit=komari-e2e-export --collect --slice=komari-e2e.slice \
      --property="User=$KOMARI_E2E_USER" --property="WorkingDirectory=$E2E_ROOT/panel" \
      --property=CPUQuota=4% --property=CPUQuotaPeriodSec=100ms \
      --property=MemoryMax=128M --property=MemorySwapMax=0 \
      --setenv=GOMEMLIMIT=90MiB --setenv=GOMAXPROCS=1 \
      "$E2E_ROOT/bin/komari" export-worker --queue "$E2E_ROOT/panel/data/exports-v2" \
      --io-bytes-per-second=131072 \
      --instance-cgroup="/sys/fs/cgroup$(systemctl show komari-e2e-panel.service --property=ControlGroup --value)"
    ;;
  qualify) node scripts/ci/e2e/export-qualification.cjs ;;
  cleanup) bash scripts/ci/e2e/cleanup.sh ;;
  *) echo 'Stages: prepare, sync, build, backend, setup, agent, browser, resume, upgrade, qualify-prepare, qualify, cleanup'; exit 2 ;;
esac
