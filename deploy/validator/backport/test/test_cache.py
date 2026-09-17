"""Cache admission must bind current inputs and reject modified or partial output."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
import zipfile
import sys
sys.dont_write_bytecode = True

SPEC = importlib.util.spec_from_file_location("backport_cache", Path(__file__).parents[1] / "cache.py")
cache = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cache)


class CacheTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "source"
        self.out = Path(self.temp.name) / "output"
        self.root.mkdir(); self.out.mkdir()
        for name in ("BuildBackport.java", "src/ValidatorWrapper.java", "upstream/ValidatorWrapper.java"):
            p = self.root / name; p.parent.mkdir(exist_ok=True); p.write_text(name)
        self.cls = b"synthetic pinned class bytes"
        self.jar = self.out / "hapi-fhir-validation-8.10.0.jar"
        with zipfile.ZipFile(self.jar, "w") as z:
            z.writestr(cache.CLASS_ENTRY, self.cls)
        with zipfile.ZipFile(self.out / "main.war", "w") as z:
            z.writestr(cache.JAR_ENTRY, self.jar.read_bytes(), compress_type=zipfile.ZIP_STORED)
        outputs = {"output_class_sha256": cache.digest(self.cls), "output_jar_sha256": cache.file_hash(self.jar),
                   "output_war_sha256": cache.file_hash(self.out / "main.war")}
        self.manifest = {"outputs": {"linux/arm64": outputs}, "compiler_image": cache.COMPILER}
        (self.root / "provenance.json").write_text(json.dumps(self.manifest))
        self.provenance = cache.expected_fields(self.root, "linux/arm64", "linux/arm64") | outputs
        self.provenance["classpath_sha256"] = cache.digest(b"classpath\n")
        (self.out / "classpath.sha256").write_text("classpath\n")
        self.provenance["build_key"] = cache.build_key(self.provenance)
        (self.out / "provenance.json").write_text(json.dumps(self.provenance))
        (self.out / "complete").write_text(self.provenance["build_key"] + "\n")
        (self.out / "request.json").write_text(json.dumps(cache.request(self.root, "linux/arm64", "linux/arm64")))

    def verify(self):
        return cache.verify(self.root, self.out, "linux/arm64", "linux/arm64")

    def test_fresh_and_reuse(self):
        self.assertEqual(self.verify(), self.verify())

    def test_tampered_output(self):
        (self.out / "main.war").write_bytes(b"tampered")
        with self.assertRaises(ValueError): self.verify()

    def test_partial_outputs(self):
        for name in ("complete", "provenance.json", "request.json", "classpath.sha256", "main.war", self.jar.name):
            p = self.out / name; data = p.read_bytes(); p.unlink()
            with self.subTest(name=name), self.assertRaises((ValueError, OSError)): self.verify()
            p.write_bytes(data)

    def test_source_and_builder_changes(self):
        for name in ("src/ValidatorWrapper.java", "BuildBackport.java"):
            p = self.root / name; original = p.read_bytes(); p.write_bytes(original + b"changed")
            with self.subTest(name=name), self.assertRaises(ValueError): self.verify()
            p.write_bytes(original)

    def test_cross_platform_and_unknown(self):
        for platform in ("linux/amd64", "linux/arm/v7", ""):
            with self.subTest(platform=platform), self.assertRaises(ValueError):
                cache.verify(self.root, self.out, platform, "linux/arm64")

    def test_wrong_child_or_output_hash_even_with_recomputed_key(self):
        for key in ("hapi_source_child", "compiler_platform_digest", "output_class_sha256", "input_war_sha256"):
            wrong = copy.deepcopy(self.provenance); wrong[key] = "0" * 64
            wrong["build_key"] = cache.build_key(wrong)
            (self.out / "provenance.json").write_text(json.dumps(wrong))
            (self.out / "complete").write_text(wrong["build_key"] + "\n")
            with self.subTest(key=key), self.assertRaises(ValueError): self.verify()

    def test_duplicate_json_and_symlink(self):
        (self.out / "request.json").write_text('{"key":"a","key":"a"}')
        with self.assertRaises(ValueError): self.verify()
        (self.out / "request.json").unlink()
        (self.out / "request.json").symlink_to(self.root / "provenance.json")
        with self.assertRaises(ValueError): self.verify()


if __name__ == "__main__": unittest.main()
