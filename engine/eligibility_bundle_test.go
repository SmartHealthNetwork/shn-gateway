package engine

// eligibility_bundle_test.go — the Bundle forms of
// CoverageEligibilityRequest/$submit: IHE FAIS's Bundle carrying one request
// with the resources it references, and a batch of requests (each one alone or
// in such a Bundle). Every form is carried exactly as sent; references resolve
// among the Bundle's own entries only to route and bind.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const eligFAISFamily = "Faisfamily"
const eligFAISBirthDate = "1975-04-02"

// eligFAISParts are the resources of IHE FAIS's CoverageEligibilityRequest
// Bundle for member, naming the payer payerValue: the request references its
// patient, coverage and payer by urn:uuid, each carried as an entry.
func eligFAISParts(member, payerValue string) (request, patient, coverage, payer map[string]any) {
	request = map[string]any{"resourceType": "CoverageEligibilityRequest", "status": "active", "purpose": []any{"benefits"},
		"patient": map[string]any{"reference": "urn:uuid:patient"}, "created": "2026-10-17",
		"insurer":   map[string]any{"reference": "urn:uuid:payer"},
		"insurance": []any{map[string]any{"coverage": map[string]any{"reference": "urn:uuid:coverage"}}}}
	patient = map[string]any{"resourceType": "Patient", "id": member, "birthDate": eligFAISBirthDate, "name": []any{map[string]any{"family": eligFAISFamily}}}
	coverage = map[string]any{"resourceType": "Coverage", "id": "cov-1", "status": "active",
		"beneficiary": map[string]any{"reference": "urn:uuid:patient"}, "payor": []any{map[string]any{"reference": "urn:uuid:payer"}}}
	payer = map[string]any{"resourceType": "Organization", "id": "payer-1", "identifier": []any{eligibilityPayorIdentifier(payerValue)}}
	return
}

// eligFAISBundle is the Bundle of entries, each {fullUrl, resource}.
func eligFAISBundle(t *testing.T, entries ...[2]any) []byte {
	t.Helper()
	var list []any
	for _, e := range entries {
		entry := map[string]any{"resource": e[1]}
		if e[0] != "" {
			entry["fullUrl"] = e[0]
		}
		list = append(list, entry)
	}
	return mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": list})
}

// eligFAIS is IHE FAIS's Bundle for member, naming the payer payerValue.
func eligFAIS(t *testing.T, member, payerValue string) []byte {
	t.Helper()
	request, patient, coverage, payer := eligFAISParts(member, payerValue)
	return eligFAISBundle(t, [2]any{"urn:uuid:request", request}, [2]any{"urn:uuid:patient", patient},
		[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer})
}

// withPatientRef is request naming its patient by ref.
func withPatientRef(request map[string]any, ref string) map[string]any {
	out := map[string]any{}
	for k, v := range request {
		out[k] = v
	}
	out["patient"] = map[string]any{"reference": ref}
	return out
}

// eligRequestWith is request with key set to value and its patient named by
// ref.
func eligRequestWith(request map[string]any, key string, value any, ref string) map[string]any {
	out := withPatientRef(request, ref)
	out[key] = value
	return out
}

// eligBundledOddly is request's bytes with key set to value and its patient
// named by ref.
func eligBundledOddly(t *testing.T, request map[string]any, key string, value any, ref string) []byte {
	t.Helper()
	return mustJSON(t, eligRequestWith(request, key, value, ref))
}

// eligBatchHidingSecond is a batch of first and second whose second entry
// names its resource in another case.
func eligBatchHidingSecond(first, second []byte) []byte {
	return []byte(`{"resourceType":"Bundle","type":"batch","entry":[{"resource":` + string(first) + `},{"Resource":` + string(second) + `}]}`)
}

// eligFAISHiding is a FAIS Bundle of request and patient that carries hidden
// as a second request under an entry member named in another case.
func eligFAISHiding(t *testing.T, request, patient map[string]any, hidden []byte) []byte {
	t.Helper()
	return []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + string(mustJSON(t, request)) +
		`},{"fullUrl":"urn:uuid:patient","resource":` + string(mustJSON(t, patient)) + `},{"Resource":` + string(hidden) + `}]}`)
}

// eligBatch is a batch of requests, each one alone or in a Bundle of its own.
func eligBatch(t *testing.T, requests ...[]byte) []byte {
	t.Helper()
	var entries []any
	for _, r := range requests {
		entries = append(entries, map[string]any{"resource": json.RawMessage(r)})
	}
	return mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "batch", "entry": entries})
}

// eligResponses is the payer's answer to several requests: a Bundle of its
// responses about each patient.
func eligResponses(t *testing.T, patientRefs ...string) []byte {
	t.Helper()
	var entries []any
	for _, ref := range patientRefs {
		entries = append(entries, map[string]any{"resource": json.RawMessage(payersEligibilityAnswer(ref))})
	}
	return mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": entries})
}

// Every Bundle form is carried to the payer exactly as sent, routed by the
// payer its requests name, and the payer's answer relayed exactly, at every
// level, with nothing edited.
func TestEligibilityBundle_EachFormCarriedAsSent(t *testing.T) {
	covered, other := eligibilityRequest(t, "MBR-COVERED"), eligibilityRequest(t, anotherHeldMember)
	one := payersEligibilityAnswer("Patient/MBR-COVERED")
	two := eligResponses(t, "Patient/MBR-COVERED", "Patient/"+anotherHeldMember)
	for name, row := range map[string]struct{ body, answer []byte }{
		"FAIS Bundle":               {eligFAIS(t, "MBR-COVERED", "00001"), one},
		"FAIS Bundle in Parameters": {eligibilityAsParameters(t, eligFAIS(t, "MBR-COVERED", "00001")), one},
		"batch of requests":         {eligBatch(t, covered, other), two},
		"batch of FAIS Bundles":     {eligBatch(t, eligFAIS(t, "MBR-COVERED", "00001"), eligFAIS(t, anotherHeldMember, "00001")), two},
		"batch of both":             {eligBatch(t, eligFAIS(t, "MBR-COVERED", "00001"), other), two},
		"batch of one":              {eligBatch(t, eligFAIS(t, "MBR-COVERED", "00001")), one},
		"batch in Parameters":       {eligibilityAsParameters(t, eligBatch(t, covered, other)), two},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runEligibilityIngress(t, level, row.body, row.answer)
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), row.answer) {
					t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
				}
				if sent := env.lastRequestPayload(); !bytes.Equal(sent, row.body) {
					t.Fatalf("the payer's side received\n%s\nwant the input as sent\n%s", sent, row.body)
				}
				if x.Outcome != ExchangeAnswered || x.Route != RouteEligibility || x.Recipient != env.payerID || len(x.Edits) != 0 {
					t.Fatalf("record %+v, want an answered eligibility exchange with the payer, carried unedited", x)
				}
			})
		}
	}
}

