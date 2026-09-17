"""Native stripping may remove only the existing Mac sqlite payload entries."""
import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest
import zipfile
import io
import json
import test_cache
sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).parents[1]))
spec = importlib.util.spec_from_file_location("runtime", Path(__file__).parents[1] / "runtime.py")
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


def jar(entries):
    stream = io.BytesIO()
    with zipfile.ZipFile(stream, "w") as z:
        for key, value in entries.items(): z.writestr(key, value)
    return stream.getvalue()


class StrippingTest(unittest.TestCase):
    def test_only_mac_natives_can_change(self):
        with tempfile.TemporaryDirectory() as directory:
            source, stripped = Path(directory)/"source.war", Path(directory)/"stripped.war"
            sqlite = {"org/sqlite/Driver.class": b"java", "org/sqlite/native/Mac/aarch64/lib.dylib": b"mac",
                      "org/sqlite/native/Linux/aarch64/lib.so": b"linux"}
            clean = {k:v for k,v in sqlite.items() if not k.startswith("org/sqlite/native/Mac/")}
            original = {"WEB-INF/lib/sqlite-jdbc-3.jar": jar(sqlite), "WEB-INF/lib/validation.jar": b"retained", "META-INF/MANIFEST.MF": b"original"}
            source.write_bytes(jar(original))
            positive = original | {"WEB-INF/lib/sqlite-jdbc-3.jar": jar(clean)}
            stripped.write_bytes(jar(positive))
            runtime.verify_stripping(source, stripped)
            for name, mutated in {
                "missing unrelated entry": {k:v for k,v in positive.items() if k != "META-INF/MANIFEST.MF"},
                "validation mutation": positive | {"WEB-INF/lib/validation.jar": b"changed"},
                "Java class mutation": positive | {"WEB-INF/lib/sqlite-jdbc-3.jar": jar(clean | {"org/sqlite/Driver.class":b"changed"})},
                "other native mutation": positive | {"WEB-INF/lib/sqlite-jdbc-3.jar": jar(clean | {"org/sqlite/native/Linux/aarch64/lib.so":b"changed"})},
                "Mac retained": original,
                "new entry": positive | {"new.class": b"unprovided"},
            }.items():
                stripped.write_bytes(jar(mutated))
                with self.subTest(name=name), self.assertRaises(ValueError): runtime.verify_stripping(source, stripped)


class AdmissionTest(unittest.TestCase):
    def setUp(self):
        test_cache.CacheTest.setUp(self)
        self.destination = Path(self.temp.name) / "runtime"
        self.script = Path(self.temp.name) / "build.sh"
        self.script.write_text("existing native strip procedure")

    def stage(self):
        return runtime.stage(self.root, self.out, self.destination, self.script)

    def complete(self):
        self.assertEqual(self.stage(), "refreshed")
        runtime.record(self.root, self.out, self.destination, self.script)

    def test_runtime_fresh_reuse_and_packaged_without_cache(self):
        self.complete()
        self.assertEqual(self.stage(), "reused")
        expected = runtime.verify(self.root, self.out, self.destination, self.script)
        # Exported assets are admitted without an unstripped build cache.
        import shutil
        shutil.rmtree(self.out)
        self.assertEqual(runtime.packaged(self.root, self.destination, self.script)["nativeStripping"], expected)

    def test_partial_runtime_requires_complete_restrip(self):
        self.complete()
        (self.destination / "stripping-provenance.json").unlink()
        self.assertEqual(self.stage(), "refreshed")
        with self.assertRaises(ValueError): runtime.packaged(self.root, self.destination, self.script)
        runtime.record(self.root, self.out, self.destination, self.script)
        self.assertEqual(self.stage(), "reused")

    def test_changed_stripper_refreshes_and_invalidates_prewarm_identity(self):
        self.complete()
        previous = (self.destination / "stripping-provenance.json").read_bytes()
        self.script.write_text("updated native strip procedure")
        with self.assertRaises(ValueError): runtime.packaged(self.root, self.destination, self.script)
        self.assertEqual(self.stage(), "refreshed")
        runtime.record(self.root, self.out, self.destination, self.script)
        self.assertNotEqual(previous, (self.destination / "stripping-provenance.json").read_bytes())

    def test_tampered_runtime_refuses_reuse_and_packaging(self):
        self.complete()
        (self.destination / "main.war").write_bytes(b"tampered")
        with self.assertRaises(ValueError): self.stage()
        with self.assertRaises(ValueError): runtime.packaged(self.root, self.destination, self.script)

    def test_wrong_platform_or_partial_provenance_refused(self):
        self.complete()
        path = self.destination / "backport-provenance.json"
        original = path.read_bytes()
        for key, value in (("hapi_source_platform", "linux/amd64"), ("compiler_platform_digest", "wrong"), ("build_key", "wrong")):
            changed = json.loads(original); changed[key] = value
            path.write_text(json.dumps(changed))
            with self.subTest(key=key), self.assertRaises(ValueError): runtime.packaged(self.root, self.destination, self.script)
        path.write_bytes(original)
        (self.destination / "stripping-provenance.json").unlink()
        with self.assertRaises(ValueError): runtime.packaged(self.root, self.destination, self.script)


if __name__ == "__main__": unittest.main()
