#!/usr/bin/env python3
"""Assertions shared by the cold-image and controlled-process verifiers (FR-G3)."""
import json
import re
import sys

ROWS = [
    "init-pas-request-bundle",
    "init-dtr-questionnaireresponse",
    "init-pdex-explanationofbenefit",
    "init-cdex-task",
    "prime-versioned-approved",
    "prime-versioned-denied",
    "prime-versioned-pended",
    "prime-unversioned-approved",
    "prime-unversioned-denied",
    "prime-unversioned-pended",
    "prime-meta-approved",
    "prime-meta-denied",
    "prime-meta-pended",
    "qualify-1-versioned-approved",
    "qualify-1-versioned-denied",
    "qualify-1-versioned-pended",
    "qualify-1-unversioned-approved",
    "qualify-1-unversioned-denied",
    "qualify-1-unversioned-pended",
    "qualify-1-meta-approved",
    "qualify-1-meta-denied",
    "qualify-1-meta-pended",
    "qualify-2-versioned-approved",
    "qualify-2-versioned-denied",
    "qualify-2-versioned-pended",
    "qualify-2-unversioned-approved",
    "qualify-2-unversioned-denied",
    "qualify-2-unversioned-pended",
    "qualify-2-meta-approved",
    "qualify-2-meta-denied",
    "qualify-2-meta-pended",
    "negative-versioned",
    "negative-unversioned",
    "negative-meta",
    "full-response-positive",
    "full-response-negative-hcpcs",
    "full-response-negative-pos",
    "full-response-negative-encounter",
]
JAVA = ["java", "--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher"]
PAS_VERSIONS = {"2.0": "2.0.1", "2.1": "2.1.0", "2.2": "2.2.1"}


def image(base, built, line):
    b, i = base[0]["Config"], built[0]["Config"]
    assert b["Entrypoint"] == JAVA, "pinned base argv changed: review required"
    assert i["Entrypoint"] == ["/healthcheck", "supervise"] + JAVA, "image does not supervise exact base argv"
    assert b["User"] == i["User"] == "65532:65532", "base user changed"
    assert b["WorkingDir"] == i["WorkingDir"] == "/app", "base workdir changed"
    assert not b.get("Cmd") and not i.get("Cmd"), "unexpected appended command"
    base_env = dict(x.split("=", 1) for x in b["Env"])
    image_env = dict(x.split("=", 1) for x in i["Env"])
    assert all(image_env.get(k) == v for k, v in base_env.items()), "base environment changed"
    assert image_env["SHN_IG_LINE"] == line, "wrong IG line"
    assert image_env["hapi.fhir.implementationguides.pas.version"] == PAS_VERSIONS[line], "wrong PAS version"


def ready(state, line):
    assert state["schema"] == 2 and state["line"] == line
    assert state["state"] == "ready" and state["warm"] == ROWS
    assert state.get("key") and ":" in state["key"]
    assert not state.get("row") and not state.get("failure")
    assert state.get("fired_at", "0001-01-01T00:00:00Z") == "0001-01-01T00:00:00Z"


def logs(text, line):
    records = re.findall(r"^warmup: line=(\S+) row=(\S+) elapsed=(\S+) outcome=(\S+)(?: reason=.*)?$", text, re.M)
    assert records, "no warm-up records"
    assert [(r[1], r[3]) for r in records] == [(row, outcome) for row in ROWS for outcome in ("started", "completed")], "warm-up missing, repeated, out of order, or failed"
    for observed_line, row, elapsed, outcome in records:
        assert observed_line == line
        # Go time.Duration formatting, rounded to milliseconds in production logs.
        matches = re.findall(r"([0-9.]+)(h|ms|m|s|µs|ns)", elapsed)
        assert "".join(n + u for n, u in matches) == elapsed, "invalid elapsed time"
        seconds = sum(float(n) * {"h": 3600, "m": 60, "s": 1, "ms": .001, "µs": .000001, "ns": .000000001}[u] for n, u in matches)
        assert 0 <= seconds < 600, "row exceeded startup budget"
        if outcome == "completed":
            print(f"warm-up line={line} row={row} elapsed={elapsed}")


if __name__ == "__main__":
    action = sys.argv[1]
    if action == "image":
        image(json.load(open(sys.argv[2])), json.load(open(sys.argv[3])), sys.argv[4])
    elif action == "ready":
        ready(json.load(sys.stdin), sys.argv[2])
    elif action == "logs":
        logs(sys.stdin.read(), sys.argv[2])
    else:
        raise SystemExit("unknown assertion")
