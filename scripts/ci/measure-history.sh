#!/usr/bin/env bash
# Run only on a development machine or an isolated CI runner, never a panel VPS.
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
output="${1:-ci-artifacts}"
mkdir -p "$output"
go test ./web/connect -run '^$' -bench 'BenchmarkHistory(EntityPartition|ReadPartition)' -benchtime=200ms -count=3 -benchmem 2>&1 | tee "$output/history-benchmarks.log"
for mode in previous grouped; do
  KOMARI_HISTORY_PERF=1 KOMARI_HISTORY_PARTITION="$mode" go test ./web/connect -run '^TestHistoryConcurrentReadAndIngestionExperiment$' -count=1 -v 2>&1 | tee "$output/history-load-$mode.log"
done
