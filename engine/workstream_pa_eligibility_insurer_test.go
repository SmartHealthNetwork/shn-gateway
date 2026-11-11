//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The identities the eligibility answer's insurer rows choose among.
var (
	insurerPublished  = shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "PUB-1"}
	insurerPublished2 = shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "PUB-2"}
	insurerNPI        = shnsdk.PayerIdentifier{System: "http://hl7.org/fhir/sid/us-npi", Value: "1234567893"}
	insurerOther      = shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "OTHER-1"}
)

// heldRefSoR is a system of record that also holds refs by reference, and holds coverage as
// member's one Coverage when coverage is set.
type heldRefSoR struct {
	SystemOfRecord
	refs     map[string][]byte
	member   string
	coverage []byte
}

func (s heldRefSoR) ResolveByReference(ref string) ([]byte, bool) {
	if b, ok := s.refs[ref]; ok {
		return b, true
	}
	return s.SystemOfRecord.ResolveByReference(ref)
}

func (s heldRefSoR) OpenCoverage(member string) ([]byte, bool) {
	if s.coverage != nil && member == s.member {
		return s.coverage, true
	}
	return s.SystemOfRecord.OpenCoverage(member)
}

func insurerIdent(p shnsdk.PayerIdentifier) map[string]any {
	return map[string]any{"system": p.System, "value": p.Value}
}

// insurerInline is a payor carrying p inline.
func insurerInline(p shnsdk.PayerIdentifier) map[string]any {
	return map[string]any{"identifier": insurerIdent(p)}
}

// insurerOrg is an Organization id listing ids, in order.
func insurerOrg(id string, ids ...shnsdk.PayerIdentifier) map[string]any {
	var list []any
	for _, p := range ids {
		list = append(list, insurerIdent(p))
	}
	return map[string]any{"resourceType": "Organization", "id": id, "identifier": list}
}

// insurerCoverage is a Coverage with payors, containing contained.
func insurerCoverage(t *testing.T, contained []any, payors ...any) []byte {
	t.Helper()
	cov := map[string]any{"resourceType": "Coverage", "id": "cov-1", "status": "active", "payor": payors}
	if len(contained) > 0 {
		cov["contained"] = contained
	}
	return mustJSON(t, cov)
}

// insurerGateway is the payer's gateway publishing published on the feed.
func insurerGateway(t *testing.T, published []shnsdk.PayerIdentifier) (*Gateway, inboundTestRequester) {
	t.Helper()
	g, requester := newInboundTestGateway(t, false)
	if len(published) > 0 {
		g.cfg.Reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", PayerIDs: published})
	}
	return g, requester
}

