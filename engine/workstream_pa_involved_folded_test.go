//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PA zone's half of the canonical names' tests (canonicalnames.go): the
// involved list's request readers (requestMembers, carriedMembers,
// pasSubjectNames) on each leg that takes the list, and each leg's own readers
// before its binding. The core's half, the views and the binding's readers, is
// canonicalnames_test.go, in the core-only build too.

// involvedListLegs are the request legs whose request names its patients for the
// involved list, read from the whole request by carriedMembers (coverage
// eligibility reads each request it submits on its own, eligibilitySubmissions).
var involvedListLegs = []string{"pas-claim", "pas-claim-update", "pas-claim-inquire", "crd-order-select", "crd-order-dispatch", "dtr-questionnaire-fetch"}

// The view is for reading: the readers leave the request's bytes as sent, and a
// view is new maps, so changing one changes no other read of the same request.
func TestCanonicalNames_TheRequestIsReadNotChanged(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"patient","id":"MBR-OX","Contained":[{"resourceType":"Patient","id":"x"}]}},{"resource":{"resourceType":"Observation","subject":{"Reference":"Patient/MBR-PAYERB"}}}]}`)
	sent := bytes.Clone(body)
	for _, leg := range involvedListLegs {
		_ = carriedMembers(body, leg)
		_ = mustMembers(t)((paWorkstream{}).requestMembers(leg, body))
	}
	_, _ = carriedDemographics(body, "MBR-OX", false)
	_ = carriesMembersParty(body, "MBR-OX", true)
	_ = indexCarriedPatients(body)
	if !bytes.Equal(body, sent) {
		t.Fatal("a reader changed the request's bytes")
	}
	a, _ := strictView(body)
	b, _ := strictView(body)
	a.doc.(map[string]any)["entry"] = nil
	if b.doc.(map[string]any)["entry"] == nil {
		t.Fatal("two views share a map")
	}
}

// foldedRow is a request naming MBR-OX besides the subject under a spelling a
// lenient reader folds, and the same request spelled exactly.
type foldedRow struct{ folded, exact string }

var foldedRows = map[string]foldedRow{
	"a Patient under Contained": {
		`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Observation","id":"o1","Contained":[{"resourceType":"Patient","id":"MBR-OX"}],"subject":{"reference":"#MBR-OX"}}}]}`,
		`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Observation","id":"o1","contained":[{"resourceType":"Patient","id":"MBR-OX"}],"subject":{"reference":"#MBR-OX"}}}]}`,
	},
	"a resourceType of patient": {
		`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"patient","id":"MBR-OX"}}]}`,
		`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"MBR-OX"}}]}`,
	},
	"a Reference": {
		`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Observation","subject":{"Reference":"Patient/MBR-OX"}}}]}`,
		`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/MBR-OX"}}}]}`,
	},
	"an entry's FullUrl and Resource": {
		`{"resourceType":"Bundle","entry":[{"FullUrl":"urn:uuid:ox","Resource":{"resourceType":"Patient","id":"MBR-OX"}},{"resource":{"resourceType":"Observation","subject":{"reference":"urn:uuid:ox"}}}]}`,
		`{"resourceType":"Bundle","entry":[{"fullUrl":"urn:uuid:ox","resource":{"resourceType":"Patient","id":"MBR-OX"}},{"resource":{"resourceType":"Observation","subject":{"reference":"urn:uuid:ox"}}}]}`,
	},
}

