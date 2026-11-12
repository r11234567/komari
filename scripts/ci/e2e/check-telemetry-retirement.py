"""Check the real fixture DB; never open an operator-supplied production path."""
import json
import os
import sqlite3
import subprocess
from pathlib import Path

# Assert responsibilities, not a magic row count. The Go scheduler contract
# test runs this exact inspector against a store initialized by production code.
REQUIRED_TASKS = {
    "traffic", "summary", "seal", "archive", "delete", "promotion", "tiers",
    "summaries", "expire", "receipts", "erased", "checkpoint", "catalog",
    "ledger", "stats", "gaps",
}


def inspect_storage(db):
    db.execute("PRAGMA query_only=ON")
    tasks = [dict(zip(("name", "attempts", "last_error"), row)) for row in db.execute(
        "SELECT task,attempts,last_error FROM metric_telemetry_schedule ORDER BY task")]
    names = {task["name"] for task in tasks}
    return {
        "legacy_hot_points": db.execute("SELECT COUNT(*) FROM metric_point_values").fetchone()[0],
        "hot_points": db.execute("SELECT COUNT(*) FROM metric_telemetry_hot").fetchone()[0],
        "receipts": db.execute("SELECT COUNT(*) FROM metric_telemetry_receipts").fetchone()[0],
        "summaries": db.execute("SELECT COUNT(*) FROM metric_telemetry_summary").fetchone()[0],
        "complete_archives": db.execute("SELECT COUNT(*) FROM metric_telemetry_segments WHERE state='complete' AND points>0").fetchone()[0],
        "raw_blocks": db.execute("SELECT COUNT(*) FROM metric_point_blocks").fetchone()[0],
        "rollup_blocks": db.execute("SELECT COUNT(*) FROM metric_rollup_blocks").fetchone()[0],
        "scheduled_tasks": len(tasks),
        "task_failures": sum(task["attempts"] for task in tasks),
        "tasks": tasks,
        "missing_tasks": sorted(REQUIRED_TASKS - names),
        "unexpected_tasks": sorted(names - REQUIRED_TASKS),
    }


def validate_storage(result):
    if (result["hot_points"] <= 0 or result["receipts"] <= 0 or result["summaries"] <= 0
            or result["legacy_hot_points"] != 0 or result["raw_blocks"] != 0 or result["rollup_blocks"] != 0):
        raise ValueError("Legacy retirement invariant failed: " + json.dumps(result))
    if (result["missing_tasks"] or result["unexpected_tasks"] or result["task_failures"] != 0
            or any(task["last_error"] for task in result["tasks"])):
        raise ValueError("Durable scheduler invariant failed: " + json.dumps(result))


def main():
    subprocess.run(["bash", str(Path(__file__).with_name("guard.sh"))], check=True)
    root = Path(os.environ["E2E_ROOT"]).resolve()
    expected = Path(os.environ["RUNNER_TEMP"]).resolve() / "komari-e2e"
    if root != expected or (os.environ.get("KOMARI_E2E_LOCAL") != "1" and os.environ.get("RUNNER_OS") != "Linux"):
        raise SystemExit("Unexpected E2E root/platform")
    path = root / "panel" / "data" / "metrics.db"
    with sqlite3.connect(path.as_uri() + "?mode=ro", uri=True) as db:
        result = inspect_storage(db)
    # Keep the task identities and individual errors even when the check fails.
    output = Path(os.environ["E2E_ARTIFACTS"]) / "telemetry-retirement.json"
    output.write_text(json.dumps(result, indent=2) + "\n")
    try:
        validate_storage(result)
    except ValueError as error:
        raise SystemExit(str(error)) from error
    print(json.dumps(result))


if __name__ == "__main__":
    main()
