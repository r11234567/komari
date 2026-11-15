#!/usr/bin/env python3
"""Attribute real catalog pages after the synthetic online turnover test.

The Go SQLite build does not include dbstat. Use the runner's independent
SQLite reader against the saved growing database; never VACUUM it.
"""
import json
from pathlib import Path
import sqlite3
import sys

root = Path(sys.argv[1]).resolve()
database = root / 'metrics.db'
with sqlite3.connect(database.as_uri() + '?mode=ro', uri=True) as connection:
    owners = connection.execute('SELECT COUNT(*) FROM metric_telemetry_segments').fetchone()[0]
    members = connection.execute('SELECT COUNT(*) FROM metric_telemetry_members').fetchone()[0]
    pages = connection.execute("SELECT SUM(pgsize) FROM dbstat WHERE name IN (SELECT name FROM sqlite_master WHERE tbl_name IN ('metric_telemetry_segments','metric_telemetry_members'))").fetchone()[0]
assert owners == 180 and members == 5220, (owners, members)
assert 0 < pages <= 2 * 1024 * 1024, pages
report = dict(origin='formula-generated 6 nodes x 29 series x 30 days x 72 handoffs',
              handoffs=12960, owners=owners, members=members, catalog_bytes=pages,
              sql_bytes=database.stat().st_size, vacuum_used=False)
(root / 'catalog-pages.json').write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
print(json.dumps(report))