// On every leg that takes the involved list, a patient the request names under a
// member name or resource type spelled in another case is named as the exact
// spelling names it: carriedMembers lists it, and the involved pass names it
// under the identity this holder's system holds, with the same omissions.
func TestInvolvedList_FoldedNamesAreNamedOnEveryLeg(t *testing.T) {
	for name, row := range foldedRows {
		for _, leg := range involvedListLegs {
			t.Run(name+"/"+leg, func(t *testing.T) {
				want := carriedMembers([]byte(row.exact), leg)
				if !slices.ContainsFunc(want, func(m string) bool { return strings.HasSuffix(m, "MBR-OX") || m == "urn:uuid:ox" }) {
					t.Fatalf("control: the exact spelling names %v, want MBR-OX among them", want)
				}
				if got := carriedMembers([]byte(row.folded), leg); !slices.Equal(got, want) {
					t.Fatalf("carriedMembers %v, want %v as the exact spelling names", got, want)
				}
				named := func(body string) ([]involvedPatient, []string) {
					g, _, _, omitted := involvedGateway(t, true)
					got := g.requestNamedPatients(context.Background(), leg, "corr-1", []byte(body), heldPCI(t, "MBR-COVERED"))
					return got, *omitted
				}
				wantNamed, wantOmitted := named(row.exact)
				gotNamed, gotOmitted := named(row.folded)
				if !slices.Equal(gotNamed, wantNamed) || !slices.Equal(gotOmitted, wantOmitted) {
					t.Fatalf("named %v omitted %v, want %v omitted %v", gotNamed, gotOmitted, wantNamed, wantOmitted)
				}
			})
		}
	}
	// A row where the exact spelling is named: MBR-OX is held, so it is named.
	g, _, _, _ := involvedGateway(t, true)
	got := g.requestNamedPatients(context.Background(), "crd-order-select", "corr-1", []byte(foldedRows["a Reference"].folded), heldPCI(t, "MBR-COVERED"))
	if len(got) != 1 || got[0].pci != heldPCI(t, "MBR-OX") {
		t.Fatalf("named %v, want MBR-OX's held identity", got)
	}
}

// A Patient carrying a decimal past float64's range is still read: on the
// involved list on every leg, and in a binding, by its demographics, or as a
// Coverage's party carrying the member's number, which is still refused.
func TestCarriedPatients_ALargeDecimalIsRead(t *testing.T) {
	body := `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"MBR-OX",` + largeDecimal + `}},{"resource":{"resourceType":"Observation","subject":{"reference":"Patient/MBR-OX"}}}]}`
	for _, leg := range involvedListLegs {
		if got := carriedMembers([]byte(body), leg); !slices.Contains(got, "MBR-OX") {
			t.Errorf("%s: members %v, want MBR-OX", leg, got)
		}
		g, _, _, _ := involvedGateway(t, true)
		got := g.requestNamedPatients(context.Background(), leg, "corr-1", []byte(body), heldPCI(t, "MBR-COVERED"))
		if len(got) != 1 || got[0].pci != heldPCI(t, "MBR-OX") {
			t.Errorf("%s: named %v, want MBR-OX's held identity", leg, got)
		}
	}
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	child := strings.Replace(exactChild, `"birthDate"`, largeDecimal+`,"birthDate"`, 1)
	wantNoParent(t, g, `{"prefetch":{"patient":`+child+`,"coverage":`+partyCoverage(`"contained":[`+exactParent+`]`)+`}}`, derivedPCI(strangerMember, "2015-06-01", "Childname"))
	parent := strings.Replace(exactParent, `"birthDate"`, largeDecimal+`,"birthDate"`, 1)
	wantNoParent(t, g, `{"prefetch":{"coverage":`+partyCoverage(`"contained":[`+parent+`]`)+`}}`, "")
	idx := indexCarriedPatients([]byte(body))
	if len(idx.byName["MBR-OX"]) != 1 {
		t.Fatalf("the carried index reads %v, want the Patient under MBR-OX", idx.byName)
	}
}

