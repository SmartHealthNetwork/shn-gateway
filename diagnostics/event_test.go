package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestFingerprintRequiresFullBodyAndExactQuery(t *testing.T) {
	a := RequestFingerprint{Algorithm: "sha256-body-v1", Method: "POST", RequestURI: "/Claim/$submit?a=1&b=2", BodySHA256: "digest-a", ObservedBytes: 100, Complete: true}
	b := a
	if !FingerprintLink(a, b) {
		t.Fatal("complete equal requests did not match")
	}
	b.RequestURI = "/Claim/$submit?b=2&a=1"
	if FingerprintLink(a, b) {
		t.Fatal("different query matched")
	}
	b = a
	b.BodySHA256 = "digest-b"
	if FingerprintLink(a, b) {
		t.Fatal("different body matched")
	}
	b = a
	b.Complete = false
	if FingerprintLink(a, b) {
		t.Fatal("partial body matched")
	}
}

func TestSignUsesRawBodyBytes(t *testing.T) {
	a := Sign([]byte("synthetic-key"), "door", "boot", "2026-09-18T12:00:00Z", []byte(`{"a":1}`))
	b := Sign([]byte("synthetic-key"), "door", "boot", "2026-09-18T12:00:00Z", []byte("{ \"a\" : 1 }"))
	if a == b {
		t.Fatal("signature ignored raw representation")
	}
}

func TestTraceProofBindsExactURIAndExpires(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 123, time.UTC)
	proof := TraceProof([]byte("synthetic-key"), "call-1", "POST", "/a%2Fb?x=1&x=2", now)
	if id, ok := VerifyTraceProof([]byte("synthetic-key"), proof, "POST", "/a%2Fb?x=1&x=2", now.Add(5*time.Minute)); !ok || id != "call-1" {
		t.Fatalf("got %q %v", id, ok)
	}
	if _, ok := VerifyTraceProof([]byte("synthetic-key"), proof, "POST", "/a/b?x=1&x=2", now); ok {
		t.Fatal("decoded path matched")
	}
	if _, ok := VerifyTraceProof([]byte("synthetic-key"), proof, "POST", "/a%2Fb?x=1&x=2", now.Add(5*time.Minute+time.Nanosecond)); ok {
		t.Fatal("expired proof matched")
	}
	if _, ok := VerifyTraceProof([]byte("wrong"), proof, "POST", "/a%2Fb?x=1&x=2", now); ok {
		t.Fatal("wrong key matched")
	}
}

func TestEmptyDigestDefinition(t *testing.T) {
	s := sha256.Sum256(nil)
	if got := hex.EncodeToString(s[:]); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal(got)
	}
}
