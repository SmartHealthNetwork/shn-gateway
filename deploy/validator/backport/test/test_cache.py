"""Cache admission must bind current inputs and reject modified or partial output."""
import copy
import hashlib
import importlib.util
import io
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


def write_war(path, jar, utilities, method=zipfile.ZIP_STORED):
    with zipfile.ZipFile(path, "w") as z:
        z.writestr(cache.JAR_ENTRY, jar, compress_type=zipfile.ZIP_STORED)
        z.writestr(cache.UTILITIES_JAR_ENTRY, utilities, compress_type=method)


class CacheTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "source"
        self.out = Path(self.temp.name) / "output"
        self.root.mkdir(); self.out.mkdir()
        for name in ("BuildBackport.java", "src/ValidatorWrapper.java", "upstream/ValidatorWrapper.java",
                     "upstream/RegexTimeout.java", "upstream/PrimitiveRegexes.java"):
            p = self.root / name; p.parent.mkdir(exist_ok=True); p.write_text(name)
        self.cls = b"synthetic pinned class bytes"
        self.jar = self.out / "hapi-fhir-validation-8.12.0.jar"
        with zipfile.ZipFile(self.jar, "w") as z:
            z.writestr(cache.CLASS_ENTRY, self.cls)
        self.utilities = self.out / "org.hl7.fhir.utilities-6.9.12.jar"
        with zipfile.ZipFile(self.utilities, "w") as z:
            for name in cache.REGEX_CLASSES:
                z.writestr(name, b"synthetic " + name.encode())
        write_war(self.out / "main.war", self.jar.read_bytes(), self.utilities.read_bytes())
        with zipfile.ZipFile(self.utilities) as z:
            regex = cache.classes_identity(z, cache.REGEX_CLASSES)
        outputs = {"output_class_sha256": cache.digest(self.cls), "output_jar_sha256": cache.file_hash(self.jar),
                   "output_utilities_jar_sha256": cache.file_hash(self.utilities), "output_regex_classes_sha256": regex,
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
        for name in ("complete", "provenance.json", "request.json", "classpath.sha256", "main.war", self.jar.name, self.utilities.name):
            p = self.out / name; data = p.read_bytes(); p.unlink()
            with self.subTest(name=name), self.assertRaises((ValueError, OSError)): self.verify()
            p.write_bytes(data)

    def test_source_and_builder_changes(self):
        for name in ("src/ValidatorWrapper.java", "BuildBackport.java", "upstream/RegexTimeout.java", "upstream/PrimitiveRegexes.java"):
            p = self.root / name; original = p.read_bytes(); p.write_bytes(original + b"changed")
            with self.subTest(name=name), self.assertRaises(ValueError): self.verify()
            p.write_bytes(original)

    def test_cross_platform_and_unknown(self):
        for platform in ("linux/amd64", "linux/arm/v7", ""):
            with self.subTest(platform=platform), self.assertRaises(ValueError):
                cache.verify(self.root, self.out, platform, "linux/arm64")

    def test_wrong_child_or_output_hash_even_with_recomputed_key(self):
        for key in ("hapi_source_child", "compiler_platform_digest", "output_class_sha256", "input_war_sha256",
                    "input_utilities_jar_sha256", "input_regex_classes_sha256", "output_regex_classes_sha256",
                    "output_utilities_jar_sha256", "regex_timeout_source_sha256"):
            wrong = copy.deepcopy(self.provenance); wrong[key] = "0" * 64
            wrong["build_key"] = cache.build_key(wrong)
            (self.out / "provenance.json").write_text(json.dumps(wrong))
            (self.out / "complete").write_text(wrong["build_key"] + "\n")
            with self.subTest(key=key), self.assertRaises(ValueError): self.verify()

    def effective(self, utilities, method=zipfile.ZIP_STORED):
        write_war(self.out / "main.war", self.jar.read_bytes(), utilities, method)
        return cache.verify_effective_war(self.out / "main.war", self.provenance)

    def utilities_with(self, changes, drop=()):
        stream = io.BytesIO()
        with zipfile.ZipFile(self.utilities) as source, zipfile.ZipFile(stream, "w") as z:
            for name in source.namelist():
                if name not in drop:
                    z.writestr(name, source.read(name))
            for name, body in changes.items():
                z.writestr(name, body)
        return stream.getvalue()

    def test_effective_regex_classes(self):
        self.effective(self.utilities.read_bytes())
        guard = cache.REGEX_PACKAGE + "RegexTimeout.class"
        for name, body in {
            "the 6.9.12 guard back": self.utilities_with({guard: b"6.9.12"}, drop=(guard,)),
            "a class missing": self.utilities_with({}, drop=(cache.REGEX_CLASSES[0],)),
            "the removed class kept": self.utilities_with({cache.REGEX_REMOVED_CLASS: b"6.9.12"}),
        }.items():
            with self.subTest(name=name), self.assertRaises(ValueError): self.effective(body)
        with self.assertRaises(ValueError): self.effective(self.utilities.read_bytes(), zipfile.ZIP_DEFLATED)

    def test_duplicate_json_and_symlink(self):
        (self.out / "request.json").write_text('{"key":"a","key":"a"}')
        with self.assertRaises(ValueError): self.verify()
        (self.out / "request.json").unlink()
        (self.out / "request.json").symlink_to(self.root / "provenance.json")
        with self.assertRaises(ValueError): self.verify()


if __name__ == "__main__": unittest.main()