// A dependent's Coverage naming its parent, a contained Patient, by a subscriber
// slot spelled "Subscriber" is read as the exact spelling is: the parent is the
// Coverage's party, never the member, and names no member on the involved list.
func TestCoverageParty_ACaseVariantSubscriberNamesTheParty(t *testing.T) {
	exact := `{"resourceType":"Coverage","id":"c","contained":[` + exactParent + `],"subscriber":{"reference":"#parent"},"beneficiary":{"reference":"Patient/` + strangerMember + `"}}`
	folded := strings.Replace(exact, `"subscriber"`, `"Subscriber"`, 1)
	for name, body := range map[string]string{"exact": exact, "Subscriber": folded} {
		v, err := strictView([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		cov := v.doc.(map[string]any)
		if !shnsdk.CoverageParty(cov, cov["contained"].([]any)[0].(map[string]any)) {
			t.Fatalf("%s: the rule names no party", name)
		}
		for _, leg := range involvedListLegs {
			if got := carriedMembers([]byte(body), leg); slices.Contains(got, "parent") || slices.Contains(got, "#parent") {
				t.Fatalf("%s, %s: the party is named a member: %v", name, leg, got)
			}
		}
		g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
		wantNoParent(t, g, `{"prefetch":{"coverage":`+body+`}}`, "")
	}
}

// strictOnlyUnreadable are request bodies the leg's strict readers refuse but the
// binding reads: a CDS Hooks vendor object spelling a name two ways, nesting past
// the relay's bound, and a byte that is not well-formed UTF-8, each outside every
// FHIR resource; and a FHIR resource repeating a name in another case, which both
// refuse. base is the request they are added to.
func strictOnlyUnreadable(base []byte) map[string][]byte {
	with := func(member string) []byte {
		return append([]byte(`{`+member+`,`), base[bytes.IndexByte(base, '{')+1:]...)
	}
	return map[string][]byte{
		"a vendor object spelling a name two ways":    with(`"extension":{"com.example":{"x":1,"X":2}}`),
		"nesting past the relay's bound":              with(`"extension":{"deep":` + strings.Repeat(`[`, 70) + `1` + strings.Repeat(`]`, 70) + `}`),
		"a byte that is not well-formed UTF-8":        with("\"extension\":{\"note\":\"\xff\"}"),
		"a resource repeating a name in another case": with(`"extension":{"p":{"resourceType":"Patient","id":"MBR-OX","ID":"MBR-PAYERB"}}`),
	}
}

// A request on a leg that takes the involved list that its strict readers cannot
// read is sent as before, with no list, at every level: the involved pass requests
// no token and records the omission once, request-unreadable. Only an embedder calling
// OriginateLeg directly sends such a body (every leg's own ingress readers refuse
// it first).
func TestInvolvedForRequest_AStrictlyUnreadableRequestIsSentWithNoList(t *testing.T) {
	base := []byte(`{"hook":"order-select","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-OX"}}}]}}}`)
	for name, body := range strictOnlyUnreadable(base) {
		if _, err := strictView(body); !errors.Is(err, errRequestPatientsUnreadable) {
			t.Fatalf("%s: the strict view reads it: the row tests nothing", name)
		}
		if name != "a resource repeating a name in another case" {
			if _, err := bindingView(body); err != nil {
				t.Fatalf("%s: the binding cannot read it: %v", name, err)
			}
		}
		for _, level := range allLevels {
			for _, leg := range involvedListLegs {
				t.Run(name+"/"+level.String()+"/"+leg, func(t *testing.T) {
					g, authz, _, omitted := involvedGateway(t, true)
					g.cfg.ConformanceEnforcement = level
					r := httptest.NewRequest(http.MethodPost, "/", nil)
					if list := g.involvedForRequest(context.Background(), r, leg, "provider-tpo", "pas-submit", "corr-1", "hash-1", body, heldPCI(t, "MBR-COVERED")); list != "" {
						t.Fatalf("list %q, want none", list)
					}
					if n := len(authz.calls()); n != 0 || !slices.Equal(*omitted, []string{involvedOmitRequestUnreadable}) {
						t.Fatalf("%d tokens requested, omitted %v; want none and one request-unreadable", n, *omitted)
					}
					// The control: the same request read strictly names MBR-OX.
					ok, _, _, _ := involvedGateway(t, true)
					if list := ok.involvedForRequest(context.Background(), r, leg, "provider-tpo", "pas-submit", "corr-1", "hash-1", base, heldPCI(t, "MBR-COVERED")); list == "" {
						t.Fatal("control: the readable request names no one")
					}
				})
			}
		}
	}
	// Through OriginateLeg, as an embedder sends it: the request is carried byte
	// for byte with no involved list, and the omission is counted.
	for name, body := range strictOnlyUnreadable(newInProcessExchange(t).crdReq) {
		for _, level := range allLevels {
			t.Run("OriginateLeg/"+name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.originator.cfg.HubAcceptsInvolved = true
				var omitted []string
				env.originator.cfg.InvolvedMetric = func(reason string) { omitted = append(omitted, reason) }
				if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "",
					Content{WorkstreamType: workstreamPA, Payload: testRequest(body)}); err != nil {
					t.Fatalf("not sent: %v", err)
				}
				if got := env.lastRequestPayload(); !bytes.Equal(got, body) {
					t.Fatalf("sent %q, want the request byte for byte", got)
				}
				env.substrate.mu.Lock()
				lists := slices.Clone(env.substrate.involved)
				env.substrate.mu.Unlock()
				if len(lists) != 1 || lists[0] != "" || !slices.Equal(omitted, []string{involvedOmitRequestUnreadable}) {
					t.Fatalf("involved lists %q, omitted %v; want one leg with none and one request-unreadable", lists, omitted)
				}
			})
		}
	}
}

