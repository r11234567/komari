#!/usr/bin/env bash
# Source this before *any* side effects, including cleanup. Never run on a VPS.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "${RUNNER_OS:-}" != Linux ]]; then
  echo 'Real E2E is restricted to GitHub-hosted Linux runners.' >&2
  exit 2
fi
if [[ -z "${RUNNER_TEMP:-}" || "${E2E_ROOT:-}" != "$RUNNER_TEMP/komari-e2e" || "${E2E_ARTIFACTS:-}" != "$GITHUB_WORKSPACE/ci-artifacts/e2e" ]]; then
  echo 'Unexpected E2E paths; refusing to touch the host.' >&2
  exit 2
fi
export E2E_URL=https://127.0.0.1:29443
