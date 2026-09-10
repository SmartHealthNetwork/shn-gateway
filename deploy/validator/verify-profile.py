#!/usr/bin/env python3
"""Characterize an initialization profile without calling it a strict positive."""

import json
import sys

SEVERITIES = {"fatal", "error", "warning", "information"}
ISSUE_TYPES = {
    "invalid",
    "structure",
    "required",
    "value",
    "invariant",
    "security",
    "login",
    "unknown",
    "expired",
    "forbidden",
    "suppressed",
    "processing",
    "not-supported",
    "duplicate",
    "multiple-matches",
    "not-found",
    "deleted",
    "too-long",
    "code-invalid",
    "extension",
    "too-costly",
    "business-rule",
    "conflict",
    "transient",
    "lock-error",
    "no-store",
    "exception",
    "timeout",
    "incomplete",
    "throttled",
    "informational",
}
PAS_REQUEST_BUNDLE_PROFILE = (
    "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"
)
PAS_VERSIONS = {"2.0.1", "2.1.0", "2.2.1"}
MESSAGE_ID_SYSTEM = "http://hl7.org/fhir/java-core-messageId"
OPEN_SLICE_MESSAGE_ID = "Details_for__matching_against_Profile_"


def unique_object(pairs):
    result = {}
    folded = set()
    for key, value in pairs:
        lower = key.lower()
        if key in result or lower in folded:
            raise ValueError("duplicate or aliased JSON member")
        result[key] = value
        folded.add(lower)
    return result


def terminology_error(diagnostic):
    lower = diagnostic.lower()
    marker = any(
        value in lower
        for value in (
            "codesystem.x12.org",
            "valueset.x12.org",
            "x12.org/codes",
            "ama-assn.org/go/cpt",
            "hcpcsreleasecodesets",
            "x12278requestedservicetype",
            "x12278locationtype",
            "place_of_service_code_set",
            "pdexpainstitutionalprocedurecodesvs",
            "x12claimadjustmentreasoncodes",
        )
    )
    failure = any(
        value in lower
        for value in (
            "unable to expand",
            "is unknown and can't be validated",
            "could not be found",
            "none of the codings",
        )
    )
    return marker and failure


def fail_issue(reason, profile, index, issue):
    encoded = json.dumps(issue, ensure_ascii=False, separators=(",", ":"), sort_keys=True)
    sys.exit("FAIL: %s: %s; issue[%d]=%s" % (reason, profile, index, encoded))


def known_pas_open_slice_information(issue, profile):
    if (
        profile != PAS_REQUEST_BUNDLE_PROFILE
        or issue["severity"] != "information"
        or issue["code"] != "processing"
    ):
        return False
    if issue.get("details") != {
        "coding": [{"system": MESSAGE_ID_SYSTEM, "code": OPEN_SLICE_MESSAGE_ID}]
    }:
        return False
    if issue.get("expression") not in (["Bundle.entry[1]"], ["Bundle.entry[2]"]):
        return False
    diagnostics = {
        "This element does not match any known slice defined in the profile "
        + PAS_REQUEST_BUNDLE_PROFILE
        + "|"
        + version
        + " (this may not be a problem, but you should check that it's not intended "
        "to match a slice) - Does not match slice 'Claim' "
        "(discriminator: (resource is Claim))"
        for version in PAS_VERSIONS
    }
    return issue.get("diagnostics") in diagnostics


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: verify-profile.py PROFILE")
    profile = sys.argv[1]
    try:
        outcome = json.load(sys.stdin, object_pairs_hook=unique_object)
    except (json.JSONDecodeError, ValueError) as error:
        sys.exit("FAIL: malformed OperationOutcome: %s" % error)
    if not isinstance(outcome, dict) or outcome.get("resourceType") != "OperationOutcome":
        sys.exit("FAIL: non-OperationOutcome for %s" % profile)
    issues = outcome.get("issue")
    if not isinstance(issues, list):
        sys.exit("FAIL: missing issue array for %s" % profile)
    if not issues:
        sys.exit("FAIL: empty issue array for %s" % profile)

    terminology = 0
    other = 0
    warnings = 0
    information = 0
    open_slice_information = 0
    for index, issue in enumerate(issues):
        if not isinstance(issue, dict):
            fail_issue("malformed issue", profile, index, issue)
        severity = issue.get("severity")
        code = issue.get("code")
        if severity not in SEVERITIES or code not in ISSUE_TYPES:
            fail_issue("invalid issue severity or code", profile, index, issue)
        diagnostic = issue.get("diagnostics", "")
        details = issue.get("details", {})
        if not isinstance(diagnostic, str) or not isinstance(details, dict):
            fail_issue("malformed issue fields", profile, index, issue)
        text = details.get("text", "")
        if not isinstance(text, str):
            fail_issue("malformed issue details", profile, index, issue)
        codes = []
        codings = details.get("coding", [])
        if not isinstance(codings, list):
            fail_issue("malformed issue coding", profile, index, issue)
        for coding in codings:
            if isinstance(coding, dict) and isinstance(coding.get("code"), str):
                codes.append(coding["code"])
        combined = " ".join([diagnostic, text, *codes]).lower()
        if "failed to retrieve profile" in combined or (
            profile.lower() in combined
            and ("unable to resolve" in combined or "could not resolve" in combined)
        ):
            fail_issue("profile not resolved", profile, index, issue)
        if "slicing" in combined or "discriminator" in combined:
            if known_pas_open_slice_information(issue, profile):
                open_slice_information += 1
                continue
            fail_issue("slicing/discriminator failure", profile, index, issue)
        if severity in ("error", "fatal"):
            if terminology_error(diagnostic):
                terminology += 1
            else:
                other += 1
        elif severity == "warning":
            warnings += 1
        elif severity == "information":
            information += 1
    print(
        "profile_characterization profile=%s terminology_errors=%d other_errors=%d warnings=%d information=%d open_slice_information=%d"
        % (profile, terminology, other, warnings, information, open_slice_information)
    )


if __name__ == "__main__":
    main()