// A FAIS Bundle's references resolve among its own entries: by fullUrl or by
// type and id, a Coverage entry's "#id" among that Coverage's own contained
// resources.
func TestEligibilityBundle_RoutesByTheEntriesItsRequestReferences(t *testing.T) {
	request, patient, coverage, payer := eligFAISParts("MBR-COVERED", "00001")
	byDisplay := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		out["insurer"] = map[string]any{"display": "the plan"}
		return out
	}
	containedPayor := map[string]any{"resourceType": "Coverage", "id": "cov-1", "status": "active",
		"beneficiary": map[string]any{"reference": "urn:uuid:patient"}, "payor": []any{map[string]any{"reference": "#org"}},
		"contained": []any{map[string]any{"resourceType": "Organization", "id": "org", "identifier": []any{eligibilityPayorIdentifier("00001")}}}}
	relative := map[string]any{}
	for k, v := range request {
		relative[k] = v
	}
	relative["insurer"] = map[string]any{"reference": "Organization/payer-1"}
	answer := payersEligibilityAnswer("Patient/MBR-COVERED")
	for name, body := range map[string][]byte{
		"insurer by fullUrl":       eligFAIS(t, "MBR-COVERED", "00001"),
		"insurer by type and id":   eligFAISBundle(t, [2]any{"", relative}, [2]any{"urn:uuid:patient", patient}, [2]any{"", payer}),
		"coverage entry's payor":   eligFAISBundle(t, [2]any{"", byDisplay(request)}, [2]any{"urn:uuid:patient", patient}, [2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}),
		"coverage's contained org": eligFAISBundle(t, [2]any{"", byDisplay(request)}, [2]any{"urn:uuid:patient", patient}, [2]any{"urn:uuid:coverage", containedPayor}),
	} {
		t.Run(name, func(t *testing.T) {
			env, rec, x := runEligibilityIngress(t, EnforcementStrict, body, answer)
			if rec.Code != http.StatusOK || x.Recipient != env.payerID {
				t.Fatalf("answer %d %s (recipient %q), want routed to the payer and answered", rec.Code, rec.Body, x.Recipient)
			}
		})
	}
}

// A submission naming more than one payer, or a request in it naming none,
// is refused as routing before anything is sent, at every level, and says
// which requests: one leg has one recipient.
func TestEligibilityBundle_UnroutableSubmissionIsRefused(t *testing.T) {
	request, patient, coverage, _ := eligFAISParts("MBR-COVERED", "00001")
	twoOrgs := eligFAISBundle(t, [2]any{"", map[string]any{"resourceType": "CoverageEligibilityRequest", "status": "active",
		"patient": map[string]any{"reference": "urn:uuid:patient"}, "insurer": map[string]any{"reference": "Organization/payer-1"}}},
		[2]any{"urn:uuid:patient", patient},
		[2]any{"urn:uuid:a", map[string]any{"resourceType": "Organization", "id": "payer-1", "identifier": []any{eligibilityPayorIdentifier("00001")}}},
		[2]any{"urn:uuid:b", map[string]any{"resourceType": "Organization", "id": "payer-1", "identifier": []any{eligibilityPayorIdentifier("99999")}}})
	noPayer := eligibilityWith(t, anotherHeldMember, map[string]any{"insurer": map[string]any{"display": "the plan"}})
	for name, row := range map[string]struct {
		body []byte
		msg  string
	}{
		"batch naming two payers": {eligBatch(t, eligibilityRequest(t, "MBR-COVERED"), eligFAIS(t, anotherHeldMember, "99999")),
			eligibilityBatchTwoPayers + " (request 0 names " + shnsdk.CMSPayerIdentity.System + "|00001, request 1 names " + shnsdk.CMSPayerIdentity.System + "|99999)"},
		"a request of the batch naming none": {eligBatch(t, eligibilityRequest(t, "MBR-COVERED"), noPayer), "request 1 of the batch: " + eligibilityNoPayer},
		"references no entry answers": {eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", patient}, [2]any{"urn:uuid:other", coverage}),
			eligibilityCoverageUnresolved + remedyEntryCoverage + `: insurance[0].coverage "urn:uuid:coverage"`},
		"insurer naming a Coverage entry": {eligFAISBundle(t, [2]any{"", map[string]any{"resourceType": "CoverageEligibilityRequest", "status": "active",
			"patient": map[string]any{"reference": "urn:uuid:patient"}, "insurer": map[string]any{"reference": "urn:uuid:coverage"}}},
			[2]any{"urn:uuid:patient", patient}, [2]any{"urn:uuid:coverage", eligibilityCoverageIdentified(coverage, eligibilityPayorIdentifier("00001"))}),
			eligibilityNoPayer},
		"two coverage entries answering one reference": {eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", patient},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:coverage", coverage}),
			"ambiguous reference for routing: more than one entry of the eligibility request's Bundle answers \"urn:uuid:coverage\""},
		"two entries answering one reference": {twoOrgs, "ambiguous reference for routing: more than one entry of the eligibility request's Bundle answers \"Organization/payer-1\""},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runEligibilityIngress(t, level, row.body, nil)
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, jsonEscaped(row.msg))
				wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
			})
		}
	}
}

