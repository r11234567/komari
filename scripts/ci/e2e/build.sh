#!/usr/bin/env bash
source "$(dirname "$0")/guard.sh"
mkdir -p "$E2E_ROOT/bin"
chmod 700 "$E2E_ROOT"
go build -trimpath -o "$E2E_ROOT/bin/komari" .
cd e2e-agent
go build -trimpath -o "$E2E_ROOT/bin/komari-agent" .
go build -trimpath -o "$E2E_ROOT/bin/komari-host" ./cmd/komari-host