// The eligibility answer's insurer is the identity the network knows this payer by, as its own
// record lists it in any position: the first identity this holder publishes on the network feed,
// in document order. When the record lists none of them, or none is known, it is read as before.
func TestEligibilityAnswerInsurer_NamesThePayersNetworkIdentity(t *testing.T) {
	pub := []shnsdk.PayerIdentifier{insurerPublished}
	clinic := map[string]any{"reference": "Organization/clinic"}
	for _, row := range []struct {
		name      string
		published []shnsdk.PayerIdentifier
		notPayer  bool
		contained []any
		held      map[string]any
		payors    []any
		want      shnsdk.PayerIdentifier
		wantOK    bool
	}{
		{name: "an NPI before the published identity", published: pub,
			contained: []any{insurerOrg("o1", insurerNPI, insurerPublished)}, payors: []any{map[string]any{"reference": "#o1"}},
			want: insurerPublished, wantOK: true},
		{name: "an NPI-only payor before the published one", published: pub,
			contained: []any{insurerOrg("o1", insurerNPI)}, payors: []any{map[string]any{"reference": "#o1"}, insurerInline(insurerPublished)},
			want: insurerPublished, wantOK: true},
		{name: "a clinic payor the record does not hold before the published one", published: pub,
			payors: []any{clinic, insurerInline(insurerPublished)},
			want:   insurerPublished, wantOK: true},
		{name: "a self-pay payor before the published one", published: pub,
			payors: []any{map[string]any{"reference": "Patient/" + "MBR-COVERED"}, insurerInline(insurerPublished)},
			want:   insurerPublished, wantOK: true},
		{name: "an Organization the system of record holds", published: pub,
			held:   map[string]any{"Organization/payer-org": insurerOrg("payer-org", insurerNPI, insurerPublished)},
			payors: []any{map[string]any{"reference": "Organization/payer-org"}},
			want:   insurerPublished, wantOK: true},
		{name: "two published identities, in document order", published: []shnsdk.PayerIdentifier{insurerPublished, insurerPublished2},
			payors: []any{insurerInline(insurerPublished2), insurerInline(insurerPublished)},
			want:   insurerPublished2, wantOK: true},
		{name: "an empty identifier is never a published one", published: []shnsdk.PayerIdentifier{{}, insurerPublished},
			payors: []any{insurerInline(shnsdk.PayerIdentifier{}), insurerInline(insurerPublished)},
			want:   insurerPublished, wantOK: true},
		{name: "identities this holder does not publish: read as before", published: pub,
			payors: []any{insurerInline(insurerOther), insurerInline(insurerNPI)},
			want:   insurerOther, wantOK: true},
		{name: "none of this payer's identities: read as before", published: pub,
			contained: []any{insurerOrg("o1", insurerNPI, insurerOther)}, payors: []any{map[string]any{"reference": "#o1"}},
			want: insurerNPI, wantOK: true},
		{name: "no identity known: read as before",
			contained: []any{insurerOrg("o1", insurerNPI, insurerPublished)}, payors: []any{map[string]any{"reference": "#o1"}},
			want: insurerNPI, wantOK: true},
		{name: "a gateway that is not a payer's: read as before", published: pub, notPayer: true,
			contained: []any{insurerOrg("o1", insurerNPI, insurerPublished)}, payors: []any{map[string]any{"reference": "#o1"}},
			want: insurerNPI, wantOK: true},
		{name: "a Coverage naming no payer", published: pub,
			payors: []any{clinic}},
	} {
		t.Run(row.name, func(t *testing.T) {
			g, _ := insurerGateway(t, row.published)
			if row.notPayer {
				g.cfg.Role = "provider"
			}
			refs := map[string][]byte{}
			for ref, res := range row.held {
				refs[ref] = mustJSON(t, res)
			}
			g.cfg.SoR = heldRefSoR{SystemOfRecord: g.cfg.SoR, refs: refs}
			got, ok, _, err := g.coverageAnswerInsurer(context.Background(), insurerCoverage(t, row.contained, row.payors...))
			if err != nil {
				t.Fatalf("coverageAnswerInsurer: %v", err)
			}
			if ok != row.wantOK || got != row.want {
				t.Fatalf("insurer = %+v (ok=%v), want %+v (ok=%v)", got, ok, row.want, row.wantOK)
			}
		})
	}
}

// failingRefSoR is a system of record over base whose read of each reference in fail fails with
// its error, counting the reads of each reference.
func failingRefSoR(t *testing.T, base SystemOfRecord, fail map[string]error) (SystemOfRecord, map[string]int) {
	t.Helper()
	reads := map[string]int{}
	var mu sync.Mutex
	return scriptedReadSoR{t: t, base: ReadSystemOfRecord(base), before: func(_ context.Context, op, key string) error {
		if op != "ResolveByReference" {
			return nil
		}
		mu.Lock()
		reads[key]++
		mu.Unlock()
		return fail[key]
	}}, reads
}

// A read the request's end cut short is named by how it ended (readEndedClass), not as the
// connector reported it.
func TestEligibilityAnswerInsurer_AReadCutShortIsNamedSo(t *testing.T) {
	g, _ := insurerGateway(t, nil)
	sor, _ := failingRefSoR(t, g.cfg.SoR, map[string]error{"Organization/x": context.Canceled})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reads := &insurerReads{ctx: ctx, sor: sor}
	if _, _, err := reads.read("Organization/x"); err == nil {
		t.Fatal("the read did not fail")
	}
	if want := []string{BackendCancelled}; !slices.Equal(reads.failed, want) {
		t.Fatalf("failed = %q, want %q", reads.failed, want)
	}
}

// A payor carrying an identifier of its own is never read from the system of record, so a read
// that would fail stops nothing.
func TestEligibilityAnswerInsurer_AnInlinePayorIsNeverRead(t *testing.T) {
	g, _ := insurerGateway(t, []shnsdk.PayerIdentifier{insurerPublished})
	sor, reads := failingRefSoR(t, g.cfg.SoR, map[string]error{"Organization/payer-org": &SoRReadError{Kind: SoRUnavailable}})
	g.cfg.SoR = sor
	cov := insurerCoverage(t, nil, map[string]any{"reference": "Organization/payer-org", "identifier": insurerIdent(insurerPublished)})
	got, ok, _, err := g.coverageAnswerInsurer(context.Background(), cov)
	if err != nil || !ok || got != insurerPublished || reads["Organization/payer-org"] != 0 {
		t.Fatalf("insurer = %+v (ok=%v, err=%v), %d reads; want %+v with no read", got, ok, err, reads["Organization/payer-org"], insurerPublished)
	}
}

