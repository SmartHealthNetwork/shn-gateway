package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// payorBundle is a PAS $submit Bundle for MBR-COVERED whose Coverage entry is
// coverage and whose other entries are extra, each a whole entry object.
func payorBundle(coverage string, extra ...string) string {
	entries := append([]string{
		`{"resource":{"resourceType":"Patient","id":"MBR-COVERED"}}`,
		`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}}`,
		`{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}}`,
		coverage,
	}, extra...)
	return `{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(entries, ",") + `]}`
}

// payorCoverage is a Coverage entry whose first payor is payor, at fullURL
// ("" for none).
func payorCoverage(fullURL, payor string) string {
	entry := `{"resource":{"resourceType":"Coverage","id":"cov1","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[` + payor + `]}}`
	if fullURL != "" {
		entry = `{"fullUrl":"` + fullURL + `",` + entry[1:]
	}
	return entry
}

// payorOrg is an Organization entry with id (none when "") carrying the payer
// identifier value (none when ""), at fullURL ("" for none).
func payorOrg(fullURL, id, value string) string {
	org := `"resourceType":"Organization"`
	if id != "" {
		org += `,"id":"` + id + `"`
	}
	if value != "" {
		org += `,"identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"` + value + `"}]`
	}
	entry := `{"resource":{` + org + `}}`
	if fullURL != "" {
		entry = `{"fullUrl":"` + fullURL + `",` + entry[1:]
	}
	return entry
}

func payorRef(ref string) string { return `{"reference":"` + ref + `"}` }

const (
	ehrOrg   = "https://ehr.example/fhir/Organization/payer-org"
	ehrCov   = "https://ehr.example/fhir/Coverage/cov1"
	otherOrg = "https://other.example/fhir/Organization/payer-org"
	urnOrg   = "urn:uuid:6d1f0c3e-8b8e-4f5c-9a43-2b7f6a1c9e01"
)

// A Coverage.payor reference resolves among the Bundle's own entries (by
// fullUrl, or a relative reference by type and id), and the request routes, at
// every level.
func TestPASIngressPayorReference_ResolvesInTheBundle(t *testing.T) {
	rows := map[string]string{
		"a relative reference to an entry": payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			payorOrg("", "payer-org", "00001")),
		"an absolute reference equal to an entry's fullUrl": payorBundle(
			payorCoverage("", payorRef(ehrOrg)),
			payorOrg(ehrOrg, "payer-org", "00001")),
		"a urn:uuid reference equal to an entry's fullUrl": payorBundle(
			payorCoverage("", payorRef(urnOrg)),
			payorOrg(urnOrg, "", "00001")),
		"a relative reference among entries with absolute fullUrls": payorBundle(
			payorCoverage(ehrCov, payorRef("Organization/payer-org")),
			payorOrg(ehrOrg, "payer-org", "00001")),
		// A Bundle that repeats its payor Organization names one payer.
		"a relative reference two identical entries answer": payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			payorOrg("", "payer-org", "00001"),
			payorOrg("", "payer-org", "00001")),
		"an absolute reference two entries naming the same payer answer": payorBundle(
			payorCoverage("", payorRef(urnOrg)),
			payorOrg(urnOrg, "", "00001"),
			payorOrg(urnOrg, "payer-org", "00001")),
		// A value that cannot be a fullUrl on another entry leaves the rest of
		// the Bundle readable, as before the payor was resolved by fullUrl.
		"an entry whose fullUrl is not a string": strings.Replace(payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			payorOrg("", "payer-org", "00001")), `{"resource":{"resourceType":"ServiceRequest"`, `{"fullUrl":7,"resource":{"resourceType":"ServiceRequest"`, 1),
		// Members named like fullUrl and id in another case are not those
		// members, and leave the Bundle readable.
		"an entry with a differently cased fullUrl member": strings.Replace(payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			payorOrg("", "payer-org", "00001")), `{"resource":{"resourceType":"ServiceRequest"`, `{"FullUrl":"x","resource":{"resourceType":"ServiceRequest"`, 1),
		"a Coverage with a differently cased id member": strings.Replace(payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			payorOrg("", "payer-org", "00001")), `"resourceType":"Coverage","id":"cov1"`, `"resourceType":"Coverage","Id":"cov1"`, 1),
	}
	for name, body := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, _ := levelPASRow(t, level, body, levelPASPayerAnswer)
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), levelPASPayerAnswer) {
					t.Fatalf("answer %d %s, want the payer's answer", rec.Code, rec.Body.String())
				}
				wantCarriedExactly(t, env, body)
			})
		}
	}
}