// jsonEscaped is s as it reads inside a JSON string.
func jsonEscaped(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// A Bundle in neither of the operation's forms, or whose request's patient
// cannot be read one way only, submits nothing to carry: refused before
// anything is sent, at every level.
func TestEligibilityBundle_RefusesWhatIsNeitherForm(t *testing.T) {
	request, patient, coverage, payer := eligFAISParts("MBR-COVERED", "00001")
	cer := eligibilityRequest(t, "MBR-COVERED")
	const unread = "parse eligibility request failed: "
	noID := map[string]any{"resourceType": "Patient", "birthDate": eligFAISBirthDate}
	for name, row := range map[string]struct {
		body []byte
		msg  string
	}{
		"two requests beside other resources": {eligFAISBundle(t, [2]any{"", request}, [2]any{"", request}, [2]any{"urn:uuid:patient", patient}),
			unread + "the Bundle is neither one CoverageEligibilityRequest with the resources it references nor a batch of requests"},
		"a batch's Bundle carrying two requests": {eligBatch(t, cer, eligFAISBundle(t, [2]any{"", request}, [2]any{"", request})),
			unread + "entry[1] is a Bundle that does not carry exactly one CoverageEligibilityRequest"},
		"a batch's Bundle carrying a Bundle": {eligBatch(t, cer, eligFAISBundle(t, [2]any{"", request}, [2]any{"", json.RawMessage(eligFAIS(t, "MBR-COVERED", "00001"))})),
			unread + "entry[1] is a Bundle that does not carry exactly one CoverageEligibilityRequest"},
		"entries named in another case": {bytes.Replace(eligFAIS(t, "MBR-COVERED", "00001"), []byte(`"entry":`), []byte(`"Entry":`), 1),
			unread + "the Bundle's entries cannot be read one way only"},
		"a request named in another case": {bytes.Replace(eligFAIS(t, "MBR-COVERED", "00001"), []byte(`"resourceType":"CoverageEligibilityRequest"`), []byte(`"RESOURCETYPE":"CoverageEligibilityRequest"`), 1),
			unread + "the Bundle's entries cannot be read one way only"},
		"patient reference two entries answer": {eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", patient}, [2]any{"urn:uuid:patient", patient},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
		"a batch's Bundle carrying no request": {eligBatch(t, cer, eligFAISBundle(t, [2]any{"urn:uuid:patient", patient})),
			unread + "entry[1] is a Bundle that does not carry exactly one CoverageEligibilityRequest"},
		"a batch's Bundle that cannot be read": {eligBatch(t, cer, bytes.Replace(eligFAIS(t, "MBR-COVERED", "00001"), []byte(`"entry":`), []byte(`"Entry":`), 1)),
			unread + "entry[1] is a Bundle that does not carry exactly one CoverageEligibilityRequest"},
		"patient entry that cannot be read": {eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", map[string]any{"resourceType": "Patient", "Identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-COVERED"}}}},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
		"absolute reference to an entry of another id": {eligFAISBundle(t, [2]any{"", withPatientRef(request, "http://ehr.example/fhir/Patient/MBR-COVERED")},
			[2]any{"http://ehr.example/fhir/Patient/MBR-COVERED", map[string]any{"resourceType": "Patient", "id": "MBR-OX"}},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
		// A reference no Patient entry answers names no patient: it is not
		// read as a member id.
		"patient reference no entry answers": {eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient-mistyped", patient},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
		"patient reference naming an Organization": {eligFAISBundle(t, [2]any{"", withPatientRef(request, "urn:uuid:payer")}, [2]any{"urn:uuid:patient", patient},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
		// A request in a Bundle is read by its exact member names: a patient
		// reference that cannot be read so, or is empty, names no patient.
		"patient named in another case": {bytes.Replace(eligFAIS(t, "MBR-COVERED", "00001"), []byte(`"patient":{"reference":"urn:uuid:patient"}`), []byte(`"PATIENT":{"reference":"urn:uuid:patient"}`), 1),
			`eligibility request names \"PATIENT\" for \"patient\": member names are case-sensitive, so the request names no patient to bind`},
		"reference named in another case": {bytes.Replace(eligFAIS(t, "MBR-COVERED", "00001"), []byte(`"patient":{"reference":"urn:uuid:patient"}`), []byte(`"patient":{"Reference":"urn:uuid:patient"}`), 1),
			`eligibility request names \"Reference\" for \"reference\": member names are case-sensitive, so the request names no patient to bind`},
		"empty patient reference": {eligFAISBundle(t, [2]any{"", withPatientRef(request, "")}, [2]any{"urn:uuid:patient", patient},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
		// A request alone in a batch carries no entries: a reference that is
		// neither Patient/<id> nor #id names no patient.
		"a batch's request naming its patient by urn:uuid": {eligBatch(t, cer, mustJSON(t, withPatientRef(request, "urn:uuid:patient"))),
			"parse member failed (request 1 of the batch)"},
		// Its contained resources too: a request whose contained cannot be
		// read one way only is not read by a reading that skips them.
		"a batch's request with contained named in another case": {eligBatch(t, cer, eligBundledOddly(t, request, "Contained", []any{containedMember("p", "MBR-OX")}, "#p")),
			`eligibility request names \"Contained\" for \"contained\": member names are case-sensitive, so the request names no patient to bind` + " (request 1 of the batch)"},
		"contained that is not a list": {eligFAISBundle(t, [2]any{"", eligRequestWith(request, "contained", "x", "#p")}, [2]any{"urn:uuid:patient", patient},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
		"#id naming no contained Patient": {eligFAISBundle(t, [2]any{"", withPatientRef(request, "#p")}, [2]any{"urn:uuid:patient", patient},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
		"absolute reference to an entry of another type": {eligFAISBundle(t, [2]any{"", withPatientRef(request, "http://ehr.example/fhir/Patient/MBR-COVERED")},
			[2]any{"http://ehr.example/fhir/Patient/MBR-COVERED", map[string]any{"resourceType": "Organization", "id": "MBR-COVERED"}},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
		// Every entry is read by its exact member names: one a reader would
		// skip or read untyped hides a request, so the Bundle is refused.
		"a batch entry's resource named in another case": {eligBatchHidingSecond(cer, eligibilityRequest(t, anotherHeldMember)),
			unread + "the Bundle's entries cannot be read one way only"},
		"a second request's type named in another case": {eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", patient},
			[2]any{"", map[string]any{"RESOURCETYPE": "CoverageEligibilityRequest", "status": "active"}}), unread + "the Bundle's entries cannot be read one way only"},
		"an entry's fullUrl named in another case": {bytes.Replace(eligFAIS(t, "MBR-COVERED", "00001"), []byte(`"fullUrl":"urn:uuid:payer"`), []byte(`"FullUrl":"urn:uuid:payer"`), 1),
			unread + "the Bundle's entries cannot be read one way only"},
		// A Bundle in a batch is read as strictly: one hiding a request under
		// an entry member named in another case, or carrying an untyped
		// entry, is refused.
		"a batch's Bundle hiding a request": {eligBatch(t, cer, eligFAISHiding(t, request, patient, eligibilityRequest(t, anotherHeldMember))),
			unread + "entry[1] is a Bundle that does not carry exactly one CoverageEligibilityRequest"},
		"a batch's Bundle with an untyped entry": {eligBatch(t, cer, eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", patient},
			[2]any{"urn:uuid:untyped", map[string]any{"id": "x"}})), unread + "entry[1] is a Bundle that does not carry exactly one CoverageEligibilityRequest"},
		"a batch's Bundle with an entry with no resource": {eligBatch(t, cer, mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
			map[string]any{"resource": request}, map[string]any{"fullUrl": "urn:uuid:x"}}})), unread + "entry[1] is a Bundle that does not carry exactly one CoverageEligibilityRequest"},
		"an entry whose resource names no type": {eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", patient},
			[2]any{"urn:uuid:untyped", map[string]any{"id": "x"}}), unread + "the Bundle's entries cannot be read one way only"},
		"an entry with no resource": {mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
			map[string]any{"resource": request}, map[string]any{"fullUrl": "urn:uuid:empty"}}}), unread + "the Bundle's entries cannot be read one way only"},
		"patient with no id or member identifier": {eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", noID},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}), "parse member failed"},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, _ := runEligibilityIngress(t, level, row.body, nil)
				refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, row.msg)
			})
		}
	}
}

// A FAIS Bundle's patient, referenced by urn:uuid, is its Patient entry: the
// member that entry's id names, held or derived from the entry's own
// demographics, as a Patient sent alongside any request binds.
func TestEligibilityBundle_BindsThePatientEntry(t *testing.T) {
	for name, row := range map[string]struct {
		member, want string
	}{
		"held":     {"MBR-COVERED", heldPCI(t, "MBR-COVERED")},
		"not held": {strangerMember, derivedPCI(strangerMember, eligFAISBirthDate, eligFAISFamily)},
	} {
		t.Run(name, func(t *testing.T) {
			env, rec, _ := runEligibilityIngress(t, EnforcementObserve, eligFAIS(t, row.member, "00001"), payersEligibilityAnswer("Patient/"+row.member))
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body)
			}
			if got := env.substrate.authorizedSubjects; len(got) != 1 || got[0] != row.want {
				t.Fatalf("leg authorized for %v, want %s", got, row.want)
			}
		})
	}
	request, _, coverage, payer := eligFAISParts("MBR-COVERED", "00001")
	for name, body := range map[string][]byte{
		// An entry with no id names its member by its member identifier.
		"by member identifier": eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", eligIdentifiedPatient("MBR-COVERED")},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}),
		// A well-formed absolute reference names the entry with that fullUrl.
		"by absolute fullUrl": eligFAISBundle(t, [2]any{"", withPatientRef(request, "http://ehr.example/fhir/Patient/MBR-COVERED")},
			[2]any{"http://ehr.example/fhir/Patient/MBR-COVERED", map[string]any{"resourceType": "Patient", "id": "MBR-COVERED"}},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer}),
	} {
		t.Run(name, func(t *testing.T) {
			env, rec, _ := runEligibilityIngress(t, EnforcementObserve, body, payersEligibilityAnswer("Patient/MBR-COVERED"))
			want := heldPCI(t, "MBR-COVERED")
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body)
			}
			if got := env.substrate.authorizedSubjects; len(got) != 1 || got[0] != want {
				t.Fatalf("leg authorized for %v, want %s", got, want)
			}
		})
	}
}