// A failed read of a payor's Organization is survived when the answer can be built without it,
// and recorded: the answer names the best identity the other payors give, else payor[0]'s as
// before; and when payor[0]'s own read fails and no other payor names this payer, the answer is
// the system's failure, exactly as before. Payors are read in order until one names a published
// identity, so a later payor is never read. readsFailed names each failed read the answer was
// built without, in read order, and is absent otherwise; backend keeps its meaning (the last read).
func TestEligibilityInbound_AFailedReadIsSurvivedAndRecorded(t *testing.T) {
	member := dtrFrameMember
	held := map[string][]byte{}
	org := func(ref string) map[string]any { return map[string]any{"reference": ref} }
	failRef, authRef, okRef, ownRef := "Organization/failing", "Organization/auth-failing", "Organization/npi-org", "Organization/own-org"
	fail := map[string]error{failRef: &SoRReadError{Kind: SoRUnavailable}, authRef: &SoRReadError{Kind: SoRAuthenticationFailed}}
	unavailable, _ := SoRFailureResponse(&SoRReadError{Kind: SoRUnavailable})
	for _, row := range []struct {
		name        string
		payors      []any
		want        shnsdk.PayerIdentifier
		status      int
		readsFailed []string
		lastUsable  bool
		reads       map[string]int
	}{
		{name: "two reads fail before the published payor, each named by its class, in read order",
			payors: []any{org(authRef), org(failRef), insurerInline(insurerPublished)},
			want:   insurerPublished, status: http.StatusOK, readsFailed: []string{BackendAuth, ExchangeOther}},
		{name: "a failed read of an exchange then refused is not recorded",
			payors: []any{map[string]any{"reference": "Organization/clinic"}, org(failRef)},
			status: http.StatusUnprocessableEntity},
		{name: "a payor after the published one is never read",
			payors: []any{insurerInline(insurerPublished), org(failRef)},
			want:   insurerPublished, status: http.StatusOK, reads: map[string]int{failRef: 0}},
		{name: "payor[0]'s read fails before the own payor's Organization, read after it",
			payors: []any{org(failRef), org(ownRef)},
			want:   insurerPublished, status: http.StatusOK, readsFailed: []string{ExchangeOther}, lastUsable: true},
		{name: "a later payor's read fails and none is this payer's: read as before, payor[0] read once",
			payors: []any{org(okRef), org(failRef)},
			want:   insurerNPI, status: http.StatusOK, readsFailed: []string{ExchangeOther}, reads: map[string]int{okRef: 1}},
		{name: "payor[0]'s own read fails and none is this payer's: the system's failure, as before, read once",
			payors: []any{org(failRef), insurerInline(insurerNPI)},
			status: unavailable, reads: map[string]int{failRef: 1}},
		{name: "every read succeeds",
			payors: []any{org(okRef), insurerInline(insurerPublished)},
			want:   insurerPublished, status: http.StatusOK, lastUsable: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := eligibilityPayer(t, EnforcementObserve, false)
			p.g.cfg.Reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", PayerIDs: []shnsdk.PayerIdentifier{insurerPublished}})
			held[okRef] = mustJSON(t, insurerOrg("npi-org", insurerNPI))
			held[ownRef] = mustJSON(t, insurerOrg("own-org", insurerNPI, insurerPublished))
			base := heldRefSoR{SystemOfRecord: p.g.cfg.SoR, refs: held, member: member, coverage: insurerCoverage(t, nil, row.payors...)}
			sor, reads := failingRefSoR(t, base, fail)
			p.g.cfg.SoR = sor
			var got exchangeRecords
			p.g.cfg.ExchangeObserved = got.observe
			ans := p.send(t, "coverage-eligibility", "", eligibilityRequest(t, member))
			if ans.status != row.status {
				t.Fatalf("answer %d %s, want %d", ans.status, ans.body, row.status)
			}
			rec := got.only(t)
			if !slices.Equal(rec.ReadsFailed, row.readsFailed) {
				t.Fatalf("readsFailed = %q, want %q", rec.ReadsFailed, row.readsFailed)
			}
			for ref, n := range row.reads {
				if reads[ref] != n {
					t.Fatalf("%s read %d times, want %d", ref, reads[ref], n)
				}
			}
			if row.status != http.StatusOK {
				return
			}
			var answer struct {
				Insurer struct {
					Identifier shnsdk.PayerIdentifier `json:"identifier"`
				} `json:"insurer"`
			}
			if err := json.Unmarshal(ans.body, &answer); err != nil {
				t.Fatalf("%v: %s", err, ans.body)
			}
			if answer.Insurer.Identifier != row.want {
				t.Fatalf("insurer = %+v, want %+v", answer.Insurer.Identifier, row.want)
			}
			// backend keeps its meaning: the last read, usable after a failed one.
			if row.lastUsable && (rec.Backend == nil || rec.Backend.ErrorClass != "") {
				t.Fatalf("backend = %+v, want the last read, usable, as before", rec.Backend)
			}
		})
	}
}

