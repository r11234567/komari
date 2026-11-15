#!/usr/bin/env bash
source "$(dirname "$0")/guard.sh"
mkdir -p "$E2E_ARTIFACTS"
for pair in "${E2E_EXPECTED_SERVER_SHA:-}:./" "${E2E_EXPECTED_AGENT_SHA:-}:e2e-agent"; do
  expected=${pair%%:*}
  directory=${pair#*:}
  if [[ -n "$expected" ]]; then
    [[ "$expected" =~ ^[0-9a-f]{40}$ ]]
    test "$(git -C "$directory" rev-parse HEAD)" = "$expected"
  fi
done
backend_proto=$(git -C third_party/komari-proto rev-parse HEAD)
for tree in third_party/komari-web e2e-agent; do
  test "$(git -C "$tree/third_party/komari-proto" rev-parse HEAD)" = "$backend_proto"
done
{
  git rev-parse HEAD
  git submodule status --recursive
  git -C e2e-agent rev-parse HEAD
  git -C e2e-agent submodule status --recursive
  go version
} > "$E2E_ARTIFACTS/revisions.txt"
cat "$E2E_ARTIFACTS/revisions.txt"
node scripts/ci/check-protocol.cjs --resolve-go
node e2e-agent/scripts/ci/check-protocol.cjs --resolve-go
