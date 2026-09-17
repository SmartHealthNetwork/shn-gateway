import io
import json
import subprocess
import sys
import unittest
from pathlib import Path

SCRIPT = Path(__file__).parent / "verify-witness.py"


def run(mode, outcome):
    return subprocess.run([sys.executable, str(SCRIPT), mode, "label"], input=json.dumps(outcome), capture_output=True, text=True)


class WitnessTests(unittest.TestCase):
    clean = {"resourceType": "OperationOutcome", "issue": [{"severity": "warning", "code": "processing", "diagnostics": "w"}]}
    unknown = {"resourceType": "OperationOutcome", "issue": [
        {"severity": "error", "code": "processing", "diagnostics": "Profile reference 'x|7.0.0' has not been checked because it could not be found, and the validator is set to not fetch unknown profiles"},
        {"severity": "error", "code": "processing", "diagnostics": "Invalid profile. Failed to retrieve profile with url=x|7.0.0"}]}
    other_error = {"resourceType": "OperationOutcome", "issue": [{"severity": "error", "code": "processing", "diagnostics": "Claim.identifier: minimum required = 1"}]}

    def test_clean_accepts_only_error_free(self):
        self.assertEqual(run("clean", self.clean).returncode, 0)
        self.assertNotEqual(run("clean", self.unknown).returncode, 0)
        self.assertNotEqual(run("clean", self.other_error).returncode, 0)

    def test_unknown_profile_requires_the_not_found_error(self):
        self.assertEqual(run("unknown-profile", self.unknown).returncode, 0)
        self.assertNotEqual(run("unknown-profile", self.clean).returncode, 0, "a clean outcome is not a refusal")
        self.assertNotEqual(run("unknown-profile", self.other_error).returncode, 0, "a different error is not the exact-version refusal")

    typed = {"resourceType": "OperationOutcome", "issue": [{"severity": "error", "code": "processing", "diagnostics": "The Extension 'http://hl7.org/fhir/StructureDefinition/alternate-reference' definition allows for the types [Reference] but found type string"}]}

    def test_type_refused_requires_the_type_rejection(self):
        self.assertEqual(run("type-refused", self.typed).returncode, 0)
        self.assertNotEqual(run("type-refused", self.clean).returncode, 0, "a clean outcome is not a type rejection")
        self.assertNotEqual(run("type-refused", self.unknown).returncode, 0, "an unknown-profile error is not a type rejection")
        self.assertNotEqual(run("type-refused", self.other_error).returncode, 0)

    def test_malformed(self):
        for body in ("not json", json.dumps({"resourceType": "Bundle"}), json.dumps({"resourceType": "OperationOutcome"})):
            self.assertNotEqual(subprocess.run([sys.executable, str(SCRIPT), "clean", "l"], input=body, capture_output=True, text=True).returncode, 0)


if __name__ == "__main__":
    unittest.main()
