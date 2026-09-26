package engine

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// countingSoR is the census system of record, counting its member reads.
type countingSoR struct {
	*censusSoR
	mu    sync.Mutex
	reads int
}

// unreadableSoR is a system of record whose member reads fail.
type unreadableSoR struct{ *prefetchSoR }

func (unreadableSoR) ResolvePatientContext(context.Context, string) (string, Demo, bool, error) {
	return "", Demo{}, false, &SoRReadError{Kind: SoRUnavailable}
}

func (c *countingSoR) ResolvePatient(member string) (string, Demo, bool) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.censusSoR.ResolvePatient(member)
}

func (c *countingSoR) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// fakeAuthz is an Authorization Framework that mints a token echoing each
// request, counting the requests. refuse and fail name subjects it refuses
// (403) or fails (500); unlabelled names subjects whose token it mints without
// the involvement requested.
type fakeAuthz struct {
	mu                             sync.Mutex
	requests                       []authorizeReq
	refuse, fail, unlabelled, hang map[string]bool
	inFlight, maxInFlight          int
}

func (f *fakeAuthz) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req authorizeReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	time.Sleep(5 * time.Millisecond) // long enough for requests to overlap
	if f.hang[req.SubjectPCI] {
		<-r.Context().Done() // answers only once the caller gives up
		return
	}
	switch {
	case f.refuse[req.SubjectPCI]:
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	case f.fail[req.SubjectPCI]:
		http.Error(w, `{"error":"decision audit failed"}`, http.StatusBadGateway)
		return
	}
	tok := shnsdk.Token{Operation: req.Operation, Frame: req.Frame, Subject: req.SubjectPCI, CorrelationID: req.CorrelationID, PayloadHash: req.PayloadHash, Involvement: req.Involvement}
	if f.unlabelled[req.SubjectPCI] {
		tok.Involvement = ""
	}
	_ = json.NewEncoder(w).Encode(authorizeResp{Token: tok})
}

func (f *fakeAuthz) calls() []authorizeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]authorizeReq(nil), f.requests...)
}

// involvedGateway is a gateway whose Hub reads the involved list (or not),
// with a fake Authorization Framework, a counting system of record, and the
// omissions it reports.
func involvedGateway(t *testing.T, hubAccepts bool) (*Gateway, *fakeAuthz, *countingSoR, *[]string) {
	t.Helper()
	authz := &fakeAuthz{refuse: map[string]bool{}, fail: map[string]bool{}, unlabelled: map[string]bool{}, hang: map[string]bool{}}
	srv := httptest.NewServer(authz)
	t.Cleanup(srv.Close)
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sor := &countingSoR{censusSoR: newCensusSoR()}
	var mu sync.Mutex
	omitted := &[]string{}
	g := &Gateway{cfg: Config{
		HolderID:           "provider",
		Identity:           shnsdk.Identity{HolderID: "provider", SignPriv: priv},
		Client:             srv.Client(),
		AuthzURL:           srv.URL,
		Clock:              func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) },
		SoR:                sor,
		HubAcceptsInvolved: hubAccepts,
		InvolvedMetric: func(reason string) {
			mu.Lock()
			defer mu.Unlock()
			*omitted = append(*omitted, reason)
		},
	}}
	return g, authz, sor, omitted
}

func heldPCI(t *testing.T, member string) string {
	t.Helper()
	pci, _, ok := newCensusSoR().ResolvePatient(member)
	if !ok {
		t.Fatalf("fixture: %s must be held", member)
	}
	return pci
}

// A request naming two members besides its subject, one held and one not, by a
// Patient it carries and by a relative reference.
func involvedRequest() []byte {
	return []byte(`{"resourceType":"Bundle","entry":[
	 {"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"},"subject":{"reference":"Patient/MBR-OX"}}},
	 {"resource":{"resourceType":"Patient","id":"MBR-COVERED"}},
	 {"resource":{"resourceType":"Patient","id":"p-other","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"eSTRANGER"}],"birthDate":"1980-01-01","name":[{"family":"Other"}]}},
	 {"resource":{"resourceType":"Observation","subject":{"reference":"Patient/p-other"}}},
	 {"resource":{"resourceType":"Observation","subject":{"reference":"https://ehr.example/fhir/Patient/ABSOLUTE"}}},
	 {"resource":{"resourceType":"Observation","subject":{"reference":"Patient/MBR-PAYERB/_history/2"}}}
	]}`)
}

