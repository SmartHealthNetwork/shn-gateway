#!/usr/bin/env python3
"""Assert one $validate witness outcome of the engine's resolution rules.

usage: verify-witness.py MODE LABEL   (stdin: the OperationOutcome)
  clean            no error/fatal issue — the reference resolved and validated
  unknown-profile  at least one error/fatal issue reporting the profile as not
                   found — an exact-version reference to a version no loaded
                   package carries, refused without fallback
  type-refused     at least one error/fatal issue rejecting a value's type
                   against an extension or profile definition ("... but found
                   type ...") — the definition was resolved and applied
"""
import json
import sys

UNKNOWN = ("could not be found", "failed to retrieve profile")
TYPE_REFUSED = ("but found type",)


def main():
    if len(sys.argv) != 3 or sys.argv[1] not in ("clean", "unknown-profile", "type-refused"):
        sys.exit("usage: verify-witness.py clean|unknown-profile|type-refused LABEL")
    mode, label = sys.argv[1], sys.argv[2]
    try:
        outcome = json.load(sys.stdin)
    except json.JSONDecodeError as error:
        sys.exit("FAIL: %s: malformed OperationOutcome: %s" % (label, error))
    if not isinstance(outcome, dict) or outcome.get("resourceType") != "OperationOutcome":
        sys.exit("FAIL: %s: non-OperationOutcome" % label)
    issues = outcome.get("issue")
    if not isinstance(issues, list):
        sys.exit("FAIL: %s: missing issue array" % label)
    errors = [i for i in issues if isinstance(i, dict) and i.get("severity") in ("error", "fatal")]
    if mode == "clean":
        if errors:
            sys.exit("FAIL: %s: %d error issue(s): %s" % (label, len(errors), json.dumps(errors, sort_keys=True)[:600]))
        print("OK: witness %s: clean (%d issue(s), none error)" % (label, len(issues)))
        return
    if mode == "type-refused":
        typed = [i for i in errors if any(m in str(i.get("diagnostics", "")).lower() for m in TYPE_REFUSED)]
        if not typed:
            sys.exit("FAIL: %s: expected a type rejection, got %d error issue(s): %s" % (label, len(errors), json.dumps(errors, sort_keys=True)[:600]))
        print("OK: witness %s: value type rejected against the resolved definition (%d error issue(s))" % (label, len(errors)))
        return
    unknown = [i for i in errors if any(m in str(i.get("diagnostics", "")).lower() for m in UNKNOWN)]
    if not unknown:
        sys.exit("FAIL: %s: expected an unknown-profile error, got %d error issue(s): %s" % (label, len(errors), json.dumps(errors, sort_keys=True)[:600]))
    print("OK: witness %s: refused as unknown profile (%d error issue(s))" % (label, len(errors)))


if __name__ == "__main__":
    main()