// A Coverage.payor that names no payer identifier the Bundle can supply is
// refused before the network at every level, and the refusal says which shape
// failed without echoing what the request carried.
func TestPASIngressPayorReference_RefusalSaysWhy(t *testing.T) {
	const prefix = noPayerIdentifier + ": "
	rows := map[string]struct{ body, why string }{
		"a relative reference no entry answers": {payorBundle(
			payorCoverage("", payorRef("Organization/payer-org"))),
			"Coverage.payor is a relative reference that matches no entry of the Bundle: the payor Organization must be an entry of the Bundle"},
		"an absolute reference to the sender's server, the Organization present only by id": {payorBundle(
			payorCoverage("", payorRef(ehrOrg)),
			payorOrg("", "payer-org", "00001")),
			"Coverage.payor is an absolute reference that is no entry's fullUrl: the payor Organization must be an entry of the Bundle"},
		"an absolute reference to another entry's server": {payorBundle(
			payorCoverage("", payorRef(ehrOrg)),
			payorOrg(otherOrg, "payer-org", "00001")),
			"Coverage.payor is an absolute reference that is no entry's fullUrl: the payor Organization must be an entry of the Bundle"},
		"an Organization with no identifier": {payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			payorOrg("", "payer-org", "")),
			"the payor Organization in the Bundle carries no identifier with both a system and a value, such as a NAIC code or payer id"},
		"an Organization whose identifier has no system": {payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			`{"resource":{"resourceType":"Organization","id":"payer-org","identifier":[{"value":"00001"}]}}`),
			"the payor Organization in the Bundle carries no identifier with both a system and a value, such as a NAIC code or payer id"},
		// The Coverage's own base would name one of them; the rule does not
		// read it, so the two are ambiguous rather than one picked.
		"a relative reference two entries answer": {payorBundle(
			payorCoverage(ehrCov, payorRef("Organization/payer-org")),
			payorOrg(ehrOrg, "payer-org", "00001"),
			payorOrg(otherOrg, "payer-org", "99999")),
			"Coverage.payor matches more than one entry of the Bundle, and they do not name one payer"},
		"two entries, one naming no payer": {payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			payorOrg("", "payer-org", "00001"),
			payorOrg("", "payer-org", "")),
			"Coverage.payor matches more than one entry of the Bundle, and they do not name one payer"},
		"two entries, the first naming no payer": {payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			payorOrg("", "payer-org", ""),
			payorOrg("", "payer-org", "00001")),
			"Coverage.payor matches more than one entry of the Bundle, and they do not name one payer"},
		"two entries, one not an Organization": {payorBundle(
			payorCoverage("", payorRef(urnOrg)),
			payorOrg(urnOrg, "", "00001"),
			`{"fullUrl":"`+urnOrg+`","resource":{"resourceType":"Patient","id":"MBR-COVERED"}}`),
			"Coverage.payor matches more than one entry of the Bundle, and they do not name one payer"},
		"an absolute reference two entries answer": {payorBundle(
			payorCoverage("", payorRef(urnOrg)),
			payorOrg(urnOrg, "", "00001"),
			payorOrg(urnOrg, "", "99999")),
			"Coverage.payor matches more than one entry of the Bundle, and they do not name one payer"},
		"an Organization whose identifier is not a list": {payorBundle(
			payorCoverage("", payorRef("Organization/payer-org")),
			`{"resource":{"resourceType":"Organization","id":"payer-org","identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}}`),
			"the payor Organization in the Bundle carries no identifier with both a system and a value, such as a NAIC code or payer id"},
		"a reference to a resource that is not an Organization": {payorBundle(
			payorCoverage("", payorRef("Patient/MBR-COVERED"))),
			"Coverage.payor references a resource that is not an Organization"},
		"a contained reference the Coverage does not contain": {payorBundle(
			payorCoverage("", payorRef("#payer-org"))),
			"Coverage.payor names a contained resource the Coverage does not contain"},
		"a contained Organization with no identifier": {payorBundle(
			strings.Replace(payorCoverage("", payorRef("#payer-org")), `"payor"`, `"contained":[{"resourceType":"Organization","id":"payer-org"}],"payor"`, 1)),
			"the contained payor Organization carries no identifier with both a system and a value, such as a NAIC code or payer id"},
		"an absolute reference that does not parse": {payorBundle(
			payorCoverage("", payorRef("https://ehr.example/fhir/Organization/%zz")),
			payorOrg("", "payer-org", "00001")),
			"Coverage.payor is an absolute reference that is no entry's fullUrl: the payor Organization must be an entry of the Bundle"},
		"a Coverage that names no payor": {payorBundle(
			payorCoverage("", "")),
			"the Coverage names no payor"},
		"a contained reference to a resource that is not an Organization": {payorBundle(
			strings.Replace(payorCoverage("", payorRef("#payer-org")), `"payor"`, `"contained":[{"resourceType":"Patient","id":"payer-org"}],"payor"`, 1)),
			"Coverage.payor references a resource that is not an Organization"},
		"a contained Organization sharing its id with another contained resource": {payorBundle(
			strings.Replace(payorCoverage("", payorRef("#payer-org")), `"payor"`, `"contained":[{"resourceType":"Patient","id":"payer-org"},{"resourceType":"Organization","id":"payer-org"}],"payor"`, 1)),
			"the contained payor Organization carries no identifier with both a system and a value, such as a NAIC code or payer id"},
		"a payor with neither a reference nor a whole identifier": {payorBundle(
			payorCoverage("", `{"identifier":{"value":"00001"}}`)),
			"Coverage.payor carries neither a reference nor an identifier with both a system and a value"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				env.originator.handlePASIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(row.body)))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, prefix+row.why)
				for _, echoed := range []string{"payer-org", "example", "6d1f0c3e", "00001"} {
					if strings.Contains(rec.Body.String(), echoed) {
						t.Fatalf("the refusal echoes %q from the request: %s", echoed, rec.Body.String())
					}
				}
			})
		}
	}
}

