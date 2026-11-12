#!/usr/bin/env python3
"""Compare compact legacy/telemetry projections of a STOPPED migration copy.

The original and maintained database are read-only inputs. No source table,
archive or retention is changed. The workspace must not already exist.
"""

import argparse
import hashlib
import json
from pathlib import Path
import sqlite3


def connect_readonly(path):
    return sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)


def table_inventory(path):
    with connect_readonly(path) as db:
        tables = [r[0] for r in db.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")]
        return {
            'file_bytes': path.stat().st_size,
            'page_size': db.execute('PRAGMA page_size').fetchone()[0],
            'freelist_pages': db.execute('PRAGMA freelist_count').fetchone()[0],
            'rows': {name: db.execute('SELECT COUNT(*) FROM "' + name + '"').fetchone()[0]
                     for name in tables},
            'objects_bytes': dict(db.execute('SELECT name,SUM(pgsize) FROM dbstat GROUP BY name')),
        }


def legacy_hashes(path):
    result = {}
    with connect_readonly(path) as db:
        tables = [r[0] for r in db.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' "
            "AND name NOT LIKE 'metric_telemetry_%' ORDER BY name")]
        for table in tables:
            columns = db.execute('PRAGMA table_info("' + table + '")').fetchall()
            keys = [c[1] for c in sorted(columns, key=lambda c: c[5]) if c[5]]
            order = ','.join('"' + key + '"' for key in keys) if keys else 'rowid'
            digest = hashlib.sha256()
            for row in db.execute('SELECT * FROM "' + table + '" ORDER BY ' + order):
                encoded = [{'blob_sha256': hashlib.sha256(v).hexdigest(), 'bytes': len(v)}
                           if isinstance(v, bytes) else v for v in row]
                digest.update(json.dumps(encoded, separators=(',', ':'), ensure_ascii=True).encode() + b'\n')
            result[table] = digest.hexdigest()
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--original-db', type=Path, required=True)
    parser.add_argument('--maintained-copy-db', type=Path, required=True)
    parser.add_argument('--archive', type=Path, required=True)
    parser.add_argument('--workspace', type=Path, required=True)
    args = parser.parse_args()
    source = args.original_db.resolve(strict=True)
    maintained = args.maintained_copy_db.resolve(strict=True)
    archive = args.archive.resolve(strict=True)
    workspace = args.workspace.resolve()
    if source == maintained or workspace == archive or workspace.is_relative_to(archive):
        raise SystemExit('Use a separate stopped migration copy and new projection workspace.')
    workspace.mkdir(parents=True, exist_ok=False)
    report = {'method': 'same stopped snapshot, separate vacuumed projections',
              'limitations': ['Not a new migration replay.', 'File and catalog verification is not an observation-value hash.'],
              'maintained_mixed_db': table_inventory(maintained)}
    for mode in ['legacy', 'telemetry']:
        path = workspace / (mode + '.db')
        with connect_readonly(maintained) as src, sqlite3.connect(path) as dst:
            src.backup(dst)
        with sqlite3.connect(path) as db:
            db.execute('PRAGMA foreign_keys=OFF')
            tables = [r[0] for r in db.execute(
                "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")]
            for name in tables:
                telemetry = name.startswith('metric_telemetry_')
                common = name == 'metric_definitions'
                if not common and telemetry != (mode == 'telemetry'):
                    db.execute('DROP TABLE "' + name + '"')
            db.commit()
            db.execute('VACUUM')
        report[mode] = table_inventory(path)
    original_hashes = legacy_hashes(source)
    assert original_hashes == legacy_hashes(workspace / 'legacy.db'), 'Legacy source population changed'
    report['legacy_content_sha256'] = original_hashes
    files = {p.stem.removesuffix('.jsonl'): p for p in archive.rglob('*.jsonl.zst')}
    assert len(files) == len(list(archive.rglob('*.jsonl.zst'))), 'Duplicate archive IDs'
    archive_points, verified = 0, 0
    with connect_readonly(maintained) as db:
        report['schema_progress'] = dict(db.execute(
            "SELECT task,value FROM metric_telemetry_progress "
            "WHERE task IN ('schema_version','legacy_migration_complete','legacy_migration_now')"))
        raw = db.execute('SELECT COALESCE(SUM(point_count),0) FROM metric_point_blocks').fetchone()[0]
        report['segment_states'] = db.execute(
            'SELECT state,COUNT(*),SUM(points),SUM(file_bytes) FROM metric_telemetry_segments GROUP BY state').fetchall()
        report['summary_tiers'] = db.execute(
            "SELECT resolution,COUNT(*),SUM(json_extract(stats,'$.attempts')),SUM(LENGTH(stats)) "
            'FROM metric_telemetry_summary GROUP BY resolution ORDER BY resolution').fetchall()
        for sid, points, checksum, size in db.execute(
                "SELECT id,points,checksum,file_bytes FROM metric_telemetry_segments "
                "WHERE state IN ('archived','complete','deleting')"):
            path = files[sid]
            assert path.stat().st_size == size, sid
            with path.open('rb') as f:
                assert hashlib.file_digest(f, 'sha256').hexdigest() == checksum, sid
            verified += 1
            archive_points += points
    assert verified == len(files), 'Uncatalogued archives'
    assert raw == archive_points, 'Archived raw population differs from source'
    report['archive'] = {'files_verified': verified, 'points': archive_points,
                         'bytes': sum(p.stat().st_size for p in files.values())}
    report['ratios'] = {
        'online_vs_legacy': report['telemetry']['file_bytes'] / report['legacy']['file_bytes'],
        'online_plus_archive_vs_legacy': (report['telemetry']['file_bytes'] + report['archive']['bytes']) / report['legacy']['file_bytes'],
    }
    (workspace / 'capacity.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps({'ratios': report['ratios'], 'archive': report['archive']}), flush=True)


if __name__ == '__main__':
    main()