// eligIdentifiedPatient is a Patient with no id, naming member by its member
// identifier.
func eligIdentifiedPatient(member string) map[string]any {
	return map[string]any{"resourceType": "Patient", "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": member}}}
}

// The leg's patient is the batch's first request's, in document order:
// reordering the batch changes only which patient the leg is authorized for,
// never what is carried, which is the batch exactly as each was sent.
func TestEligibilityBundle_FirstRequestIsTheLegsPatient(t *testing.T) {
	covered, other := eligibilityRequest(t, "MBR-COVERED"), eligibilityRequest(t, anotherHeldMember)
	answer := eligResponses(t, "Patient/MBR-COVERED", "Patient/"+anotherHeldMember)
	for name, row := range map[string]struct {
		body []byte
		want string
	}{
		"covered first": {eligBatch(t, covered, other), heldPCI(t, "MBR-COVERED")},
		"other first":   {eligBatch(t, other, covered), heldPCI(t, anotherHeldMember)},
	} {
		t.Run(name, func(t *testing.T) {
			env, rec, _ := runEligibilityIngress(t, EnforcementObserve, row.body, answer)
			if rec.Code != http.StatusOK || !bytes.Equal(env.lastRequestPayload(), row.body) {
				t.Fatalf("answer %d %s, want the batch carried as sent", rec.Code, rec.Body)
			}
			if got := env.substrate.authorizedSubjects; len(got) != 1 || got[0] != row.want {
				t.Fatalf("leg authorized for %v, want the first request's patient %s", got, row.want)
			}
		})
	}
}

// Each request of a batch binds its patient from its own resources: two FAIS
// Bundles for a member the system of record does not hold, each carrying its
// own Patient under the same id, name two patients, each derived from the
// Patient its own Bundle carries, never from another request's.
func TestEligibilityBundle_EachRequestBindsFromItsOwnResources(t *testing.T) {
	g, _, _, _ := involvedGateway(t, true)
	const member = "MBR-NEWCOMER"
	fais := func(birthDate, family string) []byte {
		request, patient, coverage, payer := eligFAISParts(member, "00001")
		patient["birthDate"], patient["name"] = birthDate, []any{map[string]any{"family": family}}
		return eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", patient},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer})
	}
	subject := derivedPCI(member, "1970-01-01", "Alpha")
	got := g.requestNamedPatients(context.Background(), "coverage-eligibility", "corr-1", eligBatch(t, fais("1970-01-01", "Alpha"), fais("1980-02-02", "Beta")), subject)
	want := []involvedPatient{{pci: derivedPCI(member, "1980-02-02", "Beta"), involvement: shnsdk.InvolvementRequestNamed}}
	if !slices.Equal(got, want) {
		t.Fatalf("named %v, want the second request's own patient %v", got, want)
	}
}

