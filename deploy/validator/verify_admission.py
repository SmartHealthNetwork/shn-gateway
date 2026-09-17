"""Assert the real Java/public listener boundary from kernel process evidence."""
import ipaddress
import json
from pathlib import Path
import sys

JAVA = ["java", "--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher"]
SUFFIX = ["--server.address=127.0.0.1", "--server.port=18080"]


def listeners(raw):
    for line in raw.splitlines():
        fields = line.split()
        if not fields or fields[0] == "sl":
            continue
        if fields[3] != "0A":
            continue
        address, port = fields[1].split(":")
        packed = b"".join(bytes.fromhex(address[i:i+8])[::-1] for i in range(0, len(address), 8))
        ip = ipaddress.ip_address(packed)
        if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped:
            ip = ip.ipv4_mapped
        yield str(ip), int(port, 16), fields[9]


def verify_runtime(record):
    assert record["child_pid"] > 1, "Java is not an owned child"
    assert record["argv"] == JAVA + SUFFIX, "actual Java argv differs from literal prefix plus owned endpoint"
    private, public = [], []
    for address, port, inode in list(listeners(record["tcp"])) + list(listeners(record["tcp6"])):
        if port == 18080:
            assert address == "127.0.0.1", "private backend exposed on a non-owned address"
            assert inode in record["child_sockets"], "private listener not owned by Java"
            private.append((address, inode))
        if port == 8080:
            assert inode in record["parent_sockets"], "public listener not owned by PID1"
            assert inode not in record["child_sockets"], "Java bypasses public admission"
            assert address in ("0.0.0.0", "::"), "public listener is not externally bound"
            public.append((address, inode))
    assert len(private) == 1, "missing or duplicate private backend"
    assert public, "public listener unavailable"


def verify_metadata(record, expected=None):
    assert isinstance(record, dict) and record.get("resourceType") == "CapabilityStatement", "metadata is not a CapabilityStatement"
    assert ":18080" not in json.dumps(record), "private backend URL leaked in metadata"
    implementation = record.get("implementation")
    assert isinstance(implementation, dict), "metadata implementation missing"
    base = implementation.get("url")
    assert isinstance(base, str) and base, "metadata implementation URL missing"
    if expected is not None:
        assert base == expected, "metadata authority changed or reused a cached authority"
    return base


def verify_forwarded_metadata(public, private):
    assert verify_metadata(public) == verify_metadata(private), "forwarded-header/base behavior changed"


def verify_peer_refusal(code, stderr, public):
    verify_metadata(public, "http://localhost:8080/fhir")
    assert code == 7, "private probe did not fail to connect (DNS, HTTP and timeout failures are not refusal)"
    assert any("port 18080" in line and "Connection refused" in line for line in stderr.splitlines()), "private probe lacks concrete connection refusal"


def verify_location_response(raw):
    # curl -D - writes headers and decoded response body to the host stdout file.
    # No host filesystem path is passed to the curl container.
    while True:
        headers, separator, body = raw.partition(b"\r\n\r\n")
        assert separator, "missing complete response headers"
        lines = headers.split(b"\r\n")
        status = lines[0].split()
        assert len(status) >= 2 and status[0].startswith(b"HTTP/"), "invalid HTTP status"
        if status[1] == b"100":
            raw = body
            continue
        assert status[1] == b"201", "synthetic create did not succeed"
        locations = [line.split(b":", 1)[1].strip() for line in lines[1:] if line.lower().startswith(b"location:")]
        assert len(locations) == 1 and locations[0].startswith(b"http://original.example:18089/fhir/Patient/"), "external Location changed"
        assert b":18080" not in locations[0], "private backend URL leaked"
        assert json.loads(body)["resourceType"] == "Patient", "synthetic create response changed"
        return


if __name__ == "__main__":
    if len(sys.argv) == 1:
        verify_runtime(json.load(sys.stdin))
        print("actual Java loopback and PID1 public socket ownership verified (IPv4 and IPv6 tables)")
    elif sys.argv[1] == "peer" and len(sys.argv) == 5:
        verify_peer_refusal(int(sys.argv[2]), Path(sys.argv[3]).read_text(), json.loads(Path(sys.argv[4]).read_text()))
        print("same-peer public FHIR and private connection refusal verified")
    elif sys.argv[1] == "metadata" and len(sys.argv) in (3, 4):
        expected = sys.argv[3] if len(sys.argv) == 4 else None
        verify_metadata(json.loads(Path(sys.argv[2]).read_text()), expected)
        print("metadata authority and complete private-address absence verified")
    elif sys.argv[1] == "forwarded" and len(sys.argv) == 4:
        verify_forwarded_metadata(*(json.loads(Path(p).read_text()) for p in sys.argv[2:]))
        print("fresh public/private forwarded-header base equivalence verified")
    elif sys.argv[1] == "location" and len(sys.argv) == 3:
        verify_location_response(Path(sys.argv[2]).read_bytes())
        print("host-captured public Location and synthetic response verified")
    else:
        raise SystemExit("invalid verification command")
