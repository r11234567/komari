"""Check the real fixture DB; never open an operator-supplied production path."""
import json
import os
import sqlite3
from pathlib import Path

if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
    raise SystemExit("This check requires the disposable GitHub-hosted E2E fixture")
root = Path(os.environ["E2E_ROOT"]).resolve()
expected = Path(os.environ["RUNNER_TEMP"]).resolve() / "komari-e2e"
if root != expected or os.environ.get("RUNNER_OS") != "Linux":
    raise SystemExit("Unexpected E2E root/platform")
path = root / "panel" / "data" / "metrics.db"
with sqlite3.connect(path.as_uri() + "?mode=ro", uri=True) as db:
    db.execute("PRAGMA query_only=ON")
    result = {
        "hot_points": db.execute("SELECT COUNT(*) FROM metric_point_values").fetchone()[0],
        "raw_blocks": db.execute("SELECT COUNT(*) FROM metric_point_blocks").fetchone()[0],
        "rollup_blocks": db.execute("SELECT COUNT(*) FROM metric_rollup_blocks").fetchone()[0],
    }
if result["hot_points"] <= 0 or result["raw_blocks"] != 0 or result["rollup_blocks"] != 0:
    raise SystemExit("Legacy retirement invariant failed: " + json.dumps(result))
output = Path(os.environ["E2E_ARTIFACTS"]) / "telemetry-retirement.json"
output.write_text(json.dumps(result, indent=2) + "\n")
print(json.dumps(result))