// A coverage-eligibility request whose body eligibilitySubmissions cannot read,
// an empty one included, is one whose patients are unknown, never one naming no
// other patient: it is sent as before with no involved list, at every level, and
// the omission is counted once, request-unreadable. The count refuses nothing.
// The leg's ingress route refuses such a body 400 first, so only an embedder
// calling OriginateLeg directly sends one.
func TestInvolvedForRequest_AnUnreadableEligibilityRequestIsCountedNotRefused(t *testing.T) {
	bodies := map[string][]byte{
		"empty":                          nil,
		"not JSON":                       []byte(`not json`),
		"a JSON array":                   []byte(`[]`),
		"not an eligibility request":     []byte(`{"resourceType":"Observation","id":"o1"}`),
		"its type named in another case": []byte(`{"ResourceType":"CoverageEligibilityRequest","patient":{"reference":"Patient/MBR-OX"}}`),
		"a member named twice":           []byte(`{"resourceType":"CoverageEligibilityRequest","patient":{"reference":"Patient/MBR-OX"},"patient":{"reference":"Patient/MBR-COVERED"}}`),
		"an empty Bundle":                []byte(`{"resourceType":"Bundle","type":"batch"}`),
		"a Bundle whose entries cannot be read": []byte(`{"resourceType":"Bundle","type":"batch","entry":[{"resource":` +
			string(eligibilityRequest(t, "MBR-OX")) + `},{"resource":{"id":"no-type"}}]}`),
		"a Parameters with no resource parameter": []byte(`{"resourceType":"Parameters","parameter":[{"name":"other","resource":` +
			string(eligibilityRequest(t, "MBR-OX")) + `}]}`),
		"a Parameters with two resource parameters": []byte(`{"resourceType":"Parameters","parameter":[{"name":"resource","resource":` +
			string(eligibilityRequest(t, "MBR-OX")) + `},{"name":"resource","resource":` + string(eligibilityRequest(t, "MBR-COVERED")) + `}]}`),
	}
	for name, body := range bodies {
		if _, err := eligibilitySubmissions(body); err == nil {
			t.Fatalf("%s: eligibilitySubmissions reads it: the row tests nothing", name)
		}
	}
	for name, body := range bodies {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				assertEligibilityInvolved(t, level, body, nil, []string{involvedOmitRequestUnreadable})
			})
		}
	}
	// Through OriginateLeg, as an embedder sends it, the empty body included:
	// the request is carried byte for byte with no involved list at every
	// level, and the omission is counted.
	for name, body := range bodies {
		for _, level := range allLevels {
			t.Run("OriginateLeg/"+name+"/"+level.String(), func(t *testing.T) {
				assertEligibilityCarried(t, level, body, nil, []string{involvedOmitRequestUnreadable})
			})
		}
	}
}