// Every other patient a batch names is named on the leg's involved list, as
// request-named, so the network records the exchange under each; a FAIS
// Bundle names only its own patient.
func TestEligibilityBundle_OtherPatientsAreOnTheInvolvedList(t *testing.T) {
	g, _, _, omitted := involvedGateway(t, true)
	subject := heldPCI(t, "MBR-COVERED")
	batch := eligBatch(t, eligibilityRequest(t, "MBR-COVERED"), eligFAIS(t, "MBR-OX", "00001"))
	got := g.requestNamedPatients(context.Background(), "coverage-eligibility", "corr-1", batch, subject)
	want := []involvedPatient{{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed}}
	if !slices.Equal(got, want) {
		t.Fatalf("named %v, want %v", got, want)
	}
	if got := g.requestNamedPatients(context.Background(), "coverage-eligibility", "corr-2", eligFAIS(t, "MBR-COVERED", "00001"), subject); got != nil {
		t.Fatalf("a FAIS Bundle named %v, want no one", got)
	}
	// Patients are read as each request's binding reads them: a contained
	// Patient and a Patient entry with no id by their member identifier, so
	// neither names a patient by a local id, and the second request's
	// patient is named by its own member.
	contained := eligibilityWith(t, "MBR-COVERED", map[string]any{"patient": map[string]any{"reference": "#p1"},
		"contained": []any{map[string]any{"resourceType": "Patient", "id": "p1", "identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-COVERED"}}}}})
	if got := g.requestNamedPatients(context.Background(), "coverage-eligibility", "corr-3", contained, subject); got != nil {
		t.Fatalf("a contained Patient named %v, want no one", got)
	}
	request, _, coverage, payer := eligFAISParts("", "00001")
	identified := func(member string) []byte {
		return eligFAISBundle(t, [2]any{"", request}, [2]any{"urn:uuid:patient", eligIdentifiedPatient(member)},
			[2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer})
	}
	got = g.requestNamedPatients(context.Background(), "coverage-eligibility", "corr-4", eligBatch(t, identified("MBR-COVERED"), identified("MBR-OX")), subject)
	if !slices.Equal(got, want) {
		t.Fatalf("a batch of identified Patients named %v, want %v", got, want)
	}
	// A patient two requests name is named once.
	got = g.requestNamedPatients(context.Background(), "coverage-eligibility", "corr-5", eligBatch(t, identified("MBR-COVERED"), identified("MBR-OX"), eligibilityRequest(t, "MBR-OX")), subject)
	if !slices.Equal(got, want) {
		t.Fatalf("a batch naming one patient twice named %v, want %v", got, want)
	}
	if len(*omitted) != 0 {
		t.Fatalf("omitted %v", *omitted)
	}
}

// A participant that checks members refuses a batch naming a member its
// system does not hold, saying which request; by default it is carried.
func TestEligibilityBundle_UnknownMemberInABatch(t *testing.T) {
	batch := eligBatch(t, eligibilityRequest(t, "MBR-COVERED"), eligibilityRequest(t, "MBR-NOBODY"))
	for _, level := range allLevels {
		t.Run("checked/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.RequireKnownMembers = true
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteEligibility)(rec, eligibilityIngressPost(batch))
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "unknown member (request 1 of the batch)")
		})
		t.Run("carried/"+level.String(), func(t *testing.T) {
			env, rec, _ := runEligibilityIngress(t, level, batch, eligResponses(t, "Patient/MBR-COVERED", "Patient/MBR-NOBODY"))
			if rec.Code != http.StatusOK || !bytes.Equal(env.lastRequestPayload(), batch) {
				t.Fatalf("answer %d %s, want the batch carried", rec.Code, rec.Body)
			}
		})
	}
}

// A Bundle answer is read as its responses: one holding none is no
// eligibility answer, relayed at none and observe and refused at structural
// and strict.
func TestEligibilityBundle_AnswerBundleHoldingNoResponse(t *testing.T) {
	answer := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"OperationOutcome","issue":[]}}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			_, rec, x := runEligibilityIngress(t, level, eligBatch(t, eligibilityRequest(t, "MBR-COVERED"), eligibilityRequest(t, anotherHeldMember)), answer)
			if level == EnforcementNone || level == EnforcementObserve {
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want relayed exactly", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("answer %d %s, want 502", rec.Code, rec.Body)
			}
			wantRefusal(t, x, http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
		})
	}
}

// A payer gateway reads a request submitted in a Bundle by its exact member
// names, as the requester's gateway does: one whose patient or contained
// resources cannot be read one way only, whose patient reference is empty,
// or which names its patient by a reference naming no patient, is refused
// 400 before the payer's system is called, on the records path and the
// forward path, at every level, never bound to an identifier made from the
// reference's text.
func TestEligibilityBundle_PayerReadsBundledPatientsExactly(t *testing.T) {
	request, patient, coverage, payer := eligFAISParts(dtrFrameMember, "00001")
	fais := func(req map[string]any) []byte {
		return eligFAISBundle(t, [2]any{"", req}, [2]any{"urn:uuid:patient", patient}, [2]any{"urn:uuid:coverage", coverage}, [2]any{"urn:uuid:payer", payer})
	}
	cer := eligibilityRequest(t, dtrFrameMember)
	namedInTheRefusal := map[string]string{
		"patient named in another case":                          "member names are case-sensitive, so the request names no patient to bind",
		"reference named in another case":                        "member names are case-sensitive, so the request names no patient to bind",
		"a batch's request with contained named in another case": "member names are case-sensitive, so the request names no patient to bind",
	}
	for name, body := range map[string][]byte{
		"patient named in another case":   bytes.Replace(eligFAIS(t, dtrFrameMember, "00001"), []byte(`"patient":{"reference":"urn:uuid:patient"}`), []byte(`"PATIENT":{"reference":"urn:uuid:patient"}`), 1),
		"reference named in another case": bytes.Replace(eligFAIS(t, dtrFrameMember, "00001"), []byte(`"patient":{"reference":"urn:uuid:patient"}`), []byte(`"patient":{"Reference":"urn:uuid:patient"}`), 1),
		"empty patient reference":         fais(withPatientRef(request, "")),
		"contained that is not a list":    fais(eligRequestWith(request, "contained", "x", "#p")),
		"a batch's request with contained named in another case": eligBatch(t, cer,
			eligBundledOddly(t, request, "Contained", []any{containedMember("p", anotherHeldMember)}, "#p")),
		"a batch's request naming its patient by urn:uuid": eligBatch(t, cer, mustJSON(t, withPatientRef(request, "urn:uuid:patient"))),
		"a batch entry's resource named in another case":   eligBatchHidingSecond(cer, eligibilityRequest(t, anotherHeldMember)),
		"a second request's type named in another case":    fais(map[string]any{"RESOURCETYPE": "CoverageEligibilityRequest", "status": "active"}),
		"a batch's Bundle hiding a request":                eligBatch(t, cer, eligFAISHiding(t, request, patient, eligibilityRequest(t, anotherHeldMember))),
	} {
		for _, declared := range []bool{false, true} {
			for _, level := range eligibilityLevels {
				t.Run(name+"/"+map[bool]string{false: "records", true: "forward"}[declared]+"/"+level.String(), func(t *testing.T) {
					p := eligibilityPayer(t, level, declared)
					ans := p.send(t, "coverage-eligibility", "", body)
					// A request's own member named in another case is named in
					// the refusal; an entry the Bundle cannot be read by is not.
					want := "parse member failed"
					if named, ok := namedInTheRefusal[name]; ok {
						want = named
					}
					if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), want) || p.partner.calls.Load() != 0 {
						t.Fatalf("answer %d %s (payer system calls %d), want 400 %s before the payer's system", ans.status, ans.body, p.partner.calls.Load(), want)
					}
				})
			}
		}
	}
}

