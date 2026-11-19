#!/usr/bin/env python3
"""Run the explicit seven-advisory contract and fail on missing/skipped tests."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
MODULE = "github.com/komari-monitor/komari/"


def evaluate(events, matrix):
    outcomes = {}
    failures = []
    for event in events:
        if event.get("Action") == "fail":
            failures.append({"package": event.get("Package"), "test": event.get("Test")})
        if event.get("Test") and event.get("Action") in ("pass", "fail", "skip"):
            key = (event.get("Package"), event["Test"])
            outcomes.setdefault(key, []).append(event["Action"])
    report = {}
    for advisory, packages in matrix.items():
        checks = []
        for package, tests in packages.items():
            for test in tests:
                observed = outcomes.get((MODULE + package, test), [])
                checks.append({"package": package, "test": test,
                               "passed": observed == ["pass"], "outcomes": observed})
        report[advisory] = {"passed": bool(checks) and all(c["passed"] for c in checks), "checks": checks}
    return {"passed": not failures and all(r["passed"] for r in report.values()),
            "advisories": report, "failures": failures}


def main():
    matrix = json.loads((ROOT / "scripts/ci/security-advisory-matrix.json").read_text())
    output = Path(sys.argv[1] if len(sys.argv) > 1 else ROOT / "ci-artifacts/security-advisories")
    output.mkdir(parents=True, exist_ok=True)
    packages = sorted({p for group in matrix.values() for p in group})
    names = sorted({t for group in matrix.values() for tests in group.values() for t in tests})
    command = ["go", "test", "-json", "-race", "-count=1", "-timeout=8m", "-p=2",
               "-run", "^(" + "|".join(re.escape(n) for n in names) + ")$"] + ["./" + p for p in packages]
    events = []
    with (output / "tests.jsonl").open("w", encoding="utf-8") as raw, (output / "tests.log").open("w", encoding="utf-8") as readable:
        process = subprocess.Popen(command, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                   text=True, encoding="utf-8", errors="replace", env=os.environ.copy())
        for line in process.stdout:
            raw.write(line)
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                readable.write(line)
                print(line, end="", flush=True)
                continue
            events.append(event)
            text = event.get("Output", "")
            readable.write(text)
            if event.get("Action") == "fail":
                print(json.dumps(event), flush=True)
        code = process.wait()
    report = evaluate(events, matrix)
    report["command"] = command
    report["exit_code"] = code
    report["passed"] = report["passed"] and code == 0
    (output / "matrix.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    for advisory, result in report["advisories"].items():
        print(advisory, "PASS" if result["passed"] else "FAIL", flush=True)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
