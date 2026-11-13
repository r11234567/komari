"""Inspect only the hosted synthetic migration database; never accept a path."""
import json
import os
from pathlib import Path
import sqlite3
import subprocess

subprocess.run(["bash", str(Path(__file__).with_name("guard.sh"))], check=True)
root = Path(os.environ["E2E_ROOT"]).resolve()
source = root / "migration-panel/data/metrics.db"
manifest = json.loads((root / "migration-manifest.json").read_text())
oracle = json.loads((root / "migration-curve-oracle.json").read_text())
assert oracle["origin"] == "formula-generated synthetic observations only"
with sqlite3.connect(source.as_uri() + "?mode=ro", uri=True) as db:
    db.execute("PRAGMA query_only=ON")
    integrity = db.execute("PRAGMA integrity_check").fetchone()[0]
    assert integrity == "ok", integrity
    tables = []
    for name, ddl in db.execute("SELECT name,sql FROM sqlite_master WHERE type='table' AND name LIKE 'metric_telemetry_%' ORDER BY name"):
        assert name.replace("_", "").isalnum()
        indexes=[]
        for index in db.execute(f'PRAGMA index_list("{name}")'):
            index_name=index[1]
            assert index_name.replace("_", "").isalnum()
            indexes.append({"definition":index,"columns":list(db.execute(f'PRAGMA index_info("{index_name}")'))})
        tables.append({"name": name, "ddl": ddl,
                       "columns": list(db.execute(f'PRAGMA table_info("{name}")')),
                       "indexes": indexes,
                       "rows": db.execute(f'SELECT COUNT(*) FROM "{name}"').fetchone()[0]})
    tiers = list(db.execute("SELECT resolution/1000000000,COUNT(*),SUM(length(stats)) FROM metric_telemetry_summary GROUP BY resolution ORDER BY resolution"))
    assert all(tier[0] in {10,60,300,1800,3600,7200,21600,43200,86400} for tier in tiers), tiers
    assert db.execute("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('metric_points','metric_rollups','metric_point_blocks','metric_rollup_blocks')").fetchone()[0] == 0
    pages = list(db.execute("SELECT name,COUNT(*),SUM(pgsize),SUM(payload),SUM(unused) FROM dbstat GROUP BY name ORDER BY SUM(pgsize) DESC"))
    report = {"origin": oracle["origin"], "source_points": manifest["synthetic_points"],
              "integrity": integrity, "file_bytes": source.stat().st_size,
              "freelist_pages": db.execute("PRAGMA freelist_count").fetchone()[0],
              "tables": tables, "tiers_seconds_cells_payload": tiers, "page_allocation": pages,
              "statistics_encoding":list(db.execute("SELECT CASE WHEN stats LIKE '\"TSC1:%' THEN 'sealed-zstd' ELSE 'plain-json' END,COUNT(*),SUM(length(stats)) FROM metric_telemetry_summary GROUP BY 1")),
              "summary_lookup_plan":list(db.execute("EXPLAIN QUERY PLAN SELECT bucket FROM metric_telemetry_summary WHERE resolution=60000000000 AND promoted_through=0 ORDER BY bucket LIMIT 1"))}
output = Path(os.environ["E2E_ARTIFACTS"]) / "storage-synthetic-table-audit.json"
output.write_text(json.dumps(report, indent=2) + "\n")