// A payer gateway answering from the payer's records reads a FAIS Bundle as
// its one request, and answers about that request's member, at every level.
func TestEligibilityBundle_RecordsAnswerAFAISBundle(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, false)
			ans := p.send(t, "coverage-eligibility", "", eligFAIS(t, dtrFrameMember, "00001"))
			if ref, err := ParseCoverageEligibilityResponsePatient(ans.body); ans.status != http.StatusOK || err != nil || ref != "Patient/"+dtrFrameMember {
				t.Fatalf("answer %d %s, want answered about the Bundle's member", ans.status, ans.body)
			}
		})
	}
}

// A payer gateway answering from the payer's records answers one request at
// a time: a batch is its own 501, saying so as the payer's answer, at every
// level, and recorded under no network rule.
func TestEligibilityBundle_RecordsDoNotAnswerABatch(t *testing.T) {
	const want = `{"error":"this payer answers coverage eligibility from its records one request at a time, so it does not answer a batch of requests: this is the payer's answer, not a network refusal"}`
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			ans := p.send(t, "coverage-eligibility", "", eligBatch(t, eligibilityRequest(t, dtrFrameMember), eligibilityRequest(t, anotherHeldMember)))
			if ans.status != http.StatusNotImplemented || strings.TrimSpace(string(ans.body)) != want {
				t.Fatalf("answer %d %s, want 501 %s", ans.status, ans.body, want)
			}
			if rec := got.only(t); rec.Rule != ExchangeOther {
				t.Fatalf("record names rule %q: the payer gateway's own answer is not a network refusal", rec.Rule)
			}
		})
	}
}

// A payer that declares its own endpoint gets a batch carried as received,
// and its answer relayed. Each request's member is bound by the payer's own
// records: the first request's is the leg's patient, and every other
// request's own binding is named on the answer's involved list, so nothing
// about a request is filed under the token's patient in place of its own.
func TestEligibilityBundle_ForwardedBatchFilesEachRequestUnderItsPatient(t *testing.T) {
	for _, level := range eligibilityLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			other, _, ok := p.store.ResolvePatient(anotherHeldMember)
			if !ok {
				t.Fatalf("%s is not in the test system of record", anotherHeldMember)
			}
			batch := eligBatch(t, eligibilityRequest(t, dtrFrameMember), eligFAIS(t, anotherHeldMember, "00001"))
			answer := eligResponses(t, "Patient/"+dtrFrameMember, "Patient/"+anotherHeldMember)
			p.partner.respByPath[eligibilityPath] = answer
			run := runObserved(t, p, "coverage-eligibility", batch, p.pci)
			if run.answer.status != http.StatusOK || !bytes.Equal(run.answer.body, answer) || !bytes.Equal(p.partner.lastBody, batch) {
				t.Fatalf("answer %d %s; the payer's endpoint received %s; want the batch carried and its answer relayed", run.answer.status, run.answer.body, p.partner.lastBody)
			}
			if run.differs != 0 {
				t.Fatalf("%d binding-differs events: the token names the first request's own patient", run.differs)
			}
			if !containsSuffix(run.authorized, other+"|"+shnsdk.InvolvementPayerHeld) || containsSuffix(run.authorized, p.pci+"|"+shnsdk.InvolvementPayerHeld) {
				t.Fatalf("authorized %v, want only the second request's own binding named", run.authorized)
			}
		})
	}
}

// Each request of a batch is validated at the payer gateway's level: one
// that is invalid is refused at strict, with the payer's system not called,
// whichever request it is.
func TestEligibilityBundle_ForwardedBatchValidatesEachRequest(t *testing.T) {
	p := eligibilityPayer(t, EnforcementStrict, true)
	p.g.cfg.Validator = &shnsdk.FakeValidator{RejectIfContains: `"Patient/` + anotherHeldMember + `"`}
	ans := p.send(t, "coverage-eligibility", "", eligBatch(t, eligibilityRequest(t, dtrFrameMember), eligibilityRequest(t, anotherHeldMember)))
	if ans.status != http.StatusUnprocessableEntity || p.partner.calls.Load() != 0 {
		t.Fatalf("answer %d %s (payer system calls %d), want the batch's invalid second request refused 422", ans.status, ans.body, p.partner.calls.Load())
	}
}

