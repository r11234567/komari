#!/usr/bin/env python3
"""Require real backend and Nginx transport tests, including >30s runtime."""
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
required = {
    "TestRescueWatchFreshSignedHeartbeatAndCredentialRevocation",
    "TestRescueWatchNginxSurvivesThirtySecondsAndRevokes",
}
passed, rejected = set(), []
for line in (root / "tests.jsonl").read_text().splitlines():
    event = json.loads(line)
    if event.get("Action") == "fail" or (
        event.get("Test") in required and event.get("Action") == "skip"
    ):
        rejected.append(event)
    if event.get("Package") == "github.com/komari-monitor/komari/web/connect" and event.get("Action") == "pass" and event.get("Test") in required:
        if event["Test"] == "TestRescueWatchNginxSurvivesThirtySecondsAndRevokes" and event.get("Elapsed", 0) < 32:
            rejected.append(event)
        passed.add(event["Test"])
result = {"passed": sorted(passed), "missing": sorted(required - passed), "rejected": rejected}
(root / "required-evidence.json").write_text(json.dumps(result, indent=2) + "\n")
print(json.dumps(result, indent=2))
if result["missing"] or rejected:
    raise SystemExit("Backend/Nginx rescue stream evidence is missing, skipped, or failed")
