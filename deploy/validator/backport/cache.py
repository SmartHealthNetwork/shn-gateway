#!/usr/bin/env python3
"""Verify content-addressed build output before any runtime consumer reuses it."""
import hashlib
import io
import json
import os
import signal
from pathlib import Path
import subprocess
import sys
import tempfile
import zipfile

ENGINE = "hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1"
COMPILER = "eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7"
JAR_ENTRY = "WEB-INF/lib/hapi-fhir-validation-8.10.0.jar"
CLASS_ENTRY = "org/hl7/fhir/common/hapi/validation/validator/ValidatorWrapper.class"
SOURCES = {
    "linux/arm64": ("sha256:ac871f44ee311bdd4d3533f7ea9e81b3a744ca709726b74d5a97d29663a7916c", "94a8a650d470060025ef8c1c404b5451a9b91efa860a118e69bbdcda3c8221e4", "378563064"),
    "linux/amd64": ("sha256:f4aa83b7102a52f19a42c4c27c5f63fa3d1ded4f393c7083e53a39dc37b2a4c5", "378be32e09f643db6c01c703851e5a639d9ea17ebf7a0aafeeefaa66a0452009", "378563063"),
}
COMPILERS = {
    "linux/arm64": "sha256:4cc4a72887c92db0128b0725443a80291171759f9731300c12a63403504f76c3",
    "linux/amd64": "sha256:ea0e59a081c886d919b177b7006ac02fbdc64e8ab7cae724e2afd62de42d6892",
}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def file_hash(path):
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"missing or linked regular file: {path.name}")
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for data in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(data)
    return h.hexdigest()


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON field: {key}")
        result[key] = value
    return result


def read_json(path):
    file_hash(path)
    return json.loads(path.read_text(), object_pairs_hook=unique_object)


def canonical(value):
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()


def build_key(provenance):
    return digest(canonical({k: v for k, v in provenance.items() if k != "build_key" and not k.startswith("output_")}))


def expected_fields(source, platform, compiler_platform):
    if platform not in SOURCES or compiler_platform not in COMPILERS:
        raise ValueError("unsupported HAPI or compiler platform")
    child, war, size = SOURCES[platform]
    return {
        "format": "explicit-profile-backport-v1", "upstream_image": ENGINE,
        "hapi_source_platform": platform, "hapi_source_child": child,
        "input_war_sha256": war, "input_war_size": size,
        "input_jar_sha256": "cf4d24f810bcd29760296fc5f9f5908196e4253755d8194afd610dc456009deb",
        "input_class_sha256": "b7055a835c7fdae7b3e6834d63743b912394fa631feda3bde015c3f8ed1edf0c",
        "upstream_source_sha256": file_hash(source / "upstream/ValidatorWrapper.java"),
        "patched_source_sha256": file_hash(source / "src/ValidatorWrapper.java"),
        "source_manifest_sha256": file_hash(source / "provenance.json"),
        "builder_sha256": file_hash(source / "BuildBackport.java"),
        "compiler_image": COMPILER, "compiler_platform": compiler_platform,
        "compiler_platform_digest": COMPILERS[compiler_platform], "compiler_runtime": "21.0.11+10-LTS",
        "java_class_major": "61", "changed_jar_entry": CLASS_ENTRY, "changed_war_entry": JAR_ENTRY,
    }


def request(source, platform, compiler_platform):
    expected = expected_fields(source, platform, compiler_platform)
    inventory = {}
    for path in sorted(source.rglob("*")):
        if "__pycache__" in path.parts or path.suffix == ".pyc":
            continue
        if path.is_symlink():
            raise ValueError("source symlinks are not build inputs")
        if path.is_file():
            inventory[path.relative_to(source).as_posix()] = file_hash(path)
    identity = {"format": "backport-cache-v1", "inputs": expected, "source_files": inventory}
    return identity | {"key": digest(canonical(identity))}


def zip_member(archive, name):
    names = archive.namelist()
    if len(names) != len(set(names)) or name not in names:
        raise ValueError("duplicate or missing archive member")
    return archive.read(name)


def verify_provenance(source, recorded, platform, compiler_platform):
    expected = expected_fields(source, platform, compiler_platform)
    outputs = read_json(source / "provenance.json").get("outputs", {}).get(platform)
    if not isinstance(outputs, dict) or set(outputs) != {"output_class_sha256", "output_jar_sha256", "output_war_sha256"}:
        raise ValueError("source platform has no qualified output identities")
    if set(recorded) != set(expected) | set(outputs) | {"classpath_sha256", "build_key"}:
        raise ValueError("unexpected or missing build provenance fields")
    for key, value in (expected | outputs).items():
        if recorded.get(key) != value:
            raise ValueError(f"cache provenance mismatch: {key}")
    if recorded["build_key"] != build_key(recorded):
        raise ValueError("build key mismatch")
    return outputs