// An answer Bundle whose entries are FAIS response Bundles, each carrying one
// response with the resources it references, is read as those responses, on
// both gateways, at strict.
func TestEligibilityBundle_AnswerOfResponseBundlesIsRead(t *testing.T) {
	inner := func(ref string) json.RawMessage {
		return mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{map[string]any{"resource": json.RawMessage(payersEligibilityAnswer(ref))}}})
	}
	answer := mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
		map[string]any{"resource": inner("Patient/" + dtrFrameMember)}, map[string]any{"resource": inner("Patient/" + anotherHeldMember)}}})
	batch := eligBatch(t, eligibilityRequest(t, dtrFrameMember), eligibilityRequest(t, anotherHeldMember))
	p := eligibilityPayer(t, EnforcementStrict, true)
	p.partner.respByPath[eligibilityPath] = answer
	if ans := p.send(t, "coverage-eligibility", "", batch); ans.status != http.StatusOK || !bytes.Equal(ans.body, answer) {
		t.Fatalf("payer: answer %d %s, want relayed at strict", ans.status, ans.body)
	}
	_, rec, _ := runEligibilityIngress(t, EnforcementStrict, batch, answer)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
		t.Fatalf("provider: answer %d %s, want relayed at strict", rec.Code, rec.Body)
	}
}

// The verdict on a Bundle answer does not depend on the order of its
// responses: one that cannot be read is refused at structural whether it
// comes before or after one naming its patient by identifier only.
func TestEligibilityBundle_ForwardedAnswerVerdictIgnoresOrder(t *testing.T) {
	byIdentifier := json.RawMessage(bytes.Replace(payersEligibilityAnswer("Patient/"+dtrFrameMember),
		[]byte(`"patient" : { "reference" : "Patient/`+dtrFrameMember+`" }`), []byte(`"patient" : { "identifier" : { "value" : "x" } }`), 1))
	unreadable := json.RawMessage(bytes.Replace(payersEligibilityAnswer("Patient/"+dtrFrameMember),
		[]byte(`"reference" : "Patient/`+dtrFrameMember+`"`), []byte(`"reference" : 7`), 1))
	batch := eligBatch(t, eligibilityRequest(t, dtrFrameMember), eligibilityRequest(t, anotherHeldMember))
	for name, entries := range map[string][]any{
		"identifier first": {map[string]any{"resource": byIdentifier}, map[string]any{"resource": unreadable}},
		"unreadable first": {map[string]any{"resource": unreadable}, map[string]any{"resource": byIdentifier}},
	} {
		t.Run(name, func(t *testing.T) {
			p := eligibilityPayer(t, EnforcementStructural, true)
			p.partner.respByPath[eligibilityPath] = mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": entries})
			if ans := p.send(t, "coverage-eligibility", "", batch); ans.status != http.StatusBadGateway {
				t.Fatalf("answer %d %s, want 502 at structural", ans.status, ans.body)
			}
		})
	}
}

// A FAIS response Bundle naming its patient by urn:uuid names the Patient
// entry it carries: the request's patient, relayed at strict.
func TestEligibilityBundle_FAISAnswerNamesItsPatientEntry(t *testing.T) {
	response := payersEligibilityAnswer("urn:uuid:patient")
	answer := eligFAISBundle(t, [2]any{"urn:uuid:response", json.RawMessage(response)},
		[2]any{"urn:uuid:patient", map[string]any{"resourceType": "Patient", "id": dtrFrameMember}})
	p := eligibilityPayer(t, EnforcementStrict, true)
	p.partner.respByPath[eligibilityPath] = answer
	if ans := p.send(t, "coverage-eligibility", "", eligFAIS(t, dtrFrameMember, "00001")); ans.status != http.StatusOK || !bytes.Equal(ans.body, answer) {
		t.Fatalf("answer %d %s, want relayed at strict", ans.status, ans.body)
	}
}

// An answer whose Bundle type is named in another case cannot be read one
// way only: refused at structural, relayed at observe.
func TestEligibilityBundle_AnswerBundleNamedInAnotherCase(t *testing.T) {
	answer := bytes.Replace(eligResponses(t, "Patient/MBR-COVERED"), []byte(`"resourceType":"Bundle"`), []byte(`"RESOURCETYPE":"Bundle"`), 1)
	for level, want := range map[ConformanceEnforcement]int{EnforcementObserve: http.StatusOK, EnforcementStructural: http.StatusBadGateway} {
		t.Run(level.String(), func(t *testing.T) {
			_, rec, _ := runEligibilityIngress(t, level, eligFAIS(t, "MBR-COVERED", "00001"), answer)
			if rec.Code != want {
				t.Fatalf("answer %d %s, want %d", rec.Code, rec.Body, want)
			}
		})
	}
}

// The provider reads an answer Bundle's entries by their exact member names
// too: a response under an entry member named in another case makes the
// answer unreadable, refused at structural and relayed at observe.
func TestEligibilityBundle_ProviderAnswerEntryNamedInAnotherCase(t *testing.T) {
	answer := bytes.Replace(eligResponses(t, "Patient/MBR-COVERED", "Patient/"+anotherHeldMember), []byte(`},{"resource":`), []byte(`},{"Resource":`), 1)
	batch := eligBatch(t, eligibilityRequest(t, "MBR-COVERED"), eligibilityRequest(t, anotherHeldMember))
	for level, want := range map[ConformanceEnforcement]int{EnforcementObserve: http.StatusOK, EnforcementStructural: http.StatusBadGateway} {
		t.Run(level.String(), func(t *testing.T) {
			_, rec, _ := runEligibilityIngress(t, level, batch, answer)
			if rec.Code != want {
				t.Fatalf("answer %d %s, want %d", rec.Code, rec.Body, want)
			}
		})
	}
}

// An answer Bundle entry with a null resource, or a resource with no type,
// cannot be read one way only: the answer is refused at structural and
// relayed at observe, on both gateways, however good its other responses.
func TestEligibilityBundle_AnswerEntryWithNoReadableResource(t *testing.T) {
	batch := eligBatch(t, eligibilityRequest(t, dtrFrameMember), eligibilityRequest(t, anotherHeldMember))
	good := []any{map[string]any{"resource": json.RawMessage(payersEligibilityAnswer("Patient/" + dtrFrameMember))},
		map[string]any{"resource": json.RawMessage(payersEligibilityAnswer("Patient/" + anotherHeldMember))}}
	for name, bad := range map[string]any{
		"null resource":    map[string]any{"resource": nil},
		"untyped resource": map[string]any{"resource": map[string]any{"id": "x"}},
	} {
		answer := mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": append(append([]any{}, good...), bad)})
		for level, want := range map[ConformanceEnforcement]int{EnforcementObserve: http.StatusOK, EnforcementStructural: http.StatusBadGateway} {
			t.Run(name+"/payer/"+level.String(), func(t *testing.T) {
				p := eligibilityPayer(t, level, true)
				p.partner.respByPath[eligibilityPath] = answer
				if ans := p.send(t, "coverage-eligibility", "", batch); ans.status != want {
					t.Fatalf("answer %d %s, want %d", ans.status, ans.body, want)
				}
			})
			t.Run(name+"/provider/"+level.String(), func(t *testing.T) {
				_, rec, _ := runEligibilityIngress(t, level, batch, answer)
				if rec.Code != want {
					t.Fatalf("answer %d %s, want %d", rec.Code, rec.Body, want)
				}
			})
		}
	}
}