// A payor the gateway cannot read for any reason it can name keeps the bare
// refusal, rather than a reason that may be wrong.
func TestPASIngressPayorReference_UnnamedReasonKeepsTheBareRefusal(t *testing.T) {
	body := payorBundle(
		payorCoverage("", payorRef(ehrOrg)),
		`{"fullUrl":"`+ehrOrg+`","resource":{"resourceType":"Organization","id":9,"identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			rec := httptest.NewRecorder()
			env.originator.handlePASIngress(rec, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(body)))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, noPayerIdentifier)
			if strings.Contains(rec.Body.String(), noPayerIdentifier+":") {
				t.Fatalf("a reason the gateway cannot name is not guessed: %s", rec.Body.String())
			}
		})
	}
}

// The inquiry route reads its payor by the same rule: an absolute reference
// equal to an entry's fullUrl routes, and one no entry answers is refused with
// the reason, at every level.
func TestPASInquirePayorReference_SameRule(t *testing.T) {
	inline := `"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]`
	withPayor := func(t *testing.T, ref string, org string) string {
		t.Helper()
		body := levelInquiryWith(t, inline, `"payor":[`+payorRef(ref)+`]`)
		if org != "" {
			body = strings.Replace(body, `"entry":[`, `"entry":[`+org+`,`, 1)
		}
		return body
	}
	for _, level := range allLevels {
		t.Run("routes/"+level.String(), func(t *testing.T) {
			answer := levelInquiryPayerAnswer(t)
			env, rec, _ := levelInquireRow(t, level, withPayor(t, urnOrg, payorOrg(urnOrg, "", "00001")), answer)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s, want the payer's answer", rec.Code, rec.Body.String())
			}
			if env.routeHitCount() == 0 {
				t.Fatal("the inquiry never crossed the network")
			}
		})
		t.Run("refused, relative/"+level.String(), func(t *testing.T) {
			env, rec, _ := levelInquireRow(t, level, withPayor(t, "Organization/payer-org", ""), levelInquiryPayerAnswer(t))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity,
				noPayerIdentifier+": Coverage.payor is a relative reference that matches no entry of the Bundle: the payor Organization must be an entry of the Bundle")
		})
		t.Run("refused/"+level.String(), func(t *testing.T) {
			env, rec, _ := levelInquireRow(t, level, withPayor(t, ehrOrg, payorOrg("", "payer-org", "00001")), levelInquiryPayerAnswer(t))
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity,
				noPayerIdentifier+": Coverage.payor is an absolute reference that is no entry's fullUrl: the payor Organization must be an entry of the Bundle")
		})
	}
}
