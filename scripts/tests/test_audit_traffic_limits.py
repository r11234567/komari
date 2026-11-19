import json
from contextlib import closing
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "audit-traffic-limits.py"


class QuotaAuditTests(unittest.TestCase):
    def test_existing_pollution_is_reported_without_payload_echo_or_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "komari.db"
            with closing(sqlite3.connect(path)) as db:
                db.execute("CREATE TABLE clients(uuid TEXT PRIMARY KEY, traffic_limit BIGINT)")
                db.executemany("INSERT INTO clients VALUES (?, ?)", [
                    ("good", 123), ("unlimited", 0), ("negative", -1),
                    ("polluted", "alert(document.cookie),1gb")])
                db.commit()
            before = path.read_bytes()
            result = subprocess.run([sys.executable, str(SCRIPT), str(path)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1, result.stderr)
            records = [json.loads(line) for line in result.stdout.splitlines()]
            self.assertEqual({row["agent_id"] for row in records[:-1]}, {"negative", "polluted"})
            self.assertEqual(records[-1], {"anomalies": 2, "modified": False})
            self.assertNotIn("document.cookie", result.stdout)
            self.assertEqual(path.read_bytes(), before)

    def test_missing_database_is_not_created(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "missing.db"
            result = subprocess.run([sys.executable, str(SCRIPT), str(path)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 2)
            self.assertFalse(path.exists())


if __name__ == "__main__":
    unittest.main()
