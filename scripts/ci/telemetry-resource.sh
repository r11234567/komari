#!/usr/bin/env bash
set -euo pipefail

# Opt-in Linux/systemd resource acceptance. Source is a completed disposable
# migration fixture containing metrics.db and archive/. It is opened read-only;
# the test creates its own SQL copy and links immutable archives into that copy.
# Build outside the quota; execute under actual CPU/memory cgroup limits.
if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo 'usage: telemetry-resource.sh /absolute/fixture /absolute/report.json [duration=15m]' >&2
  exit 2
fi
fixture=$(realpath -e -- "$1")
report=$(realpath -m -- "$2")
duration=${3:-15m}
[[ -f "$fixture/metrics.db" && -d "$fixture/archive" ]]
[[ ! -e "$report" ]] || { echo 'report already exists; choose a new output path' >&2; exit 2; }
build=$(mktemp -d /var/tmp/komari-telemetry-resource.XXXXXX)
trap 'rm -f -- "$build/trafficledger.test"; rmdir -- "$build"' EXIT
go test -c ./database/trafficledger -o "$build/trafficledger.test"
unit="komari-telemetry-resource-${RANDOM}-${RANDOM}"
systemd-run --unit="$unit" --wait --pipe --collect \
  --property=CPUQuota=100% --property=MemoryMax=1G --property=MemorySwapMax=0 \
  --property=CPUAccounting=yes --property=MemoryAccounting=yes \
  --setenv=GOMAXPROCS=1 --setenv=GOMEMLIMIT=650MiB \
  --setenv="KOMARI_TELEMETRY_RESOURCE_SOURCE=$fixture" \
  --setenv="KOMARI_TELEMETRY_RESOURCE_OUTPUT=$report" \
  --setenv="KOMARI_TELEMETRY_RESOURCE_DURATION=$duration" \
  "$build/trafficledger.test" -test.run='^TestTelemetryResourceAcceptance$' \
  -test.count=1 -test.timeout=45m -test.v
