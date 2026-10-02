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

// RequestFingerprint identifies a request's body by a hash of the bytes the
// observer saw, so the two ends of one forward can be matched without
// comparing bodies.
type RequestFingerprint struct {
	Algorithm     string `json:"algorithm"`
	Method        string `json:"method"`
	RequestURI    string `json:"requestURI"`
	BodySHA256    string `json:"bodySHA256"`
	ObservedBytes int64  `json:"observedBytes"`
	// Complete reports that the hash saw every byte of the body, whether or
	// not the capture budget kept the body itself (Event.BodyComplete says
	// that). A handler that stopped reading, a read that failed, or a body
	// cut off leaves it false.
	Complete bool `json:"complete"`
}

// FingerprintLink reports whether a and b are the same request body: both
// complete, by the same algorithm, method and URI, over the same bytes.
func FingerprintLink(a, b RequestFingerprint) bool {
	return a.Complete && b.Complete && a.Algorithm == "sha256-body-v1" &&
		b.Algorithm == a.Algorithm && a.Method == b.Method && a.RequestURI == b.RequestURI &&
		a.ObservedBytes == b.ObservedBytes && a.BodySHA256 != "" && a.BodySHA256 == b.BodySHA256
}

type Health struct {
	Source           string    `json:"source"`
	Incarnation      string    `json:"incarnation"`
	Time             time.Time `json:"time"`
	LastSequence     uint64    `json:"lastSequence"`
	LastAcknowledged uint64    `json:"lastAcknowledged"`
	Pending          uint64    `json:"pending"`
	Dropped          uint64    `json:"dropped"`
	State            string    `json:"state"`
	// The counts account for every sequenced event: each is acknowledged,
	// discarded (the ingest declined it as out of scope, or the publisher
	// withheld its kind), dropped (for a reason in DroppedBy) or still
	// pending. LastAcknowledged is only the
	// highest sequence acknowledged, so a declined event leaves it behind
	// LastSequence. Empty counts are omitted: a publisher that does
	// not keep them sends exactly what it sent before. The accounts ingest
	// decodes a heartbeat strictly, so a publisher may send the counts only
	// to an ingest that knows them, and an ingest must not be rolled back
	// below them while a publisher sends them.
	Acknowledged uint64     `json:"acknowledged,omitempty"`
	Discarded    uint64     `json:"discarded,omitempty"`
	DroppedBy    DropCounts `json:"droppedBy,omitzero"`
	// Suppressed counts the discarded events the publisher never sent: their
	// kind is one the ingest said it does not admit from this publisher
	// (kind_not_admitted). It is part of Discarded.
	Suppressed uint64 `json:"suppressed,omitempty"`
}

// DropCounts are a publisher's drops by why: its queue was full when the
// event was emitted (or, still undelivered, it was shed from a full queue to
// admit an access event), its ownership window expired, it was too large to
// publish, it could not be encoded, it was a test event that was not
// accepted, the publisher stopped with it undelivered, or the ingest refused
// it for good as invalid or as conflicting with an event it already holds.
type DropCounts struct {
	QueueFull   uint64 `json:"queueFull,omitempty"`
	Expired     uint64 `json:"expired,omitempty"`
	Oversized   uint64 `json:"oversized,omitempty"`
	Unencodable uint64 `json:"unencodable,omitempty"`
	Test        uint64 `json:"test,omitempty"`
	Stopped     uint64 `json:"stopped,omitempty"`
	Invalid     uint64 `json:"invalid,omitempty"`
}

// Counted reports whether h carries the counts.
func (h Health) Counted() bool {
	return h.Acknowledged != 0 || h.Discarded != 0 || h.DroppedBy != (DropCounts{})
}

// sum adds counts, reporting false on overflow.
func sum(counts ...uint64) (uint64, bool) {
	var total uint64
	for _, n := range counts {
		if total+n < total {
			return 0, false
		}
		total += n
	}
	return total, true
}

// CountsAgree reports whether h's counts, when it carries them, account for
// every sequenced event exactly once: acknowledged + discarded + dropped +
// pending == lastSequence, with dropped split by reason, and acknowledged
// consistent with the highest sequence acknowledged.
func (h Health) CountsAgree() bool {
	if !h.Counted() {
		return h.Suppressed == 0
	}
	if h.Suppressed > h.Discarded {
		return false
	}
	d := h.DroppedBy
	dropped, ok := sum(d.QueueFull, d.Expired, d.Oversized, d.Unencodable, d.Test, d.Stopped, d.Invalid)
	if !ok || dropped != h.Dropped {
		return false
	}
	// Sequences are unique: n acknowledged events reach at least sequence n,
	// and none acknowledged means none is the highest.
	if h.Acknowledged > h.LastAcknowledged || (h.Acknowledged == 0 && h.LastAcknowledged != 0) {
		return false
	}
	total, ok := sum(h.Acknowledged, h.Discarded, h.Dropped, h.Pending)
	return ok && total == h.LastSequence
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
