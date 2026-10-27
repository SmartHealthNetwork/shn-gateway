//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The participant's own records an order's requester can lead to.
const (
	reqPractitioner = `{"resourceType":"Practitioner","id":"prac-1","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1000000046"}],"name":[{"family":"Ordering"}]}`
	reqIssued       = `{"resourceType":"Practitioner","id":"prac-2","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1000000061"}],"qualification":[{"code":{"text":"MD"},"issuer":{"reference":"Organization/board-1"}}]}`
	reqBoard        = `{"resourceType":"Organization","id":"board-1","name":"Medical Board"}`
	reqGroup        = `{"resourceType":"Organization","id":"grp-1","name":"Ordering Group"}`
	reqLocation     = `{"resourceType":"Location","id":"loc-1","name":"Clinic","position":{"longitude":-71.0589,"latitude":42.3601}}`
	reqRole         = `{"resourceType":"PractitionerRole","id":"role-1","practitioner":{"reference":"Practitioner/prac-2"},"organization":{"reference":"Organization/grp-1"},"location":[{"reference":"Location/loc-1"}]}`
)

// requesterSoR is a system of record holding the records above, with
// overrides (a nil value removes a record).
func requesterSoR(overrides map[string]string) *prefetchSoR {
	s := newPrefetchSoR()
	for ref, rec := range map[string]string{
		"Practitioner/prac-1": reqPractitioner, "Practitioner/prac-2": reqIssued,
		"Organization/board-1": reqBoard, "Organization/grp-1": reqGroup,
		"Location/loc-1": reqLocation, "PractitionerRole/role-1": reqRole,
	} {
		s.reads[ref] = []byte(rec)
	}
	for ref, rec := range overrides {
		if rec == "" {
			delete(s.reads, ref)
			continue
		}
		s.reads[ref] = []byte(rec)
	}
	return s
}

// orderNaming is an order whose requester is ref ("" names none).
func orderNaming(ref string) []byte {
	requester := ""
	if ref != "" {
		requester = `"requester":{"reference":"` + ref + `"},`
	}
	return []byte(`{"resourceType":"ServiceRequest","id":"sr-1","status":"active","intent":"order",` + requester +
		`"subject":{"reference":"Patient/MBR-1"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}`)
}

func recordKeys(t *testing.T, records [][]byte) []string {
	t.Helper()
	var out []string
	for _, r := range records {
		var h struct{ ResourceType, ID string }
		if err := json.Unmarshal(r, &h); err != nil {
			t.Fatal(err)
		}
		out = append(out, h.ResourceType+"/"+h.ID)
	}
	return out
}

