//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

// evidenceLimitRequest is a request the completion reads, referencing
// Condition/c1, which it reads from the participant's system.
func evidenceLimitRequest() []byte {
	return []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"resourceType":"Patient","id":"p"}},` +
		`{"fullUrl":"https://example.test/fhir/ServiceRequest/sr","resource":{"resourceType":"ServiceRequest","id":"sr","status":"active","intent":"order","subject":{"reference":"Patient/p"},"reasonReference":[{"reference":"Condition/c1"}]}}]}`)
}

// evidenceLimitRecord is Condition/c1 about the request's patient, with note
// as its note's text and extra (a JSON array) as a member of its own.
func evidenceLimitRecord(note, extra string) []byte {
	r := `{"resourceType":"Condition","id":"c1","subject":{"reference":"Patient/p"},"note":[{"text":"` + note + `"}]`
	if extra != "" {
		r += `,"x-extra":` + extra
	}
	return []byte(r + `}`)
}

// completeWith completes evidenceLimitRequest with record as the system's
// answer to its read.
func completeWith(record []byte) ([]byte, error) {
	return completeWithHeld(map[string][]byte{"Condition/c1": record})
}

// completeWithHeld completes evidenceLimitRequest with held as the records the
// system holds, by reference.
func completeWithHeld(held map[string][]byte) ([]byte, error) {
	read := func(_ context.Context, ref string) ([]byte, bool, error) {
		r, ok := held[ref]
		return r, ok, nil
	}
	return retainPASRequestEvidence(context.Background(), evidenceLimitRequest(), read)
}

// conditionAbout is a small Condition id about the request's patient, with
// extra (a JSON array) as a member of its own.
func conditionAbout(id, extra string) []byte {
	r := `{"resourceType":"Condition","id":"` + id + `","subject":{"reference":"Patient/p"}`
	if extra != "" {
		r += `,"x-extra":` + extra
	}
	return []byte(r + `}`)
}

// relayTokens is b's JSON token count by the relay's own rule, the one
// relay/internal/splice's Limits states and its scanner counts: every value and
// every member name is one token. An object or array is a value, so its opening
// delimiter counts once and its closing one not at all; json.Decoder.Token
// yields both, and each member name as a string.
func relayTokens(t *testing.T, b []byte) int {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	n := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return n
		}
		if err != nil {
			t.Fatalf("counting tokens: %v", err)
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			continue
		}
		n++
	}
}

// asCarried is record at its depth in the request, the resource of an entry
// of its entry array, as heldRecordFault reads it against the relay's limits.
func asCarried(record []byte) []byte {
	return []byte(`{"entry":[{"resource":` + string(record) + `}]}`)
}

// wantPastLimit fails unless err refuses Condition/c1 past the gateway's own
// carrying limits, naming limit.
func wantPastLimit(t *testing.T, err error, limit string) {
	t.Helper()
	var past *evidencePastLimitError
	if !errors.As(err, &past) {
		t.Fatalf("err = %v, want Condition/c1 refused past the gateway's own carrying limits", err)
	}
	if past.ref != "Condition/c1" || past.limit != limit {
		t.Fatalf("refused %s past %q, want Condition/c1 past %q", past.ref, past.limit, limit)
	}
	if status, why, ok := evidenceFidelityRefusal(err); !ok || status != 502 || !strings.Contains(why, "Condition/c1") {
		t.Fatalf("answered %d %q (fidelity %v), want 502 naming Condition/c1, the fidelity of the build", status, why, ok)
	}
}

// A record read from the participant's system that takes the request exactly
// to its size bound rides; one byte more is refused naming the record and the
// bound, as the fidelity of the gateway's own build.
func TestEvidence_AtTheRequestSizeBoundRides(t *testing.T) {
	small, err := completeWith(evidenceLimitRecord("x", ""))
	if err != nil {
		t.Fatalf("control refused: %v", err)
	}
	// The note's text adds its bytes to the request one for one.
	pad := strings.Repeat("x", 1+pasGraphMaxBytes-len(small))
	atScaleAt(t, len(pad))
	out, err := completeWith(evidenceLimitRecord(pad, ""))
	if err != nil {
		t.Fatalf("a record taking the request exactly to its bound was refused: %v", err)
	}
	if len(out) != pasGraphMaxBytes {
		t.Fatalf("request is %d bytes, want exactly %d", len(out), pasGraphMaxBytes)
	}
	_, err = completeWith(evidenceLimitRecord(pad+"x", ""))
	wantPastLimit(t, err, limitGraphBytes)
}

// A record that takes the request to the relay's token limit rides; one token
// more is refused naming the record and the relay's limit on the request. A
// record past the relay's token limit alone, as the request carries it, is
// refused naming that limit.
//
// Each element of the record's array is one token, so each edge is found by
// counting a one-element record's tokens and proven by its two probes, the
// largest array that rides and one element more, rather than searched for:
// each probe reads about 1,000,000 tokens.
func TestEvidence_AtTheRelayTokenLimitRides(t *testing.T) {
	t.Parallel()
	atScale(t) // every probe reads about 1,000,000 tokens
	zeros := func(n int) string { return "[" + strings.TrimSuffix(strings.Repeat("0,", n), ",") + "]" }
	one, err := completeWith(evidenceLimitRecord("x", zeros(1)))
	if err != nil {
		t.Fatalf("a one-element record was refused: %v", err)
	}
	m := 1_000_000 - relayTokens(t, one) + 1
	if _, err := completeWith(evidenceLimitRecord("x", zeros(m))); err != nil {
		t.Fatalf("a record taking the request to the relay's 1,000,000 tokens was refused: %v", err)
	}
	_, err = completeWith(evidenceLimitRecord("x", zeros(m+1)))
	wantPastLimit(t, err, "takes the request past the relay's limits: the request holds more than 1,000,000 JSON tokens")
	// The largest array the record alone carries as the request carries it.
	n := 1_000_000 - relayTokens(t, asCarried(evidenceLimitRecord("x", zeros(1)))) + 1
	if n <= m {
		t.Fatalf("the record alone carries %d elements, the request %d: want the record's own limit past the request's", n, m)
	}
	if err := heldRecordFault(evidenceLimitRecord("x", zeros(n)), "Condition/c1"); err != nil {
		t.Fatalf("a record at the relay's 1,000,000 tokens as the request carries it was refused: %v", err)
	}
	_, err = completeWith(evidenceLimitRecord("x", zeros(n+1)))
	wantPastLimit(t, err, "holds more than 1,000,000 JSON tokens as the request carries it")
}

// Records read that together take the request to the relay's token limit
// ride; one token more is refused naming the last record read, though each
// alone is within the relay's limits as the request carries it.
//
// Each element of either record's array is one token, so the edge is found by
// counting the request one element each makes and proven by its two probes
// rather than searched for: each probe reads about 1,000,000 tokens.
func TestEvidence_RecordsTogetherAtTheRelayTokenLimitRide(t *testing.T) {
	t.Parallel()
	atScale(t) // every probe reads about 1,000,000 tokens
	zeros := func(n int) string { return "[" + strings.TrimSuffix(strings.Repeat("0,", n), ",") + "]" }
	const first = 600_000
	held := func(first, n int) map[string][]byte {
		return map[string][]byte{
			"Condition/c1": conditionAbout("c1", `[{"reference":"Condition/c2"},`+strings.TrimPrefix(zeros(first), "[")),
			"Condition/c2": conditionAbout("c2", zeros(n)),
		}
	}
	one, err := completeWithHeld(held(1, 1))
	if err != nil {
		t.Fatalf("one element each was refused: %v", err)
	}
	n := 1_000_000 - relayTokens(t, one) + 1 - (first - 1)
	if _, err := completeWithHeld(held(first, n)); err != nil {
		t.Fatalf("records taking the request to the relay's 1,000,000 tokens together were refused: %v", err)
	}
	_, err = completeWithHeld(held(first, n+1))
	var past *evidencePastLimitError
	if !errors.As(err, &past) || past.ref != "Condition/c2" || past.limit != "takes the request past the relay's limits: the request holds more than 1,000,000 JSON tokens" {
		t.Fatalf("one token past: %v, want Condition/c2 refused past the relay's token limit on the request", err)
	}
	if first+n < 1_000_000-100 || first+n >= 1_000_000 {
		t.Fatalf("the records carry %d elements together, want just under the relay's 1,000,000 tokens", first+n)
	}
}

// A record read whose references take the request to its reference bound
// rides; one reference more is refused naming that record.
func TestEvidence_AtTheRequestReferenceBoundRides(t *testing.T) {
	refs := func(k int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(`{"reference":"Patient/p"},`, k), ",") + "]"
	}
	request := strings.Count(string(evidenceLimitRequest()), `"reference"`)
	own := strings.Count(string(conditionAbout("c1", "")), `"reference"`)
	k := pasGraphMaxReferences - request - own
	if _, err := completeWith(conditionAbout("c1", refs(k))); err != nil {
		t.Fatalf("a record taking the request to %d references was refused: %v", pasGraphMaxReferences, err)
	}
	_, err := completeWith(conditionAbout("c1", refs(k+1)))
	wantPastLimit(t, err, limitGraphRefs)
}

// A record read whose references take the request to its resource bound
// rides; one resource more is refused naming that record.
func TestEvidence_AtTheRequestResourceBoundRides(t *testing.T) {
	held := func(k int) map[string][]byte {
		h := map[string][]byte{}
		var refs []string
		for n := 0; n < k; n++ {
			id := "c" + strconv.Itoa(n+2)
			refs = append(refs, `{"reference":"Condition/`+id+`"}`)
			h["Condition/"+id] = conditionAbout(id, "")
		}
		h["Condition/c1"] = conditionAbout("c1", "["+strings.Join(refs, ",")+"]")
		return h
	}
	// The request holds two entries and Condition/c1.
	k := pasGraphMaxResources - 3
	out, err := completeWithHeld(held(k))
	if err != nil {
		t.Fatalf("records taking the request to %d resources were refused: %v", pasGraphMaxResources, err)
	}
	if n := strings.Count(string(out), `"fullUrl"`); n != pasGraphMaxResources {
		t.Fatalf("the request carries %d resources, want %d", n, pasGraphMaxResources)
	}
	_, err = completeWithHeld(held(k + 1))
	wantPastLimit(t, err, limitGraphResources)
}

// A record read whose contained resources take the request to its resource
// bound rides; one more is refused naming that record. The bound counts every
// resource, contained ones included.
func TestEvidence_ContainedAtTheRequestResourceBoundRides(t *testing.T) {
	contained := func(k int) string {
		var obs []string
		for n := 0; n < k; n++ {
			obs = append(obs, `{"resourceType":"Observation","id":"o`+strconv.Itoa(n)+`","status":"final","code":{"text":"x"}}`)
		}
		return `{"resourceType":"Condition","id":"c1","subject":{"reference":"Patient/p"},"contained":[` + strings.Join(obs, ",") + `]}`
	}
	// The request holds two resources, and Condition/c1 is one more.
	k := pasGraphMaxResources - 3
	if _, err := completeWith([]byte(contained(k))); err != nil {
		t.Fatalf("a record taking the request to %d resources was refused: %v", pasGraphMaxResources, err)
	}
	_, err := completeWith([]byte(contained(k + 1)))
	wantPastLimit(t, err, limitGraphResources)
}

// A request already past the relay's limits as built is the gateway's own
// build: a record read with it is not named, and the refusal stays the
// build's own.
func TestEvidence_ARequestPastTheRelayLimitsAsBuiltNamesNoRecord(t *testing.T) {
	t.Parallel() // the request reads about 1,000,000 tokens
	zeros := "[" + strings.TrimSuffix(strings.Repeat("0,", 1_000_100), ",") + "]"
	body := strings.Replace(string(evidenceLimitRequest()), `"intent":"order",`, `"intent":"order","x-extra":`+zeros+`,`, 1)
	read := func(_ context.Context, ref string) ([]byte, bool, error) {
		return evidenceLimitRecord("x", ""), ref == "Condition/c1", nil
	}
	_, err := retainPASRequestEvidence(context.Background(), []byte(body), read)
	var past *evidencePastLimitError
	if err == nil || errors.As(err, &past) {
		t.Fatalf("err = %v, want the build's own refusal, naming no record", err)
	}
}