// A payer's own Bundle answer is fenced response by response: one about a
// patient none of the requests names is refused at strict and relayed below
// it, and a Bundle holding no response is refused at structural and strict.
func TestEligibilityBundle_ForwardedBundleAnswerIsFenced(t *testing.T) {
	batch := eligBatch(t, eligibilityRequest(t, dtrFrameMember), eligibilityRequest(t, anotherHeldMember))
	stranger := eligResponses(t, "Patient/"+dtrFrameMember, "Patient/MBR-SOMEONE-ELSE")
	empty := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	for _, level := range eligibilityLevels {
		t.Run("another patient/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.partner.respByPath[eligibilityPath] = stranger
			ans := p.send(t, "coverage-eligibility", "", batch)
			if level == EnforcementStrict {
				if ans.status != http.StatusForbidden {
					t.Fatalf("answer %d %s, want 403 at strict", ans.status, ans.body)
				}
				return
			}
			if ans.status != http.StatusOK || !bytes.Equal(ans.body, stranger) {
				t.Fatalf("answer %d %s, want relayed below strict", ans.status, ans.body)
			}
		})
		t.Run("no response/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.partner.respByPath[eligibilityPath] = empty
			ans := p.send(t, "coverage-eligibility", "", batch)
			if level == EnforcementStructural || level == EnforcementStrict {
				if ans.status != http.StatusBadGateway {
					t.Fatalf("answer %d %s, want 502", ans.status, ans.body)
				}
				return
			}
			if ans.status != http.StatusOK || !bytes.Equal(ans.body, empty) {
				t.Fatalf("answer %d %s, want relayed", ans.status, ans.body)
			}
		})
	}
	// A Bundle nested deeper than a batch of response Bundles is not read
	// past: the answer cannot be read one way only, refused at structural
	// and strict on both gateways, and relayed below.
	deep := mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
		map[string]any{"resource": json.RawMessage(payersEligibilityAnswer("Patient/" + dtrFrameMember))},
		map[string]any{"resource": map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
			map[string]any{"resource": json.RawMessage(eligResponses(t, "Patient/MBR-SOMEONE-ELSE"))}}}}}})
	for _, level := range eligibilityLevels {
		refused := level == EnforcementStructural || level == EnforcementStrict
		t.Run("nested too deep/payer/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.partner.respByPath[eligibilityPath] = deep
			ans := p.send(t, "coverage-eligibility", "", batch)
			if refused != (ans.status == http.StatusBadGateway) || (!refused && !bytes.Equal(ans.body, deep)) {
				t.Fatalf("payer: answer %d %s, want refused %v", ans.status, ans.body, refused)
			}
		})
		t.Run("nested too deep/provider/"+level.String(), func(t *testing.T) {
			_, rec, _ := runEligibilityIngress(t, level, batch, deep)
			if refused != (rec.Code == http.StatusBadGateway) || (!refused && !bytes.Equal(rec.Body.Bytes(), deep)) {
				t.Fatalf("provider: answer %d %s, want refused %v", rec.Code, rec.Body, refused)
			}
		})
	}
	// A response under an entry member named in another case is not
	// skipped: the answer cannot be read one way only.
	hidden := bytes.Replace(eligResponses(t, "Patient/"+dtrFrameMember, "Patient/MBR-SOMEONE-ELSE"), []byte(`},{"resource":`), []byte(`},{"Resource":`), 1)
	for _, level := range eligibilityLevels {
		refused := level == EnforcementStructural || level == EnforcementStrict
		t.Run("response entry named in another case/"+level.String(), func(t *testing.T) {
			p := eligibilityPayer(t, level, true)
			p.partner.respByPath[eligibilityPath] = hidden
			ans := p.send(t, "coverage-eligibility", "", batch)
			if refused != (ans.status == http.StatusBadGateway) || (!refused && !bytes.Equal(ans.body, hidden)) {
				t.Fatalf("answer %d %s, want refused %v", ans.status, ans.body, refused)
			}
		})
	}
	// A batch-response entry carrying only its outcome is no response, and
	// does not make the answer unreadable: relayed at strict.
	outcomeOnly := []byte(`{"resourceType":"Bundle","type":"batch-response","entry":[{"resource":` + string(payersEligibilityAnswer("Patient/"+dtrFrameMember)) +
		`},{"response":{"status":"422","outcome":{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}}}]}`)
	t.Run("outcome-only entry/strict", func(t *testing.T) {
		p := eligibilityPayer(t, EnforcementStrict, true)
		p.partner.respByPath[eligibilityPath] = outcomeOnly
		if ans := p.send(t, "coverage-eligibility", "", batch); ans.status != http.StatusOK || !bytes.Equal(ans.body, outcomeOnly) {
			t.Fatalf("payer: answer %d %s, want relayed at strict", ans.status, ans.body)
		}
		_, rec, _ := runEligibilityIngress(t, EnforcementStrict, batch, outcomeOnly)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), outcomeOnly) {
			t.Fatalf("provider: answer %d %s, want relayed at strict", rec.Code, rec.Body)
		}
	})
	t.Run("each patient its own/strict", func(t *testing.T) {
		p := eligibilityPayer(t, EnforcementStrict, true)
		answer := eligResponses(t, "Patient/"+dtrFrameMember, "Patient/"+anotherHeldMember)
		p.partner.respByPath[eligibilityPath] = answer
		if ans := p.send(t, "coverage-eligibility", "", batch); ans.status != http.StatusOK || !bytes.Equal(ans.body, answer) {
			t.Fatalf("answer %d %s, want relayed at strict", ans.status, ans.body)
		}
	})
}