// TestOrderRequesterRecords: the records an order's requester leads to are
// read from the participant's own system, to depth 2 over the closed set,
// each once, requester first; anything the gateway cannot carry faithfully is
// refused by name, and a failing system of record keeps its own answer.
func TestOrderRequesterRecords(t *testing.T) {
	const notIn = ", which is not in your system of record, so a prior-authorization request about this order cannot carry it"
	const onlySet = "; a prior-authorization request this gateway builds carries only Practitioner, PractitionerRole, Organization and Location records for the clinician an order names"
	manyLocations := `{"resourceType":"PractitionerRole","id":"role-1","location":[`
	many := map[string]string{}
	for i := 0; i < requesterCarryMax; i++ {
		if i > 0 {
			manyLocations += ","
		}
		manyLocations += fmt.Sprintf(`{"reference":"Location/l-%02d"}`, i)
		many[fmt.Sprintf("Location/l-%02d", i)] = fmt.Sprintf(`{"resourceType":"Location","id":"l-%02d"}`, i)
	}
	many["PractitionerRole/role-1"] = manyLocations + `]}`
	// At the bound: the role and 15 locations are 16 records, all carried.
	atBound := map[string]string{}
	atBoundRole := `{"resourceType":"PractitionerRole","id":"role-1","location":[`
	atBoundWant := []string{"PractitionerRole/role-1"}
	for i := 0; i < requesterCarryMax-1; i++ {
		if i > 0 {
			atBoundRole += ","
		}
		atBoundRole += fmt.Sprintf(`{"reference":"Location/l-%02d"}`, i)
		atBound[fmt.Sprintf("Location/l-%02d", i)] = fmt.Sprintf(`{"resourceType":"Location","id":"l-%02d"}`, i)
		atBoundWant = append(atBoundWant, fmt.Sprintf("Location/l-%02d", i))
	}
	atBound["PractitionerRole/role-1"] = atBoundRole + `]}`
	for _, row := range []struct {
		name      string
		requester string
		overrides map[string]string
		readErrs  map[string]error
		want      []string
		status    int
		fidelity  bool
		msg       string
	}{
		{name: "no requester", requester: ""},
		{name: "a contained requester travels in the order", requester: "#c-1"},
		{name: "a Practitioner", requester: "Practitioner/prac-1", want: []string{"Practitioner/prac-1"}},
		{name: "a Practitioner and its qualification issuer", requester: "Practitioner/prac-2", want: []string{"Practitioner/prac-2", "Organization/board-1"}},
		{name: "a PractitionerRole, its references, and its practitioner's issuer at depth 2", requester: "PractitionerRole/role-1",
			want: []string{"PractitionerRole/role-1", "Location/loc-1", "Organization/grp-1", "Practitioner/prac-2", "Organization/board-1"}},
		{name: "each record once", requester: "PractitionerRole/role-1",
			overrides: map[string]string{"PractitionerRole/role-1": `{"resourceType":"PractitionerRole","id":"role-1","practitioner":{"reference":"Practitioner/prac-2"},"organization":{"reference":"Organization/board-1"}}`},
			want:      []string{"PractitionerRole/role-1", "Organization/board-1", "Practitioner/prac-2"}},
		{name: "an Organization", requester: "Organization/grp-1", want: []string{"Organization/grp-1"}},
		{name: "deeper than 2", requester: "PractitionerRole/role-1",
			overrides: map[string]string{"Organization/board-1": `{"resourceType":"Organization","id":"board-1","partOf":{"reference":"Organization/board-root"}}`},
			status:    http.StatusUnprocessableEntity, fidelity: true,
			msg: "the order's requester PractitionerRole/role-1 leads to Organization/board-root, more than 2 references away from the requester; a prior-authorization request this gateway builds carries the requester's records to that depth"},
		{name: "the requester is not in the system of record", requester: "Practitioner/prac-9", status: http.StatusUnprocessableEntity, fidelity: true,
			msg: "the order's requester Practitioner/prac-9 is not in your system of record, so a prior-authorization request about this order cannot carry the clinician it names"},
		{name: "a record it leads to is not in the system of record", requester: "Practitioner/prac-2",
			overrides: map[string]string{"Organization/board-1": ""}, status: http.StatusUnprocessableEntity, fidelity: true,
			msg: "the order's requester Practitioner/prac-2 leads to Organization/board-1" + notIn},
		{name: "the requester is outside the set", requester: "Patient/MBR-1", status: http.StatusUnprocessableEntity, fidelity: true,
			msg: "the order's requester Patient/MBR-1 is a Patient" + onlySet},
		{name: "a record it leads to is outside the set", requester: "PractitionerRole/role-1",
			overrides: map[string]string{"PractitionerRole/role-1": `{"resourceType":"PractitionerRole","id":"role-1","healthcareService":[{"reference":"HealthcareService/hs-1"}]}`},
			status:    http.StatusUnprocessableEntity, fidelity: true,
			msg: "the order's requester PractitionerRole/role-1 leads to HealthcareService/hs-1, a HealthcareService" + onlySet},
		{name: "the requester is not a reference into the system of record", requester: "https://elsewhere.example/fhir/Practitioner/p-1", status: http.StatusUnprocessableEntity, fidelity: true,
			msg: "the order's requester https://elsewhere.example/fhir/Practitioner/p-1 is not a Type/id reference into your system of record, so a prior-authorization request about this order cannot carry the clinician it names"},
		{name: "a record it leads to is not a reference into the system of record", requester: "Practitioner/prac-2",
			overrides: map[string]string{"Practitioner/prac-2": `{"resourceType":"Practitioner","id":"prac-2","qualification":[{"issuer":{"reference":"https://elsewhere.example/fhir/Organization/b"}}]}`},
			status:    http.StatusUnprocessableEntity, fidelity: true,
			msg: "the order's requester Practitioner/prac-2 leads to https://elsewhere.example/fhir/Organization/b, which is not a Type/id reference into your system of record, so a prior-authorization request about this order cannot carry it"},
		{name: "records at the bound", requester: "PractitionerRole/role-1", overrides: atBound, want: atBoundWant},
		{name: "more records than the bound", requester: "PractitionerRole/role-1", overrides: many, status: http.StatusUnprocessableEntity, fidelity: true,
			msg: fmt.Sprintf("the order's requester PractitionerRole/role-1 leads to more than %d records; a prior-authorization request this gateway builds carries at most %d for the clinician an order names", requesterCarryMax, requesterCarryMax)},
		{name: "the system answers with another record", requester: "Practitioner/prac-1",
			overrides: map[string]string{"Practitioner/prac-1": `{"resourceType":"Practitioner","id":"prac-other"}`}},
		{name: "the system of record is unavailable", requester: "Practitioner/prac-1",
			readErrs: map[string]error{"Practitioner/prac-1": &SoRReadError{Kind: SoRUnavailable}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			sor := requesterSoR(row.overrides)
			sor.readErrs = row.readErrs
			g := &Gateway{cfg: Config{SoR: sor}}
			records, refusal := g.orderRequesterRecords(context.Background(), orderNaming(row.requester))
			switch {
			case row.readErrs != nil:
				status, msg := SoRFailureResponse(row.readErrs[row.requester])
				if refusal == nil || refusal.fidelity || refusal.status != status || refusal.msg != msg {
					t.Fatalf("refusal %+v, want the system of record's own %d %q", refusal, status, msg)
				}
			case row.name == "the system answers with another record":
				status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
				if refusal == nil || refusal.fidelity || refusal.status != status || refusal.msg != msg {
					t.Fatalf("refusal %+v, want the system of record's own %d %q", refusal, status, msg)
				}
			case row.msg != "":
				if refusal == nil || !refusal.fidelity || refusal.status != row.status || refusal.msg != row.msg {
					t.Fatalf("refusal %+v\nwant %d %q", refusal, row.status, row.msg)
				}
				if records != nil {
					t.Fatalf("records %v with a refusal", recordKeys(t, records))
				}
			default:
				if refusal != nil {
					t.Fatalf("refused: %+v", refusal)
				}
				if got := recordKeys(t, records); strings.Join(got, ",") != strings.Join(row.want, ",") {
					t.Fatalf("records %v, want %v", got, row.want)
				}
				for i, r := range records {
					if !bytes.Equal(r, sor.reads[row.want[i]]) {
						t.Fatalf("%s is not the system of record's bytes: %s", row.want[i], r)
					}
				}
			}
			if row.requester == "" || strings.HasPrefix(row.requester, "#") {
				if len(sor.read) != 0 {
					t.Fatalf("read %v for an order naming no record", sor.read)
				}
			}
		})
	}
}

