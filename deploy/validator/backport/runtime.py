#!/usr/bin/env python3
"""Admission for the Kit's existing native stripping, separate from build caches."""
import io
import json
from pathlib import Path
import shutil
import sys
import zipfile
import cache


def entries(archive):
    names = archive.namelist()
    if len(names) != len(set(names)):
        raise ValueError("duplicate runtime ZIP member")
    return set(names)


def verify_stripping(original, stripped):
    cache.file_hash(original); cache.file_hash(stripped)
    with zipfile.ZipFile(original) as before, zipfile.ZipFile(stripped) as after:
        names = entries(before)
        if names != entries(after):
            raise ValueError("native stripping changed outer entry set")
        sqlite = [n for n in names if n.startswith("WEB-INF/lib/sqlite-jdbc-") and n.endswith(".jar")]
        if len(sqlite) > 1:
            raise ValueError("ambiguous sqlite native input")
        for name in names:
            a, b = before.read(name), after.read(name)
            if name not in sqlite:
                if a != b:
                    raise ValueError("native stripping changed unrelated entry: " + name)
                continue
            if after.getinfo(name).compress_type != zipfile.ZIP_STORED:
                raise ValueError("stripped nested JAR must remain stored")
            with zipfile.ZipFile(io.BytesIO(a)) as old, zipfile.ZipFile(io.BytesIO(b)) as new:
                expected = {n for n in entries(old) if not n.startswith("org/sqlite/native/Mac/")}
                if entries(new) != expected:
                    raise ValueError("native stripping removed unrelated or retained Mac members")
                for member in expected:
                    if old.read(member) != new.read(member):
                        raise ValueError("native stripping changed retained sqlite member")


def verified_build(source, artifact):
    proof = cache.read_json(artifact / "provenance.json")
    return cache.verify(source, artifact, proof.get("hapi_source_platform"), proof.get("compiler_platform"))


def verify(source, artifact, destination, script):
    build = verified_build(source, artifact)
    if cache.read_json(destination / "backport-provenance.json") != build:
        raise ValueError("runtime backport inputs changed")
    stripping = cache.read_json(destination / "stripping-provenance.json")
    expected = {"format": "hapi-native-stripping-v1", "backport_build_key": build["build_key"],
                "unstripped_war_sha256": build["output_war_sha256"], "stripper_sha256": cache.file_hash(script),
                "stripped_war_sha256": cache.file_hash(destination / "main.war")}
    if stripping != expected:
        raise ValueError("stripped runtime provenance changed")
    verify_stripping(artifact / "main.war", destination / "main.war")
    return stripping


def packaged(source, destination, script):
    build = cache.read_json(destination / "backport-provenance.json")
    outputs = cache.verify_provenance(source, build, build.get("hapi_source_platform"), build.get("compiler_platform"))
    stripping = cache.read_json(destination / "stripping-provenance.json")
    expected = {"format": "hapi-native-stripping-v1", "backport_build_key": build["build_key"],
                "unstripped_war_sha256": outputs["output_war_sha256"], "stripper_sha256": cache.file_hash(script),
                "stripped_war_sha256": cache.file_hash(destination / "main.war")}
    if stripping != expected:
        raise ValueError("packaged HAPI provenance or bytes changed")
    with zipfile.ZipFile(destination / "main.war") as war:
        nested = cache.zip_member(war, cache.JAR_ENTRY)
        if war.getinfo(cache.JAR_ENTRY).compress_type != zipfile.ZIP_STORED or cache.digest(nested) != outputs["output_jar_sha256"]:
            raise ValueError("packaged HAPI validation JAR changed")
    with zipfile.ZipFile(io.BytesIO(nested)) as jar:
        if cache.digest(cache.zip_member(jar, cache.CLASS_ENTRY)) != outputs["output_class_sha256"]:
            raise ValueError("packaged HAPI class changed")
    return {"backport": build, "nativeStripping": stripping}


def atomic_json(path, value):
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_bytes(cache.canonical(value))
    temporary.replace(path)


def stage(source, artifact, destination, script):
    build = verified_build(source, artifact)
    destination.mkdir(parents=True, exist_ok=True)
    current = destination / "backport-provenance.json"
    strip_proof = destination / "stripping-provenance.json"
    if current.is_file() and strip_proof.is_file():
        old_build, old_strip = cache.read_json(current), cache.read_json(strip_proof)
        if old_build == build and old_strip.get("stripper_sha256") == cache.file_hash(script):
            verify(source, artifact, destination, script)
            return "reused"
    # Missing or superseded generated inputs require a complete strip and rewarm.
    temporary = destination / "main.war.tmp"
    shutil.copyfile(artifact / "main.war", temporary)
    temporary.replace(destination / "main.war")
    atomic_json(current, build)
    if strip_proof.exists(): strip_proof.unlink()
    return "refreshed"


def record(source, artifact, destination, script):
    build = verified_build(source, artifact)
    if cache.read_json(destination / "backport-provenance.json") != build:
        raise ValueError("runtime build changed during native stripping")
    verify_stripping(artifact / "main.war", destination / "main.war")
    proof = {"format": "hapi-native-stripping-v1", "backport_build_key": build["build_key"],
             "unstripped_war_sha256": build["output_war_sha256"], "stripper_sha256": cache.file_hash(script),
             "stripped_war_sha256": cache.file_hash(destination / "main.war")}
    atomic_json(destination / "stripping-provenance.json", proof)
    return proof


if __name__ == "__main__":
    try:
        if len(sys.argv) == 4 and sys.argv[1] == "packaged":
            print(json.dumps(packaged(Path(__file__).resolve().parent, Path(sys.argv[2]), Path(sys.argv[3])), sort_keys=True))
            sys.exit(0)
        if len(sys.argv) != 5 or sys.argv[1] not in ("stage", "record", "verify"):
            raise ValueError("usage: runtime.py stage|record|verify BUILD_DIR RUNTIME_DIR STRIPPER_SCRIPT")
        result = globals()[sys.argv[1]](Path(__file__).resolve().parent, Path(sys.argv[2]), Path(sys.argv[3]), Path(sys.argv[4]))
        print(result if isinstance(result, str) else json.dumps(result, sort_keys=True))
    except (OSError, ValueError, zipfile.BadZipFile) as error:
        print(f"backport runtime: {error}", file=sys.stderr)
        sys.exit(1)
