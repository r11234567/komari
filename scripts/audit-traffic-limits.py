#!/usr/bin/env python3
"""Inspect pre-existing quota pollution without changing any database values."""

import argparse
from contextlib import closing
import json
from pathlib import Path
import sqlite3
import sys


def audit(path: Path) -> int:
    path = path.resolve(strict=True)
    if not path.is_file():
        raise OSError("database path must be an existing file")
    count = 0
    with closing(sqlite3.connect(path.as_uri() + "?mode=ro", uri=True, timeout=5)) as db:
        db.execute("PRAGMA query_only = ON")
        rows = db.execute(
            "SELECT uuid, typeof(traffic_limit) FROM clients "
            "WHERE typeof(traffic_limit) <> 'integer' OR traffic_limit < 0 "
            "ORDER BY uuid"
        )
        for agent_id, storage_type in rows:
            # Never echo a possibly executable/huge stored payload into logs.
            print(json.dumps({"agent_id": agent_id, "storage_type": storage_type,
                              "reason": "negative" if storage_type == "integer" else "non_integer"},
                             ensure_ascii=True))
            count += 1
    print(json.dumps({"anomalies": count, "modified": False}))
    return 1 if count else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("database", type=Path, help="existing Komari SQLite database or consistent backup")
    args = parser.parse_args()
    try:
        return audit(args.database)
    except (OSError, sqlite3.Error) as error:
        print(json.dumps({"error": str(error)}, ensure_ascii=True), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