// withOrderRequester names ref as the order's requester.
func withOrderRequester(t *testing.T, order []byte, ref string) []byte {
	t.Helper()
	var o map[string]any
	if err := json.Unmarshal(order, &o); err != nil {
		t.Fatal(err)
	}
	o["requester"] = map[string]any{"reference": ref}
	out, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type carriedEntry struct {
	FullURL  string          `json:"fullUrl"`
	Resource json.RawMessage `json:"resource"`
}

func carriedEntries(t *testing.T, body []byte) map[string]carriedEntry {
	t.Helper()
	var b struct {
		Entry []carriedEntry `json:"entry"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	out := map[string]carriedEntry{}
	for _, e := range b.Entry {
		var h struct{ ResourceType, ID string }
		if err := json.Unmarshal(e.Resource, &h); err != nil {
			t.Fatal(err)
		}
		key := h.ResourceType + "/" + h.ID
		if _, dup := out[key]; dup {
			t.Fatalf("%s is in the request twice", key)
		}
		out[key] = e
	}
	return out
}

func referenceAt(t *testing.T, resource json.RawMessage, path ...string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(resource, &v); err != nil {
		t.Fatal(err)
	}
	for _, p := range path {
		switch x := v.(type) {
		case map[string]any:
			v = x[p]
		case []any:
			v = x[0]
			if m, ok := v.(map[string]any); ok {
				v = m[p]
			}
		}
	}
	s, _ := v.(string)
	return s
}

// TestModuleOnePAS_CarriesTheOrdersRequester: a PAS submit and update about an
// order naming its clinician carry the records that clinician leads to, each
// once, on the order entry's base, verbatim apart from the references this
// change makes resolvable (written absolute on the reference payer's lane, as
// every other reference to an entry is); and a request about an order naming
// no clinician is byte for byte what the authored builder makes.
func TestModuleOnePAS_CarriesTheOrdersRequester(t *testing.T) {
	sor := requesterSoR(nil)
	g := &Gateway{cfg: Config{SoR: sor}}
	for _, absolute := range []bool{false, true} {
		for _, kind := range []string{"submit", "update"} {
			t.Run(fmt.Sprintf("%s/absolute=%v", kind, absolute), func(t *testing.T) {
				build := func(order []byte, records [][]byte) ([]byte, error) {
					if kind == "submit" {
						in := attachmentInputs("ServiceRequest", "2.2", absolute)
						in.SR = order
						return buildModuleOnePASSubmit("2.2", in, records)
					}
					in := attachmentUpdateInputs("2.2", absolute, false)
					in.SR = order
					return buildModuleOnePASUpdate("2.2", in, records)
				}
				plain := attachmentInputs("ServiceRequest", "2.2", absolute).SR
				// No requester: byte for byte the authored build.
				got, err := build(plain, nil)
				if err != nil {
					t.Fatal(err)
				}
				var want []byte
				if kind == "submit" {
					want, err = buildAuthoredPASSubmit("2.2", attachmentInputs("ServiceRequest", "2.2", absolute))
				} else {
					want, err = buildAuthoredPASUpdate("2.2", attachmentUpdateInputs("2.2", absolute, false))
				}
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("an order naming no requester changed the request (err %v)", err)
				}

				order := withOrderRequester(t, plain, "PractitionerRole/role-1")
				records, refusal := g.orderRequesterRecords(context.Background(), order)
				if refusal != nil {
					t.Fatal(refusal.msg)
				}
				without, err := build(order, nil)
				if err != nil {
					t.Fatal(err)
				}
				body, err := build(order, records)
				if err != nil {
					t.Fatal(err)
				}
				before, after := carriedEntries(t, without), carriedEntries(t, body)
				orderEntry := after["ServiceRequest/"+referenceKeyID(t, before, "ServiceRequest")]
				base := strings.TrimSuffix(orderEntry.FullURL, "/ServiceRequest/"+referenceKeyID(t, before, "ServiceRequest"))
				for _, key := range []string{"PractitionerRole/role-1", "Location/loc-1", "Organization/grp-1", "Practitioner/prac-2", "Organization/board-1"} {
					e, ok := after[key]
					if !ok {
						t.Fatalf("%s not carried", key)
					}
					if e.FullURL != base+"/"+key {
						t.Fatalf("%s at %s, want the order's base %s", key, e.FullURL, base)
					}
					if !absolute && !sameJSONResource(e.Resource, sor.reads[key]) {
						t.Fatalf("%s carried as %s, want the participant's own record", key, e.Resource)
					}
				}
				if len(after) != len(before)+5 {
					t.Fatalf("%d entries, want %d: only the five records are added", len(after), len(before)+5)
				}
				for key, e := range before {
					if strings.HasPrefix(key, "ServiceRequest/") {
						continue
					}
					if after[key].FullURL != e.FullURL || !bytes.Equal(after[key].Resource, e.Resource) {
						t.Fatalf("%s changed: %s", key, after[key].Resource)
					}
				}
				wantRef := func(ref string) string {
					if absolute {
						return base + "/" + ref
					}
					return ref
				}
				if got := referenceAt(t, orderEntry.Resource, "requester", "reference"); got != wantRef("PractitionerRole/role-1") {
					t.Fatalf("order requester %q, want %q", got, wantRef("PractitionerRole/role-1"))
				}
				if got, was := referenceAt(t, orderEntry.Resource, "subject", "reference"), referenceAt(t, before["ServiceRequest/"+referenceKeyID(t, before, "ServiceRequest")].Resource, "subject", "reference"); got != was {
					t.Fatalf("order subject %q, was %q: the subject anchor stays as built", got, was)
				}
				if got := referenceAt(t, after["PractitionerRole/role-1"].Resource, "practitioner", "reference"); got != wantRef("Practitioner/prac-2") {
					t.Fatalf("role practitioner %q", got)
				}
				if got := referenceAt(t, after["Practitioner/prac-2"].Resource, "qualification", "issuer", "reference"); got != wantRef("Organization/board-1") {
					t.Fatalf("qualification issuer %q", got)
				}
				if !bytes.Contains(after["Location/loc-1"].Resource, []byte(`-71.0589`)) {
					t.Fatalf("location position not carried as written: %s", after["Location/loc-1"].Resource)
				}
			})
		}
	}
}

// referenceKeyID is the id of the one entry of type typ.
func referenceKeyID(t *testing.T, entries map[string]carriedEntry, typ string) string {
	t.Helper()
	for key := range entries {
		if strings.HasPrefix(key, typ+"/") {
			return strings.TrimPrefix(key, typ+"/")
		}
	}
	t.Fatalf("no %s entry", typ)
	return ""
}

// REJECTION: the carry refuses records the order does not name as its
// requester, and a record already in the request with other content; a record
// already in the request with the same content (a PractitionerRole the
// request names as its provider) is carried once.
func TestModuleOnePAS_CarryRefusesWhatTheOrderDoesNotName(t *testing.T) {
	in := attachmentInputs("ServiceRequest", "2.2", true)
	in.SR = withOrderRequester(t, in.SR, "Practitioner/prac-2")
	if _, err := buildModuleOnePASSubmit("2.2", in, [][]byte{[]byte(reqPractitioner)}); err == nil || !strings.Contains(err.Error(), "does not name the carried record Practitioner/prac-1") {
		t.Fatalf("a record the order does not name: err %v", err)
	}

	in = attachmentInputs("ServiceRequest", "2.2", true)
	in.SR = withOrderRequester(t, in.SR, "PractitionerRole/role-1")
	in.Provider = []byte(reqRole)
	body, err := buildModuleOnePASSubmit("2.2", in, [][]byte{[]byte(reqRole)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := carriedEntries(t, body)["PractitionerRole/role-1"]; !ok {
		t.Fatal("the role is not in the request")
	}
	in.Provider = []byte(`{"resourceType":"PractitionerRole","id":"role-1","telecom":[{"system":"phone","value":"5550100"}]}`)
	if _, err := buildModuleOnePASSubmit("2.2", in, [][]byte{[]byte(reqRole)}); err == nil || !strings.Contains(err.Error(), "conflicting entry for PractitionerRole/role-1") {
		t.Fatalf("a different record under the same identity: err %v", err)
	}
}

// The provider-data evidence guard passes a request that carries its order's
// clinician, because the reference resolves, and refuses the same request
// without the carry: the guard is not relaxed, the reference is resolved.
func TestModuleOnePAS_EvidenceGuardPassesTheCarriedRequester(t *testing.T) {
	sor := requesterSoR(nil)
	g := &Gateway{cfg: Config{SoR: sor, OriginationProfile: "provider-data"}}
	// A request the guard closes today (as its own rows build it), whose
	// order now names a clinician.
	in := attachmentInputs("ServiceRequest", "2.2", true)
	in.SR = bytes.ReplaceAll(in.SR, []byte("Patient/MBR-COVERED"), []byte(in.PatientRef))
	qc := contextInputs()
	qc.PatientRef, qc.CoverageRef, qc.OrderRef = in.PatientRef, in.CoverageRef, "ServiceRequest/source-order"
	qr, err := composeDTRContextAtLine(bytes.ReplaceAll([]byte(contextQR), []byte("Patient/synthetic"), []byte(in.PatientRef)), "2.2", qc)
	if err != nil {
		t.Fatal(err)
	}
	in.QR = qr
	plain, err := buildModuleOnePASSubmit("2.2", in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.completeAuthoredPASRequest(context.Background(), plain, in.QR, in.SR, in.CoverageRef, true); err != nil {
		t.Fatalf("the request without a requester fails the guard: %v", err)
	}
	in.SR = withOrderRequester(t, in.SR, "Practitioner/prac-2")
	records, refusal := g.orderRequesterRecords(context.Background(), in.SR)
	if refusal != nil {
		t.Fatal(refusal.msg)
	}
	without, err := buildModuleOnePASSubmit("2.2", in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.completeAuthoredPASRequest(context.Background(), without, in.QR, in.SR, in.CoverageRef, true); err == nil {
		t.Fatal("a request naming a clinician it does not carry passed the evidence guard")
	}
	with, err := buildModuleOnePASSubmit("2.2", in, records)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.completeAuthoredPASRequest(context.Background(), with, in.QR, in.SR, in.CoverageRef, true); err != nil {
		t.Fatalf("the carried request failed the evidence guard: %v", err)
	}
}

// REJECTION, at every level: an origination whose order names a clinician the
// participant's system cannot supply is refused before any leg, 422, and
// recorded as the PAS leg failing on the fidelity of SHN's own build.
func TestOrigination_RequesterUnavailableRefusesBeforeAnyLeg(t *testing.T) {
	_, _, base := originRecords(prefetchMember)
	order := bytes.Replace(base, []byte(`"intent" : "order",`), []byte(`"intent" : "order", "requester" : { "reference" : "Practitioner/prac-9" },`), 1)
	const want = "the order's requester Practitioner/prac-9 is not in your system of record, so a prior-authorization request about this order cannot carry the clinician it names"
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			sor.order = order
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				return 0, payerAnswerFor(t, order, shnsdk.CoveredNotCovered, "", "")
			})
			g.cfg.ConformanceEnforcement = level
			var events []ObserverEvent
			g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			rec := httptest.NewRecorder()
			g.runCRDThenDTROrder(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(testStatedBody(g, prefetchMember, "", false))), prefetchMember, "", "", "", "", false)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, want)
			}
			payer.mu.Lock()
			sent := len(payer.requests)
			payer.mu.Unlock()
			if sent != 0 {
				t.Fatalf("%d legs sent before the refusal", sent)
			}
			var failed bool
			for _, e := range events {
				if e.Kind == "leg.failed" && e.LegType == "pas-claim" && e.Detail == want {
					failed = true
				}
				if e.Kind == "leg.originated" {
					t.Fatalf("a leg was originated: %+v", e)
				}
			}
			if !failed {
				t.Fatalf("no pas-claim leg.failed naming the refusal in %+v", events)
			}
		})
	}
}

// guardInputs is a request the provider-data evidence guard closes today (as
// its own rows build it), whose questionnaire response is qr.
func guardInputs(t *testing.T, qr string) shnsdk.ConformantClaimInputs {
	t.Helper()
	in := attachmentInputs("ServiceRequest", "2.2", true)
	in.SR = bytes.ReplaceAll(in.SR, []byte("Patient/MBR-COVERED"), []byte(in.PatientRef))
	qc := contextInputs()
	qc.PatientRef, qc.CoverageRef, qc.OrderRef = in.PatientRef, in.CoverageRef, "ServiceRequest/source-order"
	composed, err := composeDTRContextAtLine(bytes.ReplaceAll([]byte(qr), []byte("Patient/synthetic"), []byte(in.PatientRef)), "2.2", qc)
	if err != nil {
		t.Fatal(err)
	}
	in.QR = composed
	return in
}

// A questionnaire response whose author is the clinician the order names
// resolves to the same carried entry, written as the other references to that
// entry are, and the evidence guard closes the request, for the submission and
// for the amendment.
func TestModuleOnePAS_AuthorNamingTheCarriedClinician(t *testing.T) {
	sor := requesterSoR(nil)
	g := &Gateway{cfg: Config{SoR: sor, OriginationProfile: "provider-data"}}
	qr := strings.Replace(contextQR, `"status":"in-progress",`, `"status":"in-progress","author":{"reference":"Practitioner/prac-2"},`, 1)
	in := guardInputs(t, qr)
	in.SR = withOrderRequester(t, in.SR, "Practitioner/prac-2")
	records, refusal := g.orderRequesterRecords(context.Background(), in.SR)
	if refusal != nil {
		t.Fatal(refusal.msg)
	}
	update := attachmentUpdateInputs("2.2", true, false)
	update.QR, update.SR = in.QR, in.SR
	for name, build := range map[string]func() ([]byte, error){
		"submit": func() ([]byte, error) { return buildModuleOnePASSubmit("2.2", in, records) },
		"update": func() ([]byte, error) { return buildModuleOnePASUpdate("2.2", update, records) },
	} {
		t.Run(name, func(t *testing.T) {
			body, err := build()
			if err != nil {
				t.Fatal(err)
			}
			entries := carriedEntries(t, body)
			full := entries["Practitioner/prac-2"].FullURL
			var author string
			for key, e := range entries {
				if strings.HasPrefix(key, "QuestionnaireResponse/") {
					author = referenceAt(t, e.Resource, "author", "reference")
				}
			}
			if full == "" || author != full {
				t.Fatalf("questionnaire author %q, want the carried entry %q", author, full)
			}
			if name == "submit" {
				if _, err := g.completeAuthoredPASRequest(context.Background(), body, in.QR, in.SR, in.CoverageRef, true); err != nil {
					t.Fatalf("the evidence guard refused the carried request: %v", err)
				}
			}
		})
	}
}

// The provider the request names is the record the order's requester led to,
// as read before any leg, never a second reading of it: a held record answers
// even when the system of record has since changed it.
func TestPASProvider_PrefersTheHeldRequesterRecords(t *testing.T) {
	changed := `{"resourceType":"PractitionerRole","id":"role-1","practitioner":{"reference":"Practitioner/prac-2"},"telecom":[{"system":"phone","value":"5550100"}]}`
	g := &Gateway{cfg: Config{SoR: requesterSoR(map[string]string{"PractitionerRole/role-1": changed})}}
	ref, provider, status, msg := g.pasProvider(context.Background(), orderNaming("PractitionerRole/role-1"), []byte(reqRole))
	if status != 0 || ref != "PractitionerRole/role-1" || string(provider) != reqRole {
		t.Fatalf("provider %s %s (%d %s), want the held record", ref, provider, status, msg)
	}
}

// requesterCensusSoR is the census fixture whose open orders name
// PractitionerRole/role-1, with the records it leads to; records can change
// between calls, and reads are counted.
type requesterCensusSoR struct {
	*censusSoR
	mu      sync.Mutex
	records map[string][]byte
	reads   map[string]int
	// requester is the reference the orders name ("" names PractitionerRole/role-1).
	requester string
}

func (s *requesterCensusSoR) OpenOrder(member string) ([]byte, bool) {
	order, ok := s.censusSoR.OpenOrder(member)
	if !ok {
		return order, ok
	}
	var o map[string]any
	if json.Unmarshal(order, &o) != nil {
		return order, ok
	}
	ref := s.requester
	if ref == "" {
		ref = "PractitionerRole/role-1"
	}
	o["requester"] = map[string]any{"reference": ref}
	out, _ := json.Marshal(o)
	return out, true
}

func (s *requesterCensusSoR) ResolveByReference(ref string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.records[ref]; ok {
		s.reads[ref]++
		return b, true
	}
	return s.censusSoR.ResolveByReference(ref)
}

// A pended UC-06 resumes with the clinician the submission carried: the
// records are read once, at origination, and the amendment carries the same
// bytes even after the participant's system changed the role.
func TestUC06Resume_CarriesTheClinicianTheSubmissionCarried(t *testing.T) {
	gw, stub := uc06RouteFixture(t, "2.0")
	sor := &requesterCensusSoR{censusSoR: gw.cfg.SoR.(*censusSoR), reads: map[string]int{}, records: map[string][]byte{
		"PractitionerRole/role-1": []byte(reqRole), "Practitioner/prac-2": []byte(reqIssued), "Organization/board-1": []byte(reqBoard),
		"Organization/grp-1": []byte(reqGroup), "Location/loc-1": []byte(reqLocation),
	}}
	gw.cfg.SoR = sor
	start := startUC06(gw, testRouteBody(gw, "/scenario/uc06/start", ""))
	var started startResp
	if start.Code != 200 || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
		t.Fatalf("start %d %s", start.Code, start.Body.String())
	}
	sor.mu.Lock()
	sor.records["PractitionerRole/role-1"] = []byte(`{"resourceType":"PractitionerRole","id":"role-1","practitioner":{"reference":"Practitioner/prac-2"},"telecom":[{"system":"phone","value":"5550100"}]}`)
	sor.mu.Unlock()
	rec := httptest.NewRecorder()
	gw.handleUC06Complete(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete",
		strings.NewReader(testRouteBody(gw, "/scenario/uc06/complete", `"resumeToken":"`+started.ResumeToken+`"`)))))
	if rec.Code != 200 {
		t.Fatalf("complete %d %s", rec.Code, rec.Body.String())
	}
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		sent := stub.requests[leg]
		if len(sent) != 1 {
			t.Fatalf("%d %s requests", len(sent), leg)
		}
		role, ok := carriedEntries(t, sent[0])["PractitionerRole/role-1"]
		if !ok || referenceAt(t, role.Resource, "telecom", "value") != "" {
			t.Fatalf("%s carries role %s, want the record read at origination", leg, role.Resource)
		}
	}
	sor.mu.Lock()
	defer sor.mu.Unlock()
	if n := sor.reads["PractitionerRole/role-1"]; n != 1 {
		t.Fatalf("the role was read %d times, want once, before any leg", n)
	}
}

// A dispatched order's supplier that is also among the records the order's
// requester leads to is carried once: the supplier merge recognises the
// carried entry, its references written absolute, as the same record.
func TestModuleOnePAS_SupplierAmongTheRequesterRecords(t *testing.T) {
	supplier := `{"resourceType":"Organization","id":"org-dme-ox","name":"Acme Home Medical","partOf":{"reference":"Organization/grp-1"}}`
	sor := requesterSoR(map[string]string{
		"Practitioner/prac-s":     `{"resourceType":"Practitioner","id":"prac-s","qualification":[{"issuer":{"reference":"Organization/org-dme-ox"}}]}`,
		"Organization/org-dme-ox": supplier,
	})
	g := &Gateway{cfg: Config{SoR: sor}}
	in := attachmentInputs("DeviceRequest", "2.2", true)
	in.SR = withOrderRequester(t, in.SR, "Practitioner/prac-s")
	records, refusal := g.orderRequesterRecords(context.Background(), in.SR)
	if refusal != nil {
		t.Fatal(refusal.msg)
	}
	if got := strings.Join(recordKeys(t, records), ","); got != "Practitioner/prac-s,Organization/org-dme-ox,Organization/grp-1" {
		t.Fatalf("records %s", got)
	}
	body, err := buildModuleOnePASSubmit("2.2", in, records)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := retainPASSubmitSupplier(body, []byte(supplier))
	if err != nil {
		t.Fatalf("supplier merge: %v", err)
	}
	if !bytes.Equal(merged, body) {
		t.Fatal("the supplier, already carried, was added again")
	}
	// A different record under the supplier's identity still conflicts.
	if _, err := retainPASSubmitSupplier(body, []byte(`{"resourceType":"Organization","id":"org-dme-ox","name":"Someone Else"}`)); err == nil {
		t.Fatal("a different supplier under the carried identity was accepted")
	}
}
