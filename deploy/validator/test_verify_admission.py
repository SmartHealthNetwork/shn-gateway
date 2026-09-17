"""Rejection rows for actual image listener/argv evidence, independent of labels."""
import copy
import unittest
from verify_admission import verify_runtime, verify_peer_refusal, verify_location_response, verify_metadata, verify_forwarded_metadata, JAVA


def row(address, port, inode):
    return f" 0: {address}:{port:04X} 00000000:0000 0A 0:0 00:0 0 65532 0 {inode}"


class AdmissionTests(unittest.TestCase):
    def test_peer_refusal_requires_connection_refused_and_public_fhir(self):
        public = {"resourceType": "CapabilityStatement", "implementation": {"url": "http://localhost:8080/fhir"}}
        refused = "* connect to 172.18.0.2 port 18080 failed: Connection refused\n"
        verify_peer_refusal(7, refused, public)
        for code, error, ready in [(0, refused, public), (22, refused, public), (6, "Could not resolve host", public), (28, "Connection timed out", public), (7, "Network unreachable", public), (7, refused, {"resourceType": "OperationOutcome"}), (7, refused.replace("18080", "8080"), public)]:
            with self.subTest(code=code, error=error, ready=ready):
                with self.assertRaises(AssertionError):
                    verify_peer_refusal(code, error, ready)

    def test_location_is_split_from_host_captured_stream(self):
        valid = b'HTTP/1.1 201 Created\r\nLocation: http://original.example:18089/fhir/Patient/id/_history/1\r\nContent-Type: application/fhir+json\r\n\r\n{"resourceType":"Patient","id":"id"}'
        verify_location_response(valid)
        for changed in [valid.replace(b'201 Created', b'500 Error'), valid.replace(b'original.example:18089', b'127.0.0.1:18080'), valid.replace(b'Location:', b'Other:'), valid.replace(b'Patient"', b'OperationOutcome"'), valid.split(b'\r\n\r\n')[0]]:
            with self.subTest(changed=changed):
                with self.assertRaises((AssertionError, ValueError)):
                    verify_location_response(changed)

    def valid(self):
        return {"argv": JAVA + ["--server.address=127.0.0.1", "--server.port=18080"],
                "child_pid": 8, "child_sockets": ["22"], "parent_sockets": ["11"],
                "tcp": row("00000000", 8080, 11) + "\n" + row("0100007F", 18080, 22), "tcp6": ""}

    def test_exact_private_and_public_owners(self):
        verify_runtime(self.valid())
        mapped = self.valid()
        mapped["tcp"] = row("00000000", 8080, 11)
        mapped["tcp6"] = row("0000000000000000FFFF00000100007F", 18080, 22)
        verify_runtime(mapped)  # Linux may represent an IPv4 loopback socket as mapped IPv6.

    def test_rejection_rows(self):
        changes = [
            {"argv": JAVA}, {"argv": JAVA + ["--server.port=18080"] * 2},
            {"argv": ["different"] + JAVA[1:] + ["--server.address=127.0.0.1", "--server.port=18080"]},
            {"child_pid": 1}, {"child_sockets": []}, {"parent_sockets": []},
            {"tcp": row("00000000", 8080, 11) + "\n" + row("00000000", 18080, 22)},
            {"tcp6": row("00000000000000000000000000000000", 18080, 22)},
            {"tcp6": row("00000000000000000000000001000000", 18080, 22)},
            {"tcp": row("00000000", 8080, 11)},
            {"tcp": row("00000000", 8080, 22) + "\n" + row("0100007F", 18080, 22)},
        ]
        for change in changes:
            with self.subTest(change=change):
                candidate = copy.deepcopy(self.valid()); candidate.update(change)
                with self.assertRaises((AssertionError, ValueError)):
                    verify_runtime(candidate)

class OrdinaryMetadataTests(unittest.TestCase):
    def test_cold_peer_metadata_rejects_private_url_anywhere(self):
        public = {"resourceType": "CapabilityStatement", "implementation": {"url": "http://localhost:8080/fhir"}}
        refused = "connect to 172.18.0.2 port 18080 failed: Connection refused"
        verify_peer_refusal(7, refused, public)
        for changed in [
            {"resourceType": "CapabilityStatement", "implementation": {"url": "http://127.0.0.1:18080/fhir"}},
            dict(public, rest=[{"operation": [{"definition": "http://127.0.0.1:18080/fhir/OperationDefinition/a"}]}]),
            {"resourceType": "CapabilityStatement", "implementation": {"url": "http://wrong.example:8080/fhir"}},
        ]:
            with self.subTest(changed=changed):
                with self.assertRaises(AssertionError):
                    verify_peer_refusal(7, refused, changed)

class MetadataDerivationTests(unittest.TestCase):
    def statement(self, base):
        return {"resourceType": "CapabilityStatement", "implementation": {"url": base}}

    def test_late_ordinary_preserves_cached_public_authority_but_rejects_private(self):
        for base in ["http://localhost:8080/fhir", "http://original.example:18089/fhir", "http://alternate.example:18090/fhir"]:
            verify_metadata(self.statement(base))
        for changed in [self.statement("http://127.0.0.1:18080/fhir"),
                        dict(self.statement("http://localhost:8080/fhir"), rest=[{"definition": "http://127.0.0.1:18080/other"}])]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                verify_metadata(changed)

    def test_fresh_authorities_reject_cached_hardcoded_private_and_corrupted_host(self):
        hosts = ["http://original.example:18089/fhir", "http://alternate.example:18090/fhir"]
        for expected in hosts:
            verify_metadata(self.statement(expected), expected)
            for wrong in hosts + ["http://localhost:8080/fhir", "http://127.0.0.1:18080/fhir", expected.replace(":180", ":190")]:
                if wrong != expected:
                    with self.subTest(expected=expected, wrong=wrong), self.assertRaises(AssertionError):
                        verify_metadata(self.statement(wrong), expected)

    def test_metadata_shape_refusals(self):
        for changed in [None, [], {}, {"resourceType": "Patient"},
                        {"resourceType": "CapabilityStatement"},
                        {"resourceType": "CapabilityStatement", "implementation": []},
                        self.statement(None), self.statement(""), self.statement(8080)]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                verify_metadata(changed)

    def test_forwarded_comparison_checks_both_complete_statements(self):
        public = self.statement("http://original.example:18089/fhir")
        verify_forwarded_metadata(public, copy.deepcopy(public))
        for changed in [self.statement("https://forwarded.example:18443/fhir"),
                        dict(public, rest=[{"definition": "http://127.0.0.1:18080/other"}])]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                verify_forwarded_metadata(public, changed)
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                verify_forwarded_metadata(changed, public)

    def test_location_wrong_authority_resource_or_duplicate_header_refuses(self):
        valid = b'HTTP/1.1 201 Created\r\nLocation: http://original.example:18089/fhir/Patient/id/_history/1\r\n\r\n{"resourceType":"Patient","id":"id"}'
        verify_location_response(valid)
        for changed in [valid.replace(b'original.example:18089', b'alternate.example:18090'),
                        valid.replace(b'/fhir/Patient/', b'/fhir/Claim/'),
                        valid.replace(b'Location:', b'Location: http://original.example:18089/fhir/Patient/other\r\nLocation:')]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                verify_location_response(changed)


if __name__ == "__main__":
    unittest.main()
