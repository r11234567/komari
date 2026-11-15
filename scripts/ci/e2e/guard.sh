#!/usr/bin/env bash
# Source this before *any* side effects, including cleanup. Never run on a VPS.
set -euo pipefail
if [[ "${KOMARI_E2E_LOCAL:-}" = 1 ]]; then
  local_base="/var/tmp/komari-local-e2e-$(id -u)"
  if [[ "$(uname -s)" != Linux || "${RUNNER_TEMP:-}" != "$local_base" || "${E2E_ROOT:-}" != "$local_base/komari-e2e" || "${E2E_ARTIFACTS:-}" != "$local_base/artifacts" || ! -f "$local_base/.local-fixture" || -L "$local_base" || -L "$E2E_ROOT" ]]; then
    echo 'Unexpected local E2E fixture paths.' >&2
    exit 2
  fi
  export E2E_URL=https://127.0.0.1:29443
  return 0 2>/dev/null || exit 0
fi
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "${RUNNER_OS:-}" != Linux ]]; then
  echo 'Real E2E is restricted to GitHub-hosted Linux runners.' >&2
  exit 2
fi
if [[ -z "${RUNNER_TEMP:-}" || "${E2E_ROOT:-}" != "$RUNNER_TEMP/komari-e2e" || "${E2E_ARTIFACTS:-}" != "$GITHUB_WORKSPACE/ci-artifacts/e2e" ]]; then
  echo 'Unexpected E2E paths; refusing to touch the host.' >&2
  exit 2
fi
export E2E_URL=https://127.0.0.1:29443