// A request within a coverage-eligibility body whose own patient cannot be read,
// or reads as no name at all, is one omission, request-unreadable, never a
// request naming no one; every member the other requests name is still named.
// The count refuses nothing: the body is carried byte for byte at every level.
func TestInvolvedForRequest_AnEligibilityRequestWhosePatientCannotBeReadIsCounted(t *testing.T) {
	nope := eligibilityWith(t, "MBR-COVERED", map[string]any{"patient": map[string]any{"reference": "#nope"}})
	rows := map[string]struct {
		body   []byte
		listed []string // the members the involved list names
	}{
		"a lone request with no patient":                  {[]byte(`{"resourceType":"CoverageEligibilityRequest"}`), nil},
		"a lone request whose patient is {}":              {[]byte(`{"resourceType":"CoverageEligibilityRequest","patient":{}}`), nil},
		"a batch whose second request's patient is #nope": {eligBatch(t, eligibilityRequest(t, "MBR-OX"), nope), []string{"MBR-OX"}},
	}
	for name, row := range rows {
		subs, err := eligibilitySubmissions(row.body)
		if err != nil {
			t.Fatalf("%s: eligibilitySubmissions cannot read the body (%v): the row tests the whole-body case", name, err)
		}
		if unread := slices.ContainsFunc(subs, func(s eligibilitySubmission) bool {
			_, err := eligibilitySubmissionMember(s)
			return err != nil
		}); !unread {
			t.Fatalf("%s: every request's patient reads: the row tests nothing", name)
		}
		var want []string
		for _, m := range row.listed {
			want = append(want, heldPCI(t, m))
		}
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				assertEligibilityInvolved(t, level, row.body, want, []string{involvedOmitRequestUnreadable})
			})
			t.Run("OriginateLeg/"+name+"/"+level.String(), func(t *testing.T) {
				assertEligibilityCarried(t, level, row.body, want, []string{involvedOmitRequestUnreadable})
			})
		}
	}
}

// A lone eligibility request whose patient reference names no one ("",
// "Patient/" or "Patient/ ") names no patient at all: its member read fails
// (errMemberNamesNoOne), and the leg's binding refuses the request
// (TestEligibility_APatientReferenceNamingNoOneIsRefused). Reached directly,
// the involved pass counts it once as a request it cannot read, names no one
// and never reads the system of record for it, at every level, whatever the
// leg's subject.
func TestInvolvedForRequest_AnEligibilityReferenceNamingNoOneNamesNoOtherPatient(t *testing.T) {
	for _, ref := range []string{"", "Patient/", "Patient/ "} {
		body := eligibilityWith(t, "MBR-COVERED", map[string]any{"patient": map[string]any{"reference": ref}})
		subs, err := eligibilitySubmissions(body)
		if err != nil || len(subs) != 1 {
			t.Fatalf("%q: the request does not read as one request (%v)", ref, err)
		}
		if m, err := eligibilitySubmissionMember(subs[0]); !errors.Is(err, errMemberNamesNoOne) {
			t.Fatalf("%q: the member reads as %q (%v), want errMemberNamesNoOne: the row tests nothing", ref, m, err)
		}
		for _, level := range allLevels {
			t.Run(strconv.Quote(ref)+"/"+level.String(), func(t *testing.T) {
				g, authz, sor, omitted := involvedGateway(t, true)
				g.cfg.ConformanceEnforcement = level
				r := httptest.NewRequest(http.MethodPost, "/", nil)
				if list := g.involvedForRequest(context.Background(), r, "coverage-eligibility", "provider-tpo", "eligibility", "corr-1", "hash-1", body, heldPCI(t, "MBR-COVERED")); list != "" {
					t.Fatalf("list %q, want none", list)
				}
				if n := len(authz.calls()); n != 0 || !slices.Equal(*omitted, []string{involvedOmitRequestUnreadable}) || sor.count() != 0 {
					t.Fatalf("%d tokens requested, omitted %v, %d system-of-record reads; want none, the request counted once as unreadable and no read", n, *omitted, sor.count())
				}
			})
		}
	}
}

