#!/usr/bin/env python3
"""Scan a lane's log for HAPI's "Found multiple package versions" lines.

usage: verify-collisions.py LINE   (stdin: docker logs)

Every canonical HAPI names must be in known-collisions.json for LINE — the
canonicals two loaded packages both carry, each same-version identical or
tolerated with a reason in the manifest (tools/igclosuregen writes the file
from the committed closure inventories). Any other canonical is the
nondeterministic resolution the closure gate exists to exclude, observed at
runtime, and fails the proof. A built image's warm-up may legitimately log no
such line at all (the 2.0/2.1/2.2 images log none), so PATTERN is proven
against a verbatim line captured from the engine in test_verify_collisions.py
rather than against live output; an engine that rewords the line is caught
there when PATTERN is updated against a new capture.
"""
import json
import pathlib
import re
import sys

PATTERN = re.compile(r"Found multiple package versions for FHIR version: \S+ and canonical URL: (\S+)")


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: verify-collisions.py LINE")
    line = sys.argv[1]
    known = json.loads((pathlib.Path(__file__).parent / "known-collisions.json").read_text())
    tolerated = set(known["lines"].get(line, []))
    seen = {}
    for text in sys.stdin:
        match = PATTERN.search(text)
        if match:
            seen[match.group(1)] = seen.get(match.group(1), 0) + 1
    unexpected = sorted(url for url in seen if url not in tolerated)
    if unexpected:
        sys.exit("FAIL: line %s: HAPI reports multiple package versions for canonicals outside the closure inventory's collision list: %s" % (line, ", ".join(unexpected)))
    print("OK: line %s: multiple-package-version log lines: %d hit(s) on %d known canonical(s), 0 unexpected" % (line, sum(seen.values()), len(seen)))


if __name__ == "__main__":
    main()
