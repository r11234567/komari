#!/usr/bin/env bash
source "$(dirname "$0")/guard.sh"
# Disposable exact-binary deployment proof; no production Docker context.
docker info --format '{{.CgroupVersion}}' | grep -qx 2
export KOMARI_IMAGE="komari-export-proof:${GITHUB_RUN_ID}"
export KOMARI_DATA="$E2E_ROOT/docker-data"
export KOMARI_PANEL_CGROUP=/not-yet-resolved
export KOMARI_PORT=29577
export KOMARI_UID="$(id -u)"
export KOMARI_GID="$(id -g)"
project="komari-export-${GITHUB_RUN_ID}"
mkdir -p "$KOMARI_DATA" "$E2E_ROOT/docker-image"
cp "$E2E_ROOT/bin/komari" "$E2E_ROOT/docker-image/komari"
cat > "$E2E_ROOT/docker-image/Dockerfile" <<'EOF'
FROM ubuntu:24.04
WORKDIR /srv/komari
COPY komari /app/komari
ENTRYPOINT []
EOF
docker build -t "$KOMARI_IMAGE" "$E2E_ROOT/docker-image"
compose=(docker compose -p "$project" -f deploy/compose.export-worker.yml)
cleanup() {
  "${compose[@]}" logs --no-color > "$E2E_ARTIFACTS/export-docker.log" 2>&1 || true
  "${compose[@]}" down --remove-orphans || true
}
trap cleanup EXIT
"${compose[@]}" up -d panel
id=$("${compose[@]}" ps -q panel)
pid=$(docker inspect --format '{{.State.Pid}}' "$id")
export KOMARI_PANEL_CGROUP="/sys/fs/cgroup$(awk -F: '$1==0 {print $3}' "/proc/$pid/cgroup")"
test -f "$KOMARI_PANEL_CGROUP/cpu.max"
"${compose[@]}" up -d export-worker
for attempt in $(seq 1 60); do
  if [[ -s "$KOMARI_DATA/exports-v2/worker.json" ]]; then break; fi
  sleep 1
done
test -s "$KOMARI_DATA/exports-v2/worker.json"
for cpus in .5 1 2 4 8; do
  # Docker's --cpus rejects a quota above host CPU count. The kernel accepts
  # that quota; proof must still report the smaller actual cpuset capacity.
  quota=$(awk -v cpus="$cpus" 'BEGIN {printf "%.0f", cpus * 100000}')
  docker update --cpu-period=100000 --cpu-quota="$quota" "$id"
  sleep 3
  EXPECTED_CPUS="$cpus" python3 - <<'PY'
import json, os, pathlib
p = pathlib.Path(os.environ['KOMARI_DATA'])/'exports-v2/worker.json'
a = json.loads(p.read_text())
assert a['available'], a
proof = a['proof']
expected = float(os.environ['EXPECTED_CPUS'])
# cpuset/ancestors can reduce 4/8 cores on smaller hosted runners.
assert .5 <= proof['effective_cpus'] <= expected, proof
assert proof['worker_cpus'] <= .04 and proof['period_us'] <= 100000, proof
assert proof['memory_max'] <= 128*1024*1024, proof
assert proof['worker_cpus']/proof['effective_cpus'] <= .1, proof
out = pathlib.Path(os.environ['E2E_ARTIFACTS'])/'export-docker-resources.jsonl'
with out.open('a') as f: f.write(json.dumps(a)+'\n')
PY
done
docker update --cpus=.25 "$id"
sleep 5
python3 - <<'PY'
import json, os, pathlib
a=json.loads((pathlib.Path(os.environ['KOMARI_DATA'])/'exports-v2/worker.json').read_text())
assert not a['available'], a
PY