// assertEligibilityInvolved is the coverage-eligibility involved list of body at
// level, for MBR-COVERED's leg: it names exactly listed and omits exactly omits,
// and, as the control, a readable batch at the same level names MBR-OX and
// omits nothing.
func assertEligibilityInvolved(t *testing.T, level ConformanceEnforcement, body []byte, listed, omits []string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	g, authz, _, omitted := involvedGateway(t, true)
	g.cfg.ConformanceEnforcement = level
	got := involvedSubjects(t, g.involvedForRequest(context.Background(), r, "coverage-eligibility", "provider-tpo", "eligibility", "corr-1", "hash-1", body, heldPCI(t, "MBR-COVERED")))
	if !slices.Equal(got, listed) || len(authz.calls()) != len(listed) || !slices.Equal(*omitted, omits) {
		t.Fatalf("listed %v with %d tokens requested, omitted %v; want %v and omitted %v", got, len(authz.calls()), *omitted, listed, omits)
	}
	ok, _, _, okOmitted := involvedGateway(t, true)
	ok.cfg.ConformanceEnforcement = level
	readable := eligBatch(t, eligibilityRequest(t, "MBR-COVERED"), eligFAIS(t, "MBR-OX", "00001"))
	if got := involvedSubjects(t, ok.involvedForRequest(context.Background(), r, "coverage-eligibility", "provider-tpo", "eligibility", "corr-1", "hash-1", readable, heldPCI(t, "MBR-COVERED"))); !slices.Equal(got, []string{heldPCI(t, "MBR-OX")}) || len(*okOmitted) != 0 {
		t.Fatalf("control: the readable batch listed %v, omitted %v; want MBR-OX alone and none", got, *okOmitted)
	}
}

// assertEligibilityCarried sends body on the coverage-eligibility leg through
// OriginateLeg, as an embedder does, at level: it is carried byte for byte, one
// leg goes out, its involved list names exactly listed (decoded subjects), and
// exactly omits is counted.
func assertEligibilityCarried(t *testing.T, level ConformanceEnforcement, body []byte, listed, omits []string) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	env.originator.cfg.HubAcceptsInvolved = true
	var omitted []string
	env.originator.cfg.InvolvedMetric = func(reason string) { omitted = append(omitted, reason) }
	if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "coverage-eligibility", "pci-1", "corr-1", "",
		Content{WorkstreamType: workstreamPA, Payload: testRequest(body)}); err != nil {
		t.Fatalf("not sent: %v", err)
	}
	if got := env.lastRequestPayload(); !bytes.Equal(got, body) {
		t.Fatalf("sent %q, want the request byte for byte", got)
	}
	env.substrate.mu.Lock()
	lists := slices.Clone(env.substrate.involved)
	env.substrate.mu.Unlock()
	if len(lists) != 1 || !slices.Equal(involvedSubjects(t, lists[0]), listed) || !slices.Equal(omitted, omits) {
		t.Fatalf("involved lists %q, omitted %v; want one leg listing %v and omitted %v", lists, omitted, listed, omits)
	}
}

