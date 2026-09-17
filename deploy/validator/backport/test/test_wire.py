"""A missing explicit profile needs its exact error, never metadata readiness."""
import sys
sys.dont_write_bytecode = True
from pathlib import Path
sys.path.insert(0, str(Path(__file__).parents[1]))
import unittest
import wire

class VerdictTest(unittest.TestCase):
    def test_positive_and_exact_refusal(self):
        self.assertTrue(wire.accepts(200, {"resourceType":"OperationOutcome","issue":[]}, None))
        profile = wire.PROFILE + "|9.9.9"
        issue = {"severity":"error","code":"processing", "details":{"coding":[{"code":"Validation_VAL_Profile_Unknown"}]},
                 "diagnostics":"Invalid profile. Failed to retrieve explicitly requested profile with url=" + profile}
        body = {"resourceType":"OperationOutcome","issue":[issue]}
        self.assertTrue(wire.accepts(200, body, profile))
        for name, status, payload in [
            ("server exception",500,body), ("empty",200,{"resourceType":"OperationOutcome","issue":[]}),
            ("warning",200,{"resourceType":"OperationOutcome","issue":[issue | {"severity":"warning"}]}),
            ("wrong canonical",200,{"resourceType":"OperationOutcome","issue":[issue | {"diagnostics":"another failure"}]}),
            ("unrelated error",200,{"resourceType":"OperationOutcome","issue":[issue, {"severity":"error","code":"invalid"}]}),
        ]:
            with self.subTest(name=name): self.assertFalse(wire.accepts(status,payload,profile))
        self.assertFalse(wire.accepts(200,body,None))
        self.assertFalse(wire.accepts(200,{"resourceType":"Patient"},None))

    def test_every_issue_requires_recognized_severity(self):
        profile = wire.PROFILE + "|9.9.9"
        exact = {"severity":"error", "code":"processing",
                 "details":{"coding":[{"code":"Validation_VAL_Profile_Unknown"}]},
                 "diagnostics":"Invalid profile. Failed to retrieve explicitly requested profile with url=" + profile}
        malformed = [{}, *({"severity": value} for value in ("", "unknown", None, 42, True, [], {}))]
        for bad in malformed:
            for missing, valid in ((None, {"severity":"information", "code":"informational"}), (profile, exact)):
                for issues in ([bad], [valid,bad], [bad,valid]):
                    with self.subTest(bad=bad,missing=missing,issues=issues):
                        self.assertFalse(wire.accepts(200,{"resourceType":"OperationOutcome","issue":issues},missing))
        for severity in ("warning", "information"):
            issue = {"severity":severity,"code":"informational","diagnostics":"Ordinary diagnostic"}
            self.assertTrue(wire.accepts(200,{"resourceType":"OperationOutcome","issue":[issue]},None))
            self.assertTrue(wire.accepts(200,{"resourceType":"OperationOutcome","issue":[issue,exact]},profile))
        warning = {"severity":"warning","code":"not-found","diagnostics":"Unknown profile"}
        self.assertFalse(wire.accepts(200,{"resourceType":"OperationOutcome","issue":[warning]},None))
        self.assertFalse(wire.accepts(200,{"resourceType":"OperationOutcome","issue":[{"severity":"fatal","code":"exception"}]},None))

if __name__ == "__main__": unittest.main()
