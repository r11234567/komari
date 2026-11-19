import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("advisory_runner", Path(__file__).resolve().parents[1] / "ci/run-security-advisories.py")
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class MatrixEvidenceTest(unittest.TestCase):
    matrix = {"GHSA-test": {"web/security": ["TestBoundary"]}}

    def event(self, action, test="TestBoundary"):
        return {"Action": action, "Package": runner.MODULE + "web/security", "Test": test}

    def test_requires_actual_named_pass(self):
        self.assertTrue(runner.evaluate([self.event("pass")], self.matrix)["passed"])
        for events in ([], [self.event("skip")], [self.event("fail")], [self.event("pass", "OtherTest")]):
            self.assertFalse(runner.evaluate(events, self.matrix)["passed"])

    def test_package_failure_overrides_test_pass(self):
        self.assertFalse(runner.evaluate([self.event("pass"), self.event("fail", None)], self.matrix)["passed"])

    def test_repeated_or_contradictory_evidence_is_not_accepted(self):
        for action in ("pass", "skip", "fail"):
            self.assertFalse(runner.evaluate([self.event("pass"), self.event(action)], self.matrix)["passed"])