def verify(source, output, platform, compiler_platform):
    wanted = request(source, platform, compiler_platform)
    if read_json(output / "request.json") != wanted:
        raise ValueError("cache request inputs changed")
    recorded = read_json(output / "provenance.json")
    outputs = verify_provenance(source, recorded, platform, compiler_platform)
    file_hash(output / "complete")
    if (output / "complete").read_text() != recorded["build_key"] + "\n":
        raise ValueError("incomplete build")
    if file_hash(output / "classpath.sha256") != recorded["classpath_sha256"]:
        raise ValueError("classpath inventory changed")
    war = output / "main.war"
    jar = output / "hapi-fhir-validation-8.10.0.jar"
    if file_hash(war) != outputs["output_war_sha256"] or file_hash(jar) != outputs["output_jar_sha256"]:
        raise ValueError("cached output bytes changed")
    with zipfile.ZipFile(war) as archive:
        nested = zip_member(archive, JAR_ENTRY)
        if archive.getinfo(JAR_ENTRY).compress_type != zipfile.ZIP_STORED or digest(nested) != outputs["output_jar_sha256"]:
            raise ValueError("effective nested JAR identity or compression changed")
    with zipfile.ZipFile(io.BytesIO(nested)) as archive:
        if digest(zip_member(archive, CLASS_ENTRY)) != outputs["output_class_sha256"]:
            raise ValueError("effective class identity changed")
    return recorded


def run_build(command, timeout=1200):
    def interrupted(signum, _frame):
        raise InterruptedError(f"build interrupted by signal {signum}")
    previous = signal.signal(signal.SIGTERM, interrupted)
    child = None
    try:
        child = subprocess.Popen(command, stdout=sys.stderr, stderr=sys.stderr, start_new_session=True)
        try:
            return child.wait(timeout=timeout)
        except subprocess.TimeoutExpired as error:
            raise TimeoutError("pinned build exceeded its execution bound") from error
    finally:
        # Ignore repeated terminal signals until the owned process group has stopped.
        handlers = {sig: signal.signal(sig, signal.SIG_IGN) for sig in (signal.SIGINT, signal.SIGTERM)}
        try:
            if child is not None and child.poll() is None:
                os.killpg(child.pid, signal.SIGTERM)
                try:
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    child.wait()
        finally:
            signal.signal(signal.SIGINT, handlers[signal.SIGINT])
            signal.signal(signal.SIGTERM, previous)


def ensure(source, cache_root, platform):
    # BuildKit's target platform selects both source and compiler stages.
    wanted = request(source, platform, platform)
    cache_root.mkdir(parents=True, exist_ok=True)
    output = cache_root / wanted["key"]
    if output.exists():
        verify(source, output, platform, platform)
        return output
    lock = cache_root / (wanted["key"] + ".lock")
    lock.mkdir()  # A second writer fails; it cannot publish or remove another build.
    try:
        # Keep failed output and its log outside runtime assets for diagnosis.
        temporary = Path(tempfile.mkdtemp(prefix="build-", dir=cache_root))
        staging = temporary / "output"
        command = ["docker", "build", "--platform", platform, "--progress", "plain",
                   "--output", f"type=local,dest={staging}", str(source)]
        (temporary / "command.json").write_bytes(canonical(command))
        code = run_build(command)
        (temporary / "exit").write_text(str(code) + "\n")
        if code:
            raise RuntimeError(f"pinned backport build exit {code}; retained {temporary}")
        (staging / "request.json").write_bytes(canonical(wanted))
        verify(source, staging, platform, platform)
        staging.rename(output)
        return output
    finally:
        lock.rmdir()


if __name__ == "__main__":
    try:
        source = Path(__file__).resolve().parent
        if len(sys.argv) == 4 and sys.argv[1] == "ensure":
            print(ensure(source, Path(sys.argv[2]).resolve(), sys.argv[3]))
        elif len(sys.argv) == 3 and sys.argv[1] == "platform":
            output = Path(sys.argv[2])
            recorded = read_json(output / "provenance.json")
            platform = recorded.get("hapi_source_platform")
            verify(source, output, platform, recorded.get("compiler_platform"))
            print(platform)
        elif len(sys.argv) == 5 and sys.argv[1] == "verify":
            print(json.dumps(verify(source, Path(sys.argv[2]), sys.argv[3], sys.argv[4]), sort_keys=True))
        else:
            raise ValueError("usage: cache.py ensure CACHE_ROOT PLATFORM | verify OUTPUT HAPI_PLATFORM COMPILER_PLATFORM")
    except (OSError, ValueError, RuntimeError, zipfile.BadZipFile) as error:
        print(f"backport: {error}", file=sys.stderr)
        sys.exit(1)
