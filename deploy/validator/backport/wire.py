#!/usr/bin/env python3
"""Record unchanged-resource HTTP controls for shared explicit-profile behavior."""
import hashlib
import json
from pathlib import Path
import sys
import urllib.error
import urllib.parse
import urllib.request

PROFILE = "http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient"
RESOURCE = (json.dumps({"resourceType":"Patient", "id":"profile-control",
    "meta":{"profile":[PROFILE + "|6.1.0"]},
    "identifier":[{"system":"https://example.org/patients","value":"profile-control"}],
    "name":[{"family":"Example","given":["Pat"]}], "gender":"female", "birthDate":"1980-01-01"},
    separators=(",", ":")) + "\n").encode()


def accepts(status, outcome, missing):
    if status != 200 or not isinstance(outcome, dict) or outcome.get("resourceType") != "OperationOutcome":
        return False
    issues = outcome.get("issue")
    if not isinstance(issues, list) or any(not isinstance(i, dict) for i in issues):
        return False
    if any(not isinstance(i.get("severity"), str) or i["severity"] not in
           ("fatal", "error", "warning", "information") for i in issues):
        return False
    errors = [i for i in issues if i.get("severity") in ("error", "fatal")]
    if missing is None:
        # Resolution warnings cannot stand in for available-profile validation.
        return not errors and not any("profile" in str(i).lower() and any(s in str(i).lower() for s in ("unknown", "not found", "failed to")) for i in issues)
    if len(errors) != 1:
        return False
    issue = errors[0]
    return (issue.get("severity") == "error" and issue.get("code") == "processing"
        and issue.get("diagnostics") == "Invalid profile. Failed to retrieve explicitly requested profile with url=" + missing
        and any(c.get("code") == "Validation_VAL_Profile_Unknown" for c in issue.get("details", {}).get("coding", []) if isinstance(c, dict)))


def run(base, output):
    output.mkdir(parents=True, exist_ok=False)
    (output / "request.json").write_bytes(RESOURCE)
    summary = []
    for name, profile, missing in (
        ("available", PROFILE + "|6.1.0", None),
        ("missing-version", PROFILE + "|9.9.9", PROFILE + "|9.9.9"),
        ("missing-canonical", "https://example.org/fhir/StructureDefinition/unavailable-profile", "https://example.org/fhir/StructureDefinition/unavailable-profile"),
    ):
        url = base.rstrip("/") + "/Patient/$validate?" + urllib.parse.urlencode({"profile":profile})
        call = {"method":"POST", "url":url, "contentType":"application/fhir+json", "requestSha256":hashlib.sha256(RESOURCE).hexdigest()}
        (output / (name + "-call.json")).write_text(json.dumps(call, indent=2) + "\n")
        request = urllib.request.Request(url, data=RESOURCE, headers={"Content-Type":"application/fhir+json", "Accept":"application/fhir+json"})
        try:
            response = urllib.request.urlopen(request, timeout=60)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            body = response.read(); status = response.status
            headers = dict(response.headers.items())
        (output / (name + "-response.json")).write_bytes(body)
        (output / (name + "-http.json")).write_text(json.dumps({"status":status,"headers":headers}, indent=2) + "\n")
        try: passed = accepts(status, json.loads(body), missing)
        except (ValueError, TypeError): passed = False
        summary.append({"name":name,"status":status,"passed":passed})
    (output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    if not all(row["passed"] for row in summary):
        raise ValueError("explicit-profile HTTP controls failed: " + str(output))
    return summary

if __name__ == "__main__":
    if len(sys.argv) != 3: raise SystemExit("usage: wire.py FHIR_BASE NEW_EVIDENCE_DIR")
    print(json.dumps(run(sys.argv[1], Path(sys.argv[2])), indent=2))