// A body the binding cannot decode at all is refused 422 on authority, never bound
// by the member id alone (TestBinding_AnUndecodableBodyIsRefused). No request
// carried in earlier releases meets this: on every leg that takes the involved
// list, and on coverage eligibility, the leg's own readers refuse such a body
// before its binding reads it, with the answer they gave before the binding
// refused (unchanged: status and message pinned here).
func TestBinding_AnUndecodableBodyIsRefusedFirstOnEveryLeg(t *testing.T) {
	ctx := context.Background()
	// Each leg's readers before its binding, on the same kind of body: a FHIR
	// resource repeating a name, which the binding's own decode refuses. Each
	// answer is what that reader answered in earlier releases: these readers are not
	// changed, and refuse before any binding reads the body.
	g := &Gateway{cfg: Config{SoR: newCensusSoR(), ConformanceEnforcement: EnforcementNone}}
	pas := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/eNEW"}}},{"resource":{"resourceType":"Patient","id":"eNEW","id":"eNEW"}}]}`)
	crd := []byte(`{"hook":"order-select","hookInstance":"h","context":{"patientId":"eNEW","draftOrders":{"resourceType":"Bundle","entry":[]}},"prefetch":{"patient":{"resourceType":"Patient","id":"eNEW","id":"eNEW"}}}`)
	dispatch := []byte(`{"hook":"order-dispatch","hookInstance":"h","context":{"patientId":"eNEW","dispatchedOrders":["DeviceRequest/d"],"performer":"Organization/o"},"prefetch":{"patient":{"resourceType":"Patient","id":"eNEW","id":"eNEW"}}}`)
	dtr := []byte(`{"resourceType":"Parameters","parameter":[{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/eNEW"},"id":"c","id":"c"}}]}`)
	for _, row := range []struct {
		leg, side string
		answer    func() (int, string)
		status    int
		msg       string
	}{
		{"crd-order-select", "provider", func() (int, string) { _, s, m := g.ingressCRDSubjectPCIContext(ctx, crd); return s, m }, http.StatusBadRequest, "parse cds request failed"},
		{"crd-order-select", "payer", func() (int, string) { _, _, _, s, m := g.conformantCRDBindContext(ctx, crd); return s, m }, http.StatusBadRequest, "parse cds request failed"},
		{"crd-order-dispatch", "provider", func() (int, string) { _, s, m := g.ingressCRDSubjectPCIContext(ctx, dispatch); return s, m }, http.StatusBadRequest, "parse cds request failed"},
		{"crd-order-dispatch", "payer", func() (int, string) { _, _, _, s, m := g.conformantCRDDispatchBindContext(ctx, dispatch); return s, m }, http.StatusBadRequest, "parse cds request failed"},
		{"pas-claim and pas-claim-update", "provider", func() (int, string) { _, s, m := g.ingressPASNativeSubjectPCIContext(ctx, pas); return s, m }, http.StatusBadRequest, "parse claim bundle failed"},
		{"pas-claim and pas-claim-update", "payer", func() (int, string) { _, _, s, m := g.conformantPASBindContext(ctx, pas); return s, m }, http.StatusBadRequest, "parse claim bundle failed"},
		{"pas-claim-inquire", "both", func() (int, string) {
			_, s, m := readPASInquiryFacts(pas, func(string) bool { return false })
			return s, m
		}, http.StatusBadRequest, "parse inquiry bundle failed"},
		{"dtr-questionnaire-fetch", "provider", func() (int, string) { _, s, m := g.prepareDTRPackageRequest(ctx, dtr); return s, m }, http.StatusBadRequest, "parse questionnaire-package parameters failed"},
		{"dtr-questionnaire-fetch", "payer", func() (int, string) { _, s, m := g.bindPackageParameters(ctx, dtr); return s, m }, http.StatusBadRequest, "parse questionnaire-package parameters failed"},
	} {
		if status, msg := row.answer(); status != row.status || msg != row.msg {
			t.Errorf("%s (%s): answered %d %q, want %d %q, as before", row.leg, row.side, status, msg, row.status, row.msg)
		}
	}
	// Coverage eligibility refuses a request repeating a name, on both
	// gateways, before it reads the requests it submits (repeatsAName), and
	// binds each from a value it parsed: its binding never meets such a body.
	elig := bytes.Replace(eligibilityRequest(t, "eNEW"), []byte(`"resourceType":"CoverageEligibilityRequest"`), []byte(`"resourceType":"CoverageEligibilityRequest","id":"x","id":"y"`), 1)
	if !repeatsAName(elig) {
		t.Fatal("fixture: the eligibility request repeats no name")
	}
	if _, rec, _ := runEligibilityIngress(t, EnforcementNone, elig, nil); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "it can be read two ways") {
		t.Errorf("coverage-eligibility (provider): answered %d %s, want the 400 a repeated name has always had", rec.Code, rec.Body)
	}
}

// readerAnswers are every answer the involved list, the binding and the party
// rule give for body: each leg's carriedMembers, the Bundle's entry parties as
// the involved list reads them, and the binding's answers (bindingAnswers).
func readerAnswers(t *testing.T, body []byte) string {
	t.Helper()
	var out []string
	for _, leg := range involvedListLegs {
		out = append(out, leg+": "+strings.Join(carriedMembers(body, leg), ","))
	}
	if v, err := strictView(body); err == nil {
		parties, slots := entryPartyReads(v)
		out = append(out, "entry parties: "+strings.Repeat("p", len(parties))+strings.Repeat("s", len(slots)))
	}
	return strings.Join(out, "\n") + "\n" + bindingAnswers(t, body)
}