// The members a request names, read as the leg's own subject binding reads
// them: each carried Patient's id (no fullUrl here), and each Patient
// reference by what the leg's reader returns — everything after the last
// "Patient/" on PAS, CRD and inquiry, the id without a version suffix on the
// DTR package. A Patient's member identifier is not read. Sorted, each once.
func TestCarriedMembers_ReadsMembersAsEachLegsSubjectBindingDoes(t *testing.T) {
	for leg, want := range map[string][]string{
		"pas-claim":               {"ABSOLUTE", "MBR-COVERED", "MBR-OX", "MBR-PAYERB/_history/2", "p-other"},
		"dtr-questionnaire-fetch": {"ABSOLUTE", "MBR-COVERED", "MBR-OX", "MBR-PAYERB", "p-other"},
	} {
		if got := carriedMembers(involvedRequest(), leg); !slices.Equal(got, want) {
			t.Fatalf("%s: members %v, want %v", leg, got, want)
		}
	}
	if carriedMembers([]byte("not json"), "pas-claim") != nil || carriedMembers(nil, "pas-claim") != nil {
		t.Fatal("an unreadable request names no member")
	}
}

// However a request refers to its own patient — by urn:uuid: to its entry, by
// #id to a contained Patient, or (on the DTR package) by a versioned
// reference beside an unversioned one — every reference reads as the subject
// binding reads it, so no one else is named.
func TestRequestNamedPatients_EveryFormOfTheSubjectsReferenceIsTheSubject(t *testing.T) {
	for _, tc := range []struct {
		name, leg, subjectMember, payload string
	}{
		{"urn:uuid entry", "pas-claim", "urn:uuid:abc",
			`{"resourceType":"Bundle","entry":[{"fullUrl":"urn:uuid:abc","resource":{"resourceType":"Patient","id":"X"}},{"resource":{"resourceType":"Claim","patient":{"reference":"urn:uuid:abc"}}}]}`},
		{"contained", "pas-claim", "#p1",
			`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","contained":[{"resourceType":"Patient","id":"p1"}],"patient":{"reference":"#p1"}}}]}`},
		{"urn:uuid entry on the inquiry", "pas-claim-inquire", "MBR",
			`{"resourceType":"Bundle","entry":[{"fullUrl":"urn:uuid:1111","resource":{"resourceType":"Patient","id":"MBR"}},{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR"}}}]}`},
		{"urn:uuid entry on the DTR package", "dtr-questionnaire-fetch", "MBR",
			`{"resourceType":"Parameters","parameter":[{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR"}}},{"name":"bundle","resource":{"resourceType":"Bundle","entry":[{"fullUrl":"urn:uuid:1111","resource":{"resourceType":"Patient","id":"MBR"}}]}}]}`},
		{"contained on the DTR package", "dtr-questionnaire-fetch", "MBR",
			`{"resourceType":"Parameters","parameter":[{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR"},"contained":[{"resourceType":"Patient","id":"MBR"}]}}]}`},
		{"versioned on the DTR package", "dtr-questionnaire-fetch", "X",
			`{"resourceType":"Parameters","parameter":[{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/X/_history/2"}}},{"name":"referenced","resource":{"resourceType":"Patient","id":"X"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _, _, omitted := involvedGateway(t, true)
			subject, found, err := g.resolveSubjectPCI(context.Background(), tc.subjectMember, []byte(tc.payload))
			if err != nil || !found {
				t.Fatalf("subject: %v %v", found, err)
			}
			if got := g.requestNamedPatients(context.Background(), tc.leg, "corr-1", []byte(tc.payload), subject); got != nil {
				t.Fatalf("named %v, want no one", got)
			}
			if len(*omitted) != 0 {
				t.Fatalf("omitted %v", *omitted)
			}
		})
	}
}

// A request that names its own patient everywhere by the store's own id, with
// that Patient carried under a different member identifier or not carried at
// all, names no one else: every reference resolves to the leg's subject, which
// is bound by that same id.
func TestRequestNamedPatients_ServerIDOfTheSubjectIsTheSubject(t *testing.T) {
	carried := `{"resourceType":"Patient","id":"srv-123","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-COVERED"}],"birthDate":"1975-04-02","name":[{"family":"Johansson"}]}`
	for name, payload := range map[string]string{
		"carried": `{"resourceType":"Bundle","entry":[{"fullUrl":"https://ehr.example/fhir/Patient/srv-123","resource":` + carried + `},` +
			`{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/srv-123"}}},` +
			`{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"https://ehr.example/fhir/Patient/srv-123"}}}]}`,
		"not carried": `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/srv-123"}}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			g, _, _, omitted := involvedGateway(t, true)
			subject, found, err := g.resolveSubjectPCI(context.Background(), "srv-123", []byte(payload))
			if err != nil || !found {
				t.Fatalf("subject: %v %v", found, err)
			}
			if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", []byte(payload), subject); got != nil {
				t.Fatalf("named %v, want no one: every reference is the leg's own patient", got)
			}
			if len(*omitted) != 0 {
				t.Fatalf("omitted %v", *omitted)
			}
		})
	}
}

// Every other patient the request names is identified through the system of
// record as the subject is (held, or derived from the Patient carried), and
// the leg's own patient is not named.
func TestRequestNamedPatients_NamesEveryOtherPatient(t *testing.T) {
	g, _, _, omitted := involvedGateway(t, true)
	subject := heldPCI(t, "MBR-COVERED")
	got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", involvedRequest(), subject)
	want := []involvedPatient{
		{pci: derivedPCI("ABSOLUTE", "", ""), involvement: shnsdk.InvolvementRequestNamed},
		{pci: heldPCI(t, "MBR-OX"), involvement: shnsdk.InvolvementRequestNamed},
		{pci: derivedPCI("MBR-PAYERB/_history/2", "", ""), involvement: shnsdk.InvolvementRequestNamed},
		{pci: derivedPCI("p-other", "1980-01-01", "Other"), involvement: shnsdk.InvolvementRequestNamed},
	}
	byPCI := func(a, b involvedPatient) int { return strings.Compare(a.pci, b.pci) }
	slices.SortFunc(got, byPCI)
	slices.SortFunc(want, byPCI)
	if !slices.Equal(got, want) {
		t.Fatalf("named %v, want %v", got, want)
	}
	if len(*omitted) != 0 {
		t.Fatalf("nothing should be left out: %v", *omitted)
	}
}

// With the Hub not reading the list, and on a leg that is not a
// prior-authorization request, the pass reads nothing and names no one.
func TestRequestNamedPatients_ReadsNothingUnlessTheHubReadsTheList(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hubAccepts bool
		leg        string
	}{
		{"hub does not read the list", false, "pas-claim"},
		{"not a prior-authorization request leg", true, "coverage-eligibility"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, authz, sor, _ := involvedGateway(t, tc.hubAccepts)
			if got := g.requestNamedPatients(context.Background(), tc.leg, "corr-1", involvedRequest(), heldPCI(t, "MBR-COVERED")); got != nil {
				t.Fatalf("named %v, want none", got)
			}
			if sor.count() != 0 {
				t.Fatalf("%d system-of-record reads, want none", sor.count())
			}
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if got := g.involvedForRequest(context.Background(), r, tc.leg, "provider-tpo", "pas-submit", "corr-1", "hash-1", involvedRequest(), heldPCI(t, "MBR-COVERED")); got != "" {
				t.Fatalf("list %q, want none", got)
			}
			if sor.count() != 0 || len(authz.calls()) != 0 {
				t.Fatalf("%d system-of-record reads and %d authorize calls, want none", sor.count(), len(authz.calls()))
			}
		})
	}
}

// A member that cannot be identified is left out, with the reason, and the
// others are still named.
func TestRequestNamedPatients_LeavesOutWhatCannotBeIdentified(t *testing.T) {
	t.Run("system of record unavailable", func(t *testing.T) {
		g, _, _, omitted := involvedGateway(t, true)
		g.cfg.SoR = unreadableSoR{newPrefetchSoR()}
		if got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", involvedRequest(), "pci:subject"); got != nil {
			t.Fatalf("named %v, want none", got)
		}
		if want := []string{involvedOmitUnreadable, involvedOmitUnreadable, involvedOmitUnreadable, involvedOmitUnreadable, involvedOmitUnreadable}; !slices.Equal(*omitted, want) {
			t.Fatalf("omitted %v, want %v", *omitted, want)
		}
	})
	t.Run("known members required", func(t *testing.T) {
		g, _, _, omitted := involvedGateway(t, true)
		g.cfg.RequireKnownMembers = true
		got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", involvedRequest(), heldPCI(t, "MBR-COVERED"))
		if len(got) != 1 || got[0].pci != heldPCI(t, "MBR-OX") {
			t.Fatalf("named %v, want only the held MBR-OX", got)
		}
		if !slices.Equal(*omitted, []string{involvedOmitUnknown, involvedOmitUnknown, involvedOmitUnknown}) {
			t.Fatalf("omitted %v, want three unknown members", *omitted)
		}
	})
	t.Run("more members than are read", func(t *testing.T) {
		g, _, sor, omitted := involvedGateway(t, true)
		var entries []string
		for i := range maxInvolvedMembers + 3 {
			entries = append(entries, fmt.Sprintf(`{"resource":{"resourceType":"Patient","id":"m%03d"}}`, i))
		}
		payload := []byte(`{"resourceType":"Bundle","entry":[` + joinComma(entries) + `]}`)
		got := g.requestNamedPatients(context.Background(), "pas-claim", "corr-1", payload, "pci:subject")
		if len(got) != maxInvolvedMembers || sor.count() != maxInvolvedMembers {
			t.Fatalf("named %d after %d reads, want %d each", len(got), sor.count(), maxInvolvedMembers)
		}
		if len(*omitted) != 3 || (*omitted)[0] != involvedOmitOverflow {
			t.Fatalf("omitted %v, want three overflows", *omitted)
		}
	})
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// Each named patient gets a token bound to the leg with its involvement. The
// leg's own patient and repeats are dropped and the rest ordered; a refused,
// failed or unlabelled token leaves its patient out, never the leg.
func TestInvolvedList_MintsBoundTokensAndLeavesOutWhatFails(t *testing.T) {
	g, authz, _, omitted := involvedGateway(t, true)
	authz.refuse["pci:refused"], authz.fail["pci:failed"], authz.unlabelled["pci:unlabelled"] = true, true, true
	entries := []involvedPatient{
		{pci: "pci:b", involvement: shnsdk.InvolvementRequestNamed},
		{pci: "pci:subject", involvement: shnsdk.InvolvementRequestNamed},
		{pci: "pci:refused", involvement: shnsdk.InvolvementRequestNamed},
		{pci: "pci:a", involvement: shnsdk.InvolvementRequestNamed},
		{pci: "pci:failed", involvement: shnsdk.InvolvementRequestNamed},
		{pci: "pci:b", involvement: shnsdk.InvolvementRequestNamed},
		{pci: "pci:unlabelled", involvement: shnsdk.InvolvementRequestNamed},
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	encoded := g.involvedList(r, "originate", "pas-claim", "provider-tpo", "pas-submit", "corr-1", "hash-1", "pci:subject", entries)
	list, err := shnsdk.DecodeInvolved(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, e := range list {
		var tok shnsdk.Token
		if err := json.Unmarshal([]byte(e.Token), &tok); err != nil {
			t.Fatal(err)
		}
		if tok.Frame != "provider-tpo" || tok.Operation != "pas-submit" || tok.CorrelationID != "corr-1" || tok.PayloadHash != "hash-1" || tok.Involvement != e.Involvement || e.Involvement != shnsdk.InvolvementRequestNamed {
			t.Fatalf("token not bound to the leg: %+v (entry %q)", tok, e.Involvement)
		}
		subjects = append(subjects, tok.Subject)
	}
	if !slices.Equal(subjects, []string{"pci:a", "pci:b"}) {
		t.Fatalf("listed %v, want pci:a then pci:b", subjects)
	}
	slices.Sort(*omitted)
	if want := []string{involvedOmitFailed, involvedOmitRefused, involvedOmitUnlabelled}; !slices.Equal(*omitted, want) {
		t.Fatalf("omitted %v, want %v", *omitted, want)
	}
	if n := len(authz.calls()); n != 5 {
		t.Fatalf("%d authorize calls, want one per distinct other patient (5)", n)
	}
}

// Past the Hub's limit the rest are left out, and no token is requested for them.
func TestInvolvedList_LeavesOutPastTheHubsLimit(t *testing.T) {
	g, authz, _, omitted := involvedGateway(t, true)
	var entries []involvedPatient
	for i := range maxInvolved + 2 {
		entries = append(entries, involvedPatient{pci: fmt.Sprintf("pci:%02d", i), involvement: shnsdk.InvolvementRequestNamed})
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	list, err := shnsdk.DecodeInvolved(g.involvedList(r, "originate", "pas-claim", "provider-tpo", "pas-submit", "corr-1", "hash-1", "pci:subject", entries))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != maxInvolved || len(authz.calls()) != maxInvolved {
		t.Fatalf("listed %d after %d authorize calls, want %d each", len(list), len(authz.calls()), maxInvolved)
	}
	if !slices.Equal(*omitted, []string{involvedOmitOverflow, involvedOmitOverflow}) {
		t.Fatalf("omitted %v, want two overflows", *omitted)
	}
}

// The payer's own binding is named only when it differs from the leg token's
// patient: payer-held when its system of record holds the member, else
// payer-derived. With the Hub not reading the list there is no collector.
func TestInvolvedCollector_NamesADifferingPayerBinding(t *testing.T) {
	g, _, _, _ := involvedGateway(t, true)
	ctx := g.withInvolvedCollector(context.Background(), "pas-claim")
	if _, status, msg := g.bindInboundSubject(ctx, "MBR-COVERED", nil); status != 0 {
		t.Fatalf("bind: %d %s", status, msg)
	}
	if _, status, msg := g.bindInboundSubject(ctx, "eSTRANGER", nil); status != 0 {
		t.Fatalf("bind: %d %s", status, msg)
	}
	held, derived := heldPCI(t, "MBR-COVERED"), derivedPCI("eSTRANGER", "", "")
	g.noteSubjectBinding(ctx, "pas-claim", "corr-1", held, held) // agrees: not named
	g.noteSubjectBinding(ctx, "pas-claim", "corr-1", "pci:token", held)
	g.noteSubjectBinding(ctx, "pas-claim", "corr-1", "pci:token", derived)
	want := []involvedPatient{{pci: held, involvement: shnsdk.InvolvementPayerHeld}, {pci: derived, involvement: shnsdk.InvolvementPayerDerived}}
	if got := involvedCollectorFrom(ctx).list(); !slices.Equal(got, want) {
		t.Fatalf("collected %v, want %v", got, want)
	}

	off, _, _, _ := involvedGateway(t, false)
	ctx = off.withInvolvedCollector(context.Background(), "pas-claim")
	if involvedCollectorFrom(ctx) != nil {
		t.Fatal("no collector when the Hub does not read the list")
	}
	if involvedCollectorFrom(g.withInvolvedCollector(context.Background(), "coverage-eligibility")) != nil {
		t.Fatal("no collector on a leg that is not a prior-authorization leg")
	}
	off.noteSubjectBinding(ctx, "pas-claim", "corr-1", "pci:token", held) // nil-safe
}

// hangingSoR is a system of record whose member read for hang answers only
// once its caller gives up.
type hangingSoR struct {
	*prefetchSoR
	hang string
}

func (h hangingSoR) ResolvePatientContext(ctx context.Context, member string) (string, Demo, bool, error) {
	if member == h.hang {
		<-ctx.Done()
		return "", Demo{}, false, ctx.Err()
	}
	return h.prefetchSoR.ResolvePatientContext(ctx, member)
}

// A system of record that does not answer holds the leg only as long as the
// budget: the pass returns when it runs out, and every member it had not
// named is left out for the budget.
func TestInvolvedForRequest_SlowSystemOfRecordStopsAtTheBudget(t *testing.T) {
	g, authz, _, omitted := involvedGateway(t, true)
	g.cfg.SoR = hangingSoR{prefetchSoR: newPrefetchSoR(), hang: "a-slow"}
	g.cfg.InvolvedBudget = 50 * time.Millisecond
	payload := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/a-slow"}}},{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/b-after"}}}]}`)
	start := time.Now()
	got := g.involvedForRequest(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "pas-claim", "provider-tpo", "pas-submit", "corr-1", "hash-1", payload, "pci:subject")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the pass held the leg %s, past its 50ms budget", elapsed)
	}
	if got != "" || len(authz.calls()) != 0 {
		t.Fatalf("list %q after %d authorize calls, want none", got, len(authz.calls()))
	}
	if !slices.Equal(*omitted, []string{involvedOmitBudget, involvedOmitBudget}) {
		t.Fatalf("omitted %v, want both members left out for the budget", *omitted)
	}
}

// An Authorization Framework that does not answer for one patient costs only
// that patient: the others are minted, at most involvedConcurrency at a time,
// and the pass returns when the budget runs out.
func TestInvolvedList_SlowAuthorizationStopsAtTheBudget(t *testing.T) {
	g, authz, _, omitted := involvedGateway(t, true)
	authz.hang["pci:03"] = true
	var entries []involvedPatient
	for i := range 8 {
		entries = append(entries, involvedPatient{pci: fmt.Sprintf("pci:%02d", i), involvement: shnsdk.InvolvementRequestNamed})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	list, err := shnsdk.DecodeInvolved(g.involvedList(httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx), "originate", "pas-claim", "provider-tpo", "pas-submit", "corr-1", "hash-1", "pci:subject", entries))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the pass held the leg %s, past its budget", elapsed)
	}
	if len(list) != 7 || !slices.Equal(*omitted, []string{involvedOmitBudget}) {
		t.Fatalf("listed %d, omitted %v; want 7 listed and the hanging one left out for the budget", len(list), *omitted)
	}
	authz.mu.Lock()
	defer authz.mu.Unlock()
	if authz.maxInFlight > involvedConcurrency {
		t.Fatalf("%d token requests at once, want at most %d", authz.maxInFlight, involvedConcurrency)
	}
}
