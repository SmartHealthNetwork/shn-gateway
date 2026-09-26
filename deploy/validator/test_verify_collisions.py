import json
import subprocess
import sys
import unittest
from pathlib import Path

DIR = Path(__file__).parent
SCRIPT = DIR / "verify-collisions.py"
LINE = "Found multiple package versions for FHIR version: R4 and canonical URL: %s"


# Verbatim from a 2.2 make-validate lane (HAPI 8.10.0, 2026-09-11): the line PATTERN must match.
# The canonical it names is a known 2.2 collision again since the support package carries the
# artifact-versionAlgorithm closure (the version-algorithm CodeSystem, byte-identical to the
# extensions package's own entry), which makes the verbatim line the acceptance row as captured;
# the rejection row substitutes a canonical the inventory does not record.
CAPTURED = "2026-09-11T15:18:34.546Z  WARN 1 --- [hapi-fhir-jpaserver-starter] [nio-8080-exec-9] c.uhn.fhir.jpa.packages.JpaPackageCache  : Found multiple package versions for FHIR version: R4 and canonical URL: http://hl7.org/fhir/version-algorithm"
CAPTURED_URL = "http://hl7.org/fhir/version-algorithm"


def run(line, log):
    return subprocess.run([sys.executable, str(SCRIPT), line], input=log, capture_output=True, text=True)


class CollisionScanTests(unittest.TestCase):
    known = json.loads((DIR / "known-collisions.json").read_text())["lines"]

    def test_known_canonicals_pass_and_unknown_fail(self):
        with_collisions = {line: urls for line, urls in self.known.items() if urls}
        self.assertTrue(with_collisions, "no line records a known collision; the acceptance half of this row has nothing to exercise")
        for line, urls in with_collisions.items():
            log = "\n".join("WARN c.uhn.fhir.jpa.packages.JpaPackageCache : " + LINE % u for u in urls[:2]) + "\n"
            self.assertEqual(run(line, log).returncode, 0, run(line, log).stderr)
            bad = log + LINE % "http://example.org/not-in-the-inventory" + "\n"
            result = run(line, bad)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("not-in-the-inventory", result.stderr)

    def test_a_line_with_no_known_collisions_refuses_any_hit(self):
        # 2.0 and 2.1 load every canonical from exactly one package; a multiple-versions
        # line there is the nondeterminism the closure gate excludes, whatever it names.
        empty = [line for line, urls in self.known.items() if not urls]
        self.assertTrue(empty, "every line records a collision; the rejection half of this row has nothing to exercise")
        for line in empty:
            result = run(line, LINE % self.known["2.2"][0] + "\n")
            self.assertNotEqual(result.returncode, 0, line)
            self.assertIn(self.known["2.2"][0], result.stderr)

    def test_no_hits_is_clean_and_other_lines_ignored(self):
        self.assertEqual(run("2.0", "INFO nothing to see\n").returncode, 0)

    def test_pattern_matches_the_captured_engine_line(self):
        self.assertIn(CAPTURED_URL, self.known["2.2"], "the captured canonical is a recorded 2.2 collision today; if it leaves the inventory, swap the roles below")
        # the verbatim engine line names a recorded collision: green, counted
        result = run("2.2", CAPTURED + "\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("1 hit(s) on 1 known canonical(s)", result.stdout)
        # the same engine line naming a canonical outside the inventory: red, naming it
        unknown = "http://example.org/not-in-the-inventory"
        self.assertNotIn(unknown, self.known["2.2"])
        result = run("2.2", CAPTURED.replace(CAPTURED_URL, unknown) + "\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(unknown, result.stderr)

    def test_a_known_canonical_of_another_line_is_not_known_here(self):
        url = self.known["2.2"][0]
        self.assertEqual(run("2.2", LINE % url + "\n").returncode, 0)
        result = run("9.9", LINE % url + "\n")
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
