#!/usr/bin/env python3
"""Require successful independent checks for the Web commit pinned by the backend."""
import json
import pathlib
import subprocess
import sys

LEGACY_WORKFLOWS = (
    "development.yaml", "codeql.yml", "privileged-delivery.yml", "terminal-workbench.yml",
    "admin-ui.yml", "lite-alerts.yml", "lite-billing.yml", "credential-lifecycle.yml",
    "native-otp.yml", "native-password-login.yml", "trusted-actions.yml", "workspace-gate.yml",
)
EVENTS = {"push", "pull_request", "workflow_dispatch"}


def discover(sha, workflows):
    # Read repository history by path: old workflow runs remain addressable
    # after the workflow files are retired. Always verify the recorded SHA.
    endpoint = "repos/r11234567/komari-web/actions/runs"
    latest = {}
    for query in (f"&head_sha={sha}", ""):
        page = 1
        while True:
            raw = subprocess.check_output(
                ["gh", "api", f"{endpoint}?per_page=100&page={page}{query}"], text=True
            )
            runs = json.loads(raw)["workflow_runs"]
            for run in runs:
                if run["head_sha"] != sha or run["event"] not in EVENTS:
                    continue
                name = run["path"].removeprefix(".github/workflows/")
                if name in workflows and (name not in latest or run["id"] > latest[name]["id"]):
                    latest[name] = {key: run[key] for key in (
                        "id", "status", "conclusion", "html_url", "run_attempt"
                    )}
            if len(latest) == len(workflows):
                return latest
            if len(runs) < 100:
                break
            page += 1
        # Some Actions searches omit CodeQL runs. Rescan unfiltered pages,
        # newest first, without carrying partial results from the search.
        if query:
            latest.clear()
    return latest


def main():
    sha = subprocess.check_output(
        ["git", "-C", "third_party/komari-web", "rev-parse", "HEAD"], text=True
    ).strip()
    layout = pathlib.Path("third_party/komari-web/.github/workflows/ci.yml")
    workflows = ("ci.yml", "codeql.yml") if layout.is_file() else LEGACY_WORKFLOWS
    runs = discover(sha, workflows)
    evidence = [{"workflow": name, "sha": sha, "run": runs.get(name)} for name in workflows]
    result = json.dumps({"web_sha": sha, "workflows": evidence}, indent=2)
    out = pathlib.Path("ci-artifacts/web-workflow-evidence.json")
    out.parent.mkdir(exist_ok=True)
    out.write_text(result + "\n", encoding="utf-8")
    print(result)
    if any(not row["run"] or row["run"]["status"] != "completed"
           or row["run"]["conclusion"] != "success" for row in evidence):
        sys.exit("Exact Web commit must pass every Web workflow before backend integration")


if __name__ == "__main__":
    main()
