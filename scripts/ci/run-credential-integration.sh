#!/usr/bin/env bash
set -euo pipefail
[[ "${GITHUB_ACTIONS:-}" == true ]] || { echo 'CI only';exit 2; }
agent_dir="$PWD/credential-agent"
[[ -f "$agent_dir/go.mod" ]] || { echo 'Exact Agent checkout missing';exit 1; }
backup=$(mktemp -d)
cp go.mod go.sum "$backup/"
trap 'cp "$backup/go.mod" go.mod;cp "$backup/go.sum" go.sum;rm -f web/connect/credential_agent_integration_test.go;rm -rf "$backup"' EXIT
cp scripts/ci/credential/agent_integration.go.txt web/connect/credential_agent_integration_test.go
go mod edit -require=github.com/komari-monitor/komari-agent@v0.0.0 -replace=github.com/komari-monitor/komari-agent="$agent_dir"
GOFLAGS=-mod=mod bash scripts/ci/run-go-tests.sh -race -run '^TestCredentialLifecycleActualAgent' ./web/connect
