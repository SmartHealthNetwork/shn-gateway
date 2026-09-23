// Package diagnostics provides optional, bounded observation of gateway HTTP
// traffic. Failure or overload in this package must never affect exchange traffic.
package diagnostics

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Identity struct {
	VerifiedClientID   string `json:"verifiedClientId,omitempty"`
	ClaimedClientID    string `json:"claimedClientId,omitempty"`
	RegisteredClientID string `json:"registeredClientId,omitempty"`
	AuthResult         string `json:"authResult,omitempty"`
}

type Event struct {
	bodyBudget            BodyBudget
	Source                string             `json:"source"`
	Incarnation           string             `json:"incarnation"`
	Sequence              uint64             `json:"sequence"`
	Time                  time.Time          `json:"time"`
	Kind                  string             `json:"kind"`
	CallID                string             `json:"callId,omitempty"`
	CorrelationID         string             `json:"correlationId,omitempty"`
	RequestCiphertextHash string             `json:"requestCiphertextHash,omitempty"`
	Sender                string             `json:"sender,omitempty"`
	Recipient             string             `json:"recipient,omitempty"`
	LegType               string             `json:"legType,omitempty"`
	ContractLine          string             `json:"contractLine,omitempty"`
	Method                string             `json:"method,omitempty"`
	URL                   string             `json:"url,omitempty"`
	Headers               http.Header        `json:"headers,omitempty"`
	HeadersComplete       bool               `json:"headersComplete"`
	Body                  []byte             `json:"body,omitempty"`
	BodyComplete          bool               `json:"bodyComplete"`
	RequestFingerprint    RequestFingerprint `json:"requestFingerprint"`
	Status                int                `json:"status,omitempty"`
	DurationNanos         int64              `json:"durationNanos,omitempty"`
	Identity              Identity           `json:"identity"`
	Detail                string             `json:"detail,omitempty"`
}

type RequestFingerprint struct {
	Algorithm     string `json:"algorithm"`
	Method        string `json:"method"`
	RequestURI    string `json:"requestURI"`
	BodySHA256    string `json:"bodySHA256"`
	ObservedBytes int64  `json:"observedBytes"`
	Complete      bool   `json:"complete"`
}

func FingerprintLink(a, b RequestFingerprint) bool {
	return a.Complete && b.Complete && a.Algorithm == "sha256-body-v1" &&
		b.Algorithm == a.Algorithm && a.Method == b.Method && a.RequestURI == b.RequestURI &&
		a.ObservedBytes == b.ObservedBytes && a.BodySHA256 != "" && a.BodySHA256 == b.BodySHA256
}

type Health struct {
	Closed           bool      `json:"closed,omitempty"`
	Source           string    `json:"source"`
	Incarnation      string    `json:"incarnation"`
	Time             time.Time `json:"time"`
	LastSequence     uint64    `json:"lastSequence"`
	LastAcknowledged uint64    `json:"lastAcknowledged"`
	Pending          uint64    `json:"pending"`
	Dropped          uint64    `json:"dropped"`
	State            string    `json:"state"`
}

func Sign(key []byte, source, incarnation, timestamp string, body []byte) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%x", source, incarnation, timestamp, sum)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type traceDocument struct {
	CallID     string `json:"callId"`
	Method     string `json:"method"`
	RequestURI string `json:"requestURI"`
	IssuedAt   string `json:"issuedAt"`
	Signature  string `json:"signature,omitempty"`
}

func TraceProof(key []byte, callID, method, requestURI string, now time.Time) string {
	d := traceDocument{CallID: callID, Method: method, RequestURI: requestURI, IssuedAt: now.UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(d)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	d.Signature = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	raw, _ = json.Marshal(d)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func VerifyTraceProof(key []byte, proof, method, requestURI string, now time.Time) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(proof)
	if err != nil {
		return "", false
	}
	var d traceDocument
	if json.Unmarshal(raw, &d) != nil || d.CallID == "" || d.Method != method || d.RequestURI != requestURI {
		return "", false
	}
	issued, err := time.Parse(time.RFC3339Nano, d.IssuedAt)
	if err != nil || now.Before(issued) || now.Sub(issued) > 5*time.Minute {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(d.Signature)
	if err != nil {
		return "", false
	}
	d.Signature = ""
	unsigned, _ := json.Marshal(d)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(unsigned)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	return d.CallID, true
}
