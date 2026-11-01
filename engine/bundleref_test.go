//go:build !shn_coreonly

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
	t.Parallel()
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
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), relayedPASAnswer(t, levelPASPayerAnswer)) {
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
	t.Parallel()
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
			strings.Replace(payorCoverage("", payorRef("#payer-org")), `"payor"`, `"contained":[{"resourceType":"Location","id":"payer-org"}],"payor"`, 1)),
			"Coverage.payor references a resource that is not an Organization"},
		"a contained Organization sharing its id with another contained resource": {payorBundle(
			strings.Replace(payorCoverage("", payorRef("#payer-org")), `"payor"`, `"contained":[{"resourceType":"Location","id":"payer-org"},{"resourceType":"Organization","id":"payer-org"}],"payor"`, 1)),
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
	t.Parallel()
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

// TestCoveragePayorRecords: each Coverage's payor reference outside itself
// resolves against the Bundle it came in by entryAnswers' rule, each
// reference once, else against the record the caller holds for it; a
// reference no record answers, or several different Organizations answer, is
// left out and told with its reason and resource type; a contained or
// identifier-only payor, and a record that is not an Organization, add
// nothing and are not told.
func TestCoveragePayorRecords(t *testing.T) {
	org := `{"resourceType":"Organization","id":"o1","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}`
	other := `{"resourceType":"Organization","id":"o1","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00002"}]}`
	cov := func(id, payor, extra string) string {
		return `{"resourceType":"Coverage","id":"` + id + `",` + extra + `"payor":[` + payor + `]}`
	}
	bundle := func(entries ...string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`)
	}
	entry := func(res string) string { return `{"resource":` + res + `}` }
	byRef := cov("c1", `{"reference":"Organization/o1"}`, "")
	for name, row := range map[string]struct {
		bundle    []byte
		coverages []string
		knownRef  string
		known     string
		want      []string
		told      []string
	}{
		"included in the bundle":              {bundle(entry(byRef), entry(org)), []string{byRef}, "", "", []string{org}, nil},
		"the record the caller holds":         {bundle(entry(byRef)), []string{byRef}, "Organization/o1", org, []string{org}, nil},
		"one reference, two Coverages":        {bundle(entry(byRef), entry(org)), []string{byRef, cov("c2", `{"reference":"Organization/o1"}`, "")}, "", "", []string{org}, nil},
		"one record, two spellings":           {bundle(`{"fullUrl":"https://sor.example/fhir/Organization/o1","resource":` + org + `}`), []string{byRef, cov("c2", `{"reference":"https://sor.example/fhir/Organization/o1"}`, "")}, "", "", []string{org}, nil},
		"an absolute reference by fullUrl":    {bundle(`{"fullUrl":"https://sor.example/fhir/Organization/o1","resource":` + org + `}`), []string{cov("c1", `{"reference":"https://sor.example/fhir/Organization/o1"}`, "")}, "", "", []string{org}, nil},
		"no record answers":                   {bundle(entry(byRef)), []string{byRef}, "", "", nil, []string{"none-answers Organization"}},
		"the caller holds another's":          {bundle(entry(byRef)), []string{byRef}, "Organization/o9", org, nil, []string{"none-answers Organization"}},
		"several Organizations answer":        {bundle(entry(byRef), entry(org), entry(other)), []string{byRef}, "Organization/o1", org, nil, []string{"several-answer Organization"}},
		"a URN no record answers":             {bundle(), []string{cov("c1", `{"reference":"urn:uuid:5f0c9a5e-0000-4000-8000-000000000001"}`, "")}, "", "", nil, []string{"none-answers "}},
		"a contained payor":                   {bundle(entry(cov("c1", `{"reference":"#o1"}`, `"contained":[`+org+`],`))), []string{cov("c1", `{"reference":"#o1"}`, `"contained":[`+org+`],`)}, "#o1", org, nil, nil},
		"a payor named by identifier alone":   {bundle(), []string{cov("c1", `{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}`, "")}, "", "", nil, nil},
		"a payor that is not an Organization": {bundle(entry(cov("c1", `{"reference":"Patient/p1"}`, "")), entry(`{"resourceType":"Patient","id":"p1"}`)), []string{cov("c1", `{"reference":"Patient/p1"}`, "")}, "", "", nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			coverages := make([][]byte, len(row.coverages))
			for i, c := range row.coverages {
				coverages[i] = []byte(c)
			}
			var told []string
			got := coveragePayorRecords(row.bundle, coverages, row.knownRef, []byte(row.known), func(reason, typ string) {
				told = append(told, reason+" "+typ)
			})
			if len(got) != len(row.want) {
				t.Fatalf("got %d records %q, want %q", len(got), got, row.want)
			}
			for i := range got {
				if !bytes.Equal(got[i], []byte(row.want[i])) {
					t.Fatalf("record %d = %s, want %s", i, got[i], row.want[i])
				}
			}
			if strings.Join(told, "|") != strings.Join(row.told, "|") {
				t.Fatalf("told %q, want %q", told, row.told)
			}
			// No one listening is no fault.
			coveragePayorRecords(row.bundle, coverages, row.knownRef, []byte(row.known), nil)
		})
	}
}

// TestReferenceResourceType: the resource type a reference names, and
// nothing else of it.
func TestReferenceResourceType(t *testing.T) {
	for ref, want := range map[string]string{
		"Organization/o1":                                     "Organization",
		"https://sor.example/fhir/Organization/o1":            "Organization",
		"https://sor.example/fhir/Organization/o1/_history/2": "Organization",
		"urn:uuid:5f0c9a5e-0000-4000-8000-000000000001":       "",
		"o1":                            "",
		"organization/o1":               "",
		"Org-anization/o1":              "",
		"https://h/x/Patient/":          "",
		"https://sor.example/JaneDoe/1": "",
	} {
		if got := referenceResourceType(ref); got != want {
			t.Errorf("referenceResourceType(%q) = %q, want %q", ref, got, want)
		}
	}
}
