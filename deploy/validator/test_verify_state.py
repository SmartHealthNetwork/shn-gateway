import copy
import importlib.util
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location("verify_state", Path(__file__).with_name("verify-state.py"))
v = importlib.util.module_from_spec(spec)
spec.loader.exec_module(v)


class VerificationTests(unittest.TestCase):
    def test_ready_rejections(self):
        valid = dict(schema=2, line="2.2", key="boot:123", state="ready", warm=v.ROWS)
        v.ready(valid, "2.2")
        for field, value in [("schema", 1), ("line", "2.1"), ("key", ""), ("state", "warming"), ("warm", v.ROWS[:-1]), ("warm", v.ROWS + ["extra"]), ("warm", v.ROWS[:10] + [v.ROWS[11], v.ROWS[10]] + v.ROWS[12:]), ("warm", v.ROWS[:-1] + [v.ROWS[0]]), ("row", "negative-meta"), ("failure", "failed"), ("fired_at", "2026-09-06T00:00:00Z")]:
            with self.subTest(field=field, value=value), self.assertRaises(AssertionError):
                v.ready(dict(valid, **{field: value}), "2.2")

    def test_log_rejections(self):
        valid = "\n".join(f"warmup: line=2.2 row={r} elapsed={e} outcome={o}" for r in v.ROWS for e, o in [("0s", "started"), ("1.234s", "completed")])
        v.logs(valid, "2.2")
        for text in ["", valid + "\n" + valid, valid.replace("1.234s", "10m"), valid.replace("1.234s", "invalid"), valid.replace("completed", "failed", 1), valid.replace("line=2.2", "line=2.1", 1)]:
            with self.subTest(text=text), self.assertRaises(AssertionError):
                v.logs(text, "2.2")

    def test_image_rejections(self):
        base = [{"Config": dict(Entrypoint=v.JAVA, User="65532:65532", WorkingDir="/app", Env=["PATH=/usr/bin"], Cmd=None)}]
        built = copy.deepcopy(base)
        built[0]["Config"].update(Entrypoint=["/healthcheck", "supervise"] + v.JAVA, Env=["PATH=/usr/bin", "SHN_IG_LINE=2.2", "hapi.fhir.implementationguides.pas.version=2.2.1"])
        v.image(base, built, "2.2")
        for field, value in [("Entrypoint", v.JAVA), ("Entrypoint", ["/healthcheck", "supervise", "sh", "-c", "java"]), ("User", "0"), ("WorkingDir", "/"), ("Env", ["SHN_IG_LINE=2.2"]), ("Cmd", ["extra"])]:
            wrong = copy.deepcopy(built)
            wrong[0]["Config"][field] = value
            with self.subTest(field=field), self.assertRaises(AssertionError):
                v.image(base, wrong, "2.2")
        wrong = copy.deepcopy(built)
        wrong[0]["Config"]["Env"][-1] = "hapi.fhir.implementationguides.pas.version=2.1.0"
        with self.assertRaises(AssertionError):
            v.image(base, wrong, "2.2")


if __name__ == "__main__":
    unittest.main()
