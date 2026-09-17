#!/usr/bin/env python3
"""Build and qualify both existing evaluation HAPI activation configurations."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request

DEPLOY = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(DEPLOY / "validator/backport"))
import wire
import cache


def run(args, output, name, **kwargs):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, **kwargs)
    (output / (name + ".stdout")).write_bytes(result.stdout)
    (output / (name + ".stderr")).write_bytes(result.stderr)
    (output / (name + "-call.json")).write_text(json.dumps({"argv":args,"exit":result.returncode}, indent=2) + "\n")
    result.check_returncode()
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--image", default="shn-eval-hapi:local")
    args = parser.parse_args()
    output = args.evidence.resolve(); output.mkdir(parents=True, exist_ok=False)
    run(["docker","build","-f",str(DEPLOY / "eval/hapi/Dockerfile"),"-t",args.image,str(DEPLOY)], output,"build")
    image = json.loads(run(["docker","image","inspect",args.image],output,"image"))[0]
    for name, compose, service in (("provider", "eval/compose.eval.yml", "hapi"), ("payer", "eval/payer/compose.eval.payer.yml", "validator")):
        # Config rendering starts no service and uses no operator secret files.
        env = os.environ.copy(); env["SHN_SECRETS"] = str(output / "unused-secrets")
        rendered = json.loads(run(["docker","compose","-f",str(DEPLOY / compose),"config","--format","json"], output,name+"-compose",env=env))
        selected = rendered["services"][service]
        build = selected["build"]
        if Path(build["context"]).resolve() != DEPLOY or build["dockerfile"] != "eval/hapi/Dockerfile":
            raise ValueError("evaluation build context does not select shared source")
        activation = selected["environment"]
        expected = {"uscore", "pas"} | ({"cdex", "hrex"} if name == "provider" else set())
        actual = {k.split(".")[3] for k in activation if k.startswith("hapi.fhir.implementationguides.") and k.endswith(".name")}
        if actual != expected: raise ValueError("evaluation package activation changed")
        command = ["docker","run","-d","-p","127.0.0.1::8080"]
        for key, value in sorted(activation.items()): command += ["-e",key+"="+str(value)]
        command += [image["Id"]]
        cid = run(command,output,name+"-start").decode().strip()
        try:
            inspected = json.loads(run(["docker","inspect",cid],output,name+"-container"))[0]
            port = inspected["NetworkSettings"]["Ports"]["8080/tcp"][0]["HostPort"]
            base = "http://127.0.0.1:"+port+"/fhir/DEFAULT"
            deadline = time.monotonic() + 900
            while True:
                try:
                    with urllib.request.urlopen(base+"/metadata",timeout=10) as response:
                        if response.status == 200: break
                except (OSError, urllib.error.URLError): pass
                if time.monotonic() >= deadline: raise TimeoutError("HAPI metadata readiness expired")
                time.sleep(10)
            proof_path = output / (name+"-backport.json")
            run(["docker","cp",cid+":/app/backport-provenance.json",str(proof_path)],output,name+"-proof-copy")
            proof = cache.read_json(proof_path)
            cache.verify_provenance(DEPLOY/"validator/backport",proof,proof["hapi_source_platform"],proof["compiler_platform"])
            # The only effective runtime WAR is selected by the image, with no mounts.
            war_path = output / (name+"-main.war")
            run(["docker","cp",cid+":/app/main.war",str(war_path)],output,name+"-war-copy")
            if cache.file_hash(war_path) != proof["output_war_sha256"]: raise ValueError("effective evaluation WAR differs")
            wire.run(base,output/(name+"-validation"))
        finally:
            try: run(["docker","logs",cid],output,name+"-logs")
            finally: run(["docker","rm","-fv",cid],output,name+"-cleanup")

if __name__ == "__main__": main()