// recaseCorpus is the readers' own test rows: the binding's (bindingCorpus), and
// the involved list's requests, the sdk's Coverage party table as the walkers'
// agreement rows send it (with the probe), the entry-party fixtures with each
// parent, the derivation's party rows and the folded rows.
func recaseCorpus(t *testing.T) map[string][]byte {
	t.Helper()
	corpus := bindingCorpus(t)
	corpus["involvedRequest"] = involvedRequest()
	corpus["partyNamingRequest"] = partyNamingRequest()
	for _, r := range sdkCoveragePartyRows(t) {
		cov := walkerVariant(t, r)
		corpus["sdk party/"+r.Name+"/resource"] = mustJSON(t, cov)
		corpus["sdk party/"+r.Name+"/entry"] = mustJSON(t, map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{map[string]any{"fullUrl": "urn:uuid:c1", "resource": cov}}})
	}
	for fname, fixture := range entryParentFixtures(t) {
		for pname, p := range entryParentAccepts {
			if body, ok := withEntryParent(t, fixture, p); ok {
				corpus["entry parent/"+fname+"/"+pname] = body
			}
		}
		for pname, p := range entryParentRefusals {
			if body, ok := withEntryParent(t, fixture, p); ok {
				corpus["entry parent refused/"+fname+"/"+pname] = body
			}
		}
	}
	for name, cov := range unclearPartyCoverages {
		corpus["unclear party/"+name] = []byte(`{"prefetch":{"patient":` + exactChild + `,"coverage":` + cov + `}}`)
	}
	corpus["party, with the child"] = []byte(`{"prefetch":{"patient":` + exactChild + `,"coverage":` + partyCoverage(`"contained":[`+exactParent+`]`) + `}}`)
	corpus["party alone"] = []byte(`{"prefetch":{"coverage":` + partyCoverage(`"contained":[`+exactParent+`]`) + `}}`)
	for name, row := range foldedRows {
		corpus["folded/"+name] = []byte(row.folded)
	}
	return corpus
}

// The fence on the canonical names, the involved list's readers and the binding's
// together (the binding's alone run in the core-only build too,
// TestCanonicalNames_EveryBindingRowReadsTheSameRecased): re-casing every member name within a FHIR
// resource (but the identity facts, exactNames) and every resourceType value, in
// every row of the readers' own tables, changes no answer the involved list, the
// binding or the party rule gives. A name a reader reads that viewNames does not
// list, or a type it compares that viewTypes does not, makes some row differ.
// Deterministic: upper, lower, and three fixed seeds of mixed case.
func TestCanonicalNames_EveryRowReadsTheSameRecased(t *testing.T) {
	recasingFence(t, recaseCorpus(t), readerAnswers, 50)
}

// A member spelling "contained" in another case outside every FHIR resource is
// no contained list to the binding (TestBinding_AVendorContainedIsNoContainedList),
// but the involved list names a Patient there, as it names any Patient the
// request carries wherever it is carried; on CRD this one is the leg's own
// patient (context.patientId), so the involved pass names no one else.
func TestInvolvedList_AVendorContainedPatientIsNamed(t *testing.T) {
	patient := `[{"resourceType":"Patient","id":"MBR-ZZ","birthDate":"1990-01-01","name":[{"family":"Vendor"}]}]`
	for name, body := range map[string]string{
		"a top-level Contained":    `{"hook":"order-select","context":{"patientId":"MBR-ZZ"},"Contained":` + patient + `}`,
		"an extension's Contained": `{"hook":"order-select","context":{"patientId":"MBR-ZZ"},"extension":{"Contained":` + patient + `}}`,
	} {
		for _, leg := range involvedListLegs {
			if got := carriedMembers([]byte(body), leg); !slices.Equal(got, []string{"MBR-ZZ"}) {
				t.Errorf("%s, %s: members %v, want the carried Patient MBR-ZZ", name, leg, got)
			}
		}
		for _, leg := range []string{"crd-order-select", "crd-order-dispatch"} {
			ig, _, _, omitted := involvedGateway(t, true)
			if got := ig.requestNamedPatients(context.Background(), leg, "corr-1", []byte(body), derivedPCI("MBR-ZZ", "", "")); got != nil || len(*omitted) != 0 {
				t.Errorf("%s, %s: named %v, omitted %v; want the leg's own patient named by no one", name, leg, got, *omitted)
			}
		}
	}
}