// With no Coverage, the answer names the payer's own Organization by the same rule.
func TestEligibilityAnswerInsurer_TheOwnOrganization(t *testing.T) {
	pub := []shnsdk.PayerIdentifier{insurerPublished}
	for _, row := range []struct {
		name      string
		published []shnsdk.PayerIdentifier
		org       map[string]any
		want      shnsdk.PayerIdentifier
		wantOK    bool
	}{
		{"an NPI before the published identity", pub, insurerOrg("payer", insurerNPI, insurerPublished), insurerPublished, true},
		{"none of this payer's identities: read as before", pub, insurerOrg("payer", insurerNPI, insurerOther), insurerNPI, true},
		{"no identity known: read as before", nil, insurerOrg("payer", insurerNPI, insurerPublished), insurerNPI, true},
		{"not an Organization", pub, map[string]any{"resourceType": "Patient", "id": "payer"}, shnsdk.PayerIdentifier{}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			g, _ := insurerGateway(t, row.published)
			got, ok := g.organizationAnswerInsurer(mustJSON(t, row.org))
			if ok != row.wantOK || got != row.want {
				t.Fatalf("insurer = %+v (ok=%v), want %+v (ok=%v)", got, ok, row.want, row.wantOK)
			}
		})
	}
}

// The answer a provider receives names the payer's network identity: a member Coverage whose
// Organization lists an NPI first names the published identity, never the NPI.
func TestEligibilityInbound_InsurerIsThePayersNetworkIdentity(t *testing.T) {
	const member = "MBR-COVERED"
	for _, row := range []struct {
		name      string
		published []shnsdk.PayerIdentifier
		coverage  func(t *testing.T) []byte
		want      shnsdk.PayerIdentifier
		absent    shnsdk.PayerIdentifier
	}{
		{"an NPI before the published identity", []shnsdk.PayerIdentifier{insurerPublished}, func(t *testing.T) []byte {
			return insurerCoverage(t, []any{insurerOrg("o1", insurerNPI, insurerPublished)}, map[string]any{"reference": "#o1"})
		}, insurerPublished, insurerNPI},
		{"an NPI-only payor before the published one", []shnsdk.PayerIdentifier{insurerPublished}, func(t *testing.T) []byte {
			return insurerCoverage(t, []any{insurerOrg("o1", insurerNPI)}, map[string]any{"reference": "#o1"}, insurerInline(insurerPublished))
		}, insurerPublished, insurerNPI},
	} {
		t.Run(row.name, func(t *testing.T) {
			g, requester := insurerGateway(t, row.published)
			g.cfg.SoR = heldRefSoR{SystemOfRecord: g.cfg.SoR, member: member, coverage: row.coverage(t)}
			pci, _, found := g.cfg.SoR.ResolvePatient(member)
			if !found {
				t.Fatalf("fixture bug: %s not resolvable", member)
			}
			now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
			cerJSON, err := shnsdk.BuildEligibilityRequestExact(member, "1234567890", insurerPublished, now)
			if err != nil {
				t.Fatal(err)
			}
			env, err := shnsdk.Seal(shnsdk.Metadata{
				Sender: requester.ID, Recipient: "payer", TransactionType: "coverage-eligibility",
				AuthorityFrame: "provider-tpo", Timestamp: g.cfg.Clock().Format(time.RFC3339), CorrelationID: "corr-insurer",
			}, cerJSON, g.cfg.Identity.EncPub)
			if err != nil {
				t.Fatal(err)
			}
			tok := shnsdk.Token{Operation: "eligibility-inquiry", Subject: pci, CorrelationID: "corr-insurer"}
			rec := httptest.NewRecorder()
			g.handleEligibilityInbound(rec, newSignedInboundRequest(t, g, requester.ID), env, tok, cerJSON, "")
			if rec.Code != 200 {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			plain := openResponseLeg(t, requester, rec.Body.Bytes())
			var answer struct {
				Insurer struct {
					Identifier shnsdk.PayerIdentifier `json:"identifier"`
				} `json:"insurer"`
			}
			if err := json.Unmarshal(plain, &answer); err != nil {
				t.Fatal(err)
			}
			if answer.Insurer.Identifier != row.want {
				t.Fatalf("insurer = %+v, want %+v; answer=%s", answer.Insurer.Identifier, row.want, plain)
			}
			if strings.Contains(string(plain), row.absent.Value) {
				t.Fatalf("the answer names %s: %s", row.absent.Value, plain)
			}
		})
	}
}
