//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestRecipientForResolvesAndFailsClosed(t *testing.T) {
	router, _ := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078", HolderID: "acme-health"},
	})
	g := &Gateway{cfg: Config{PayerRouter: router, SoR: nil}}

	cov, _ := containedPayorCoverage("Patient/m", "m",
		shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078"})
	h, status, _ := g.recipientFor(context.Background(), cov)
	if status != 0 || h != "acme-health" {
		t.Fatalf("resolve: got (%q,%d)", h, status)
	}

	miss, _ := containedPayorCoverage("Patient/m", "m",
		shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "99999"})
	_, status, msg := g.recipientFor(context.Background(), miss)
	if status != http.StatusUnprocessableEntity || msg == "" {
		t.Fatalf("miss must be 422 + legible: got (%d,%q)", status, msg)
	}
}

// TestBundleRefsResolver proves the PAS Bundle resolver matches a present "<Type>/<id>" against
// an inbound Bundle's entries and misses an absent one (the Finding-1 fix core).
func TestBundleRefsResolver(t *testing.T) {
	bundle := []byte(`{"resourceType":"Bundle","entry":[
		{"resource":{"resourceType":"Organization","id":"payer-org","identifier":[{"system":"s","value":"v"}]}},
		{"resource":{"resourceType":"Patient","id":"p-1"}}
	]}`)
	r := readBundleRefs(bundle).resolver()
	got, ok := r("Organization/payer-org")
	if !ok {
		t.Fatal("present ref did not resolve")
	}
	var org struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if json.Unmarshal(got, &org) != nil || org.ResourceType != "Organization" || org.ID != "payer-org" {
		t.Fatalf("resolved wrong resource: %s", got)
	}
	if _, ok := r("Organization/absent"); ok {
		t.Fatal("absent ref must miss")
	}
	// A malformed bundle yields a resolver that always misses (fail-closed, never panics).
	if _, ok := readBundleRefs([]byte("not json")).resolver()("Organization/x"); ok {
		t.Fatal("malformed bundle must miss")
	}
}

// externalPayorPASBundle builds an inbound PAS Bundle whose Coverage.payor is the EXTERNAL entry
// form — payor:[{"reference":"Organization/payer-org"}] — with (withOrg) or without a SIBLING
// Organization entry carrying (system,value). This is the form br-payer's own findInBundle mandates
// for $submit and the case the two-RI smoke (which uses a CONTAINED #payor-org) never exercises.
func externalPayorPASBundle(system, value string, withOrg bool) []byte {
	org := ""
	if withOrg {
		org = `,{"resource":{"resourceType":"Organization","id":"payer-org","identifier":[{"system":"` + system + `","value":"` + value + `"}]}}`
	}
	return []byte(`{"resourceType":"Bundle","type":"collection","entry":[
		{"resource":{"resourceType":"Patient","id":"MBR-COVERED"}},
		{"resource":{"resourceType":"Coverage","id":"cov1","status":"active","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"reference":"Organization/payer-org"}]}},
		{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}}` + org + `]}`)
}

// TestPASIngressExternalPayorEntryRoutes is the Finding-1 regression guard: the inbound PAS-ingress
// routing expression — pasRecipient(body) — resolves
// an EXTERNAL bundle-entry payor Organization against the inbound bundle (NOT the provider SoR) and
// routes to the mapped holder; the SAME bundle without the sibling Organization, or with an unmapped
// identifier, fails closed 422 with no route. Before the fix this used the provider SoR resolver and
// MISSED the external form → 422 even for a mapped payer.
func TestPASIngressExternalPayorEntryRoutes(t *testing.T) {
	gw, _ := twoPayerTestSystem(t)

	// External entry-form carrying CMSPayerIdentity (00001 → payer-a in the 2-entry directory).
	bundle := externalPayorPASBundle(shnsdk.CMSPayerIdentity.System, shnsdk.CMSPayerIdentity.Value, true)
	recipient, pid, status, msg, _ := gw.pasRecipient(bundle)
	if status != 0 {
		t.Fatalf("external entry-form must route, got (%d,%q)", status, msg)
	}
	if recipient != "payer-a" {
		t.Fatalf("external entry-form routed to %q, want payer-a", recipient)
	}
	if pid != shnsdk.CMSPayerIdentity {
		t.Fatalf("parsed pid = %+v, want CMSPayerIdentity", pid)
	}

	// SAME bundle WITHOUT the sibling Organization → the external ref cannot resolve → 422, no route.
	noOrg := externalPayorPASBundle(shnsdk.CMSPayerIdentity.System, shnsdk.CMSPayerIdentity.Value, false)
	if r, _, s, _, why := gw.pasRecipient(noOrg); s != http.StatusUnprocessableEntity || r != "" || why != reasonNoPayerIdentifier {
		t.Fatalf("missing payor Organization must fail closed 422 with no route, got (%q,%d)", r, s)
	}

	// Present Organization but an UNMAPPED identifier (00099) → resolves the Org but no directory
	// entry → 422 "no registered payer".
	unmapped := externalPayorPASBundle(shnsdk.CMSPayerIdentity.System, "00099", true)
	if _, _, s, m, why := gw.pasRecipient(unmapped); s != http.StatusUnprocessableEntity || !strings.Contains(m, "no registered payer") || why != reasonUnregisteredPayer {
		t.Fatalf("unmapped identifier must 422 no-registered-payer, got (%d,%q)", s, m)
	}
}

// ---- hermetic two-payer routing proof (FR-G40) ----
//
// twoPayerSubstrate is a fake Hub+Authz RoundTripper for the multi-payer routing proof. Unlike
// stubSubstrate/homeOxygenSubstrate (originate_test.go / originate_homeoxygen_test.go), which
// hardcode the single recipient "payer", this stub accepts ANY recipient and RECORDS every
// /route call's env.Metadata.Recipient — the WIRE-LEVEL proof of which holder the leg was
// actually sealed and addressed to (not just recipientFor's in-memory return value). It reuses
// the shared crypto/seal helpers already defined in originate_test.go (genKeyPair / genED25519 /
// signTestToken / sealForProvider / errResp — same package, same file set).
type twoPayerSubstrate struct {
	authzPriv      ed25519.PrivateKey
	providerEncPub *[32]byte
	clock          func() time.Time

	mu         sync.Mutex
	recipients []string // env.Metadata.Recipient seen on each /route call, in order
}

func (s *twoPayerSubstrate) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	body, _ := io.ReadAll(req.Body)
	switch {
	case strings.HasSuffix(path, "/authorize"):
		return s.handleAuthorize(body)
	case strings.HasSuffix(path, "/route"):
		return s.handleRoute(body)
	default:
		return errResp("unexpected stub call to " + path), nil
	}
}

func (s *twoPayerSubstrate) handleAuthorize(body []byte) (*http.Response, error) {
	var req struct {
		Frame         string `json:"frame"`
		Operation     string `json:"operation"`
		SubjectPCI    string `json:"subjectPCI"`
		CorrelationID string `json:"correlationId"`
		PayloadHash   string `json:"payloadHash"`
	}
	_ = json.Unmarshal(body, &req)
	tok := shnsdk.Token{
		Operation:     req.Operation,
		Scope:         "crd-context",
		Subject:       req.SubjectPCI,
		Frame:         req.Frame,
		Holder:        "provider",
		CorrelationID: req.CorrelationID,
		Expiry:        s.clock().Add(time.Hour),
		PayloadHash:   req.PayloadHash,
	}
	tok = signTestToken(tok, s.authzPriv)
	b, _ := json.Marshal(map[string]any{"token": tok})
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// handleRoute is the crux of the proof: it decodes the SEALED outbound envelope and records
// env.Metadata.Recipient — whichever holder the gateway actually addressed the leg to — before
// replying with a canned crd-cards response echoed back to whoever the request named (so the
// SAME stub serves both persona A's payer-a leg and persona B's payer-b leg without per-call
// state). The response subject (pci) is read off the DECODED request token's Subject, which is
// plaintext envelope metadata (only the payload is encrypted), so no per-call pci field is needed
// on the stub itself.
func (s *twoPayerSubstrate) handleRoute(body []byte) (*http.Response, error) {
	env, err := hubDecodeEnvelope(body)
	if err != nil {
		return errResp("stub: decode envelope: " + err.Error()), nil
	}
	s.mu.Lock()
	s.recipients = append(s.recipients, env.Metadata.Recipient)
	s.mu.Unlock()

	var reqTok shnsdk.Token
	_ = json.Unmarshal([]byte(env.Metadata.AuthzToken), &reqTok)

	respPayload := crdAnswerFor(shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededNoAuth})
	meta := shnsdk.Metadata{
		Sender:          env.Metadata.Recipient, // echo the resolved holder back as Sender
		Recipient:       "provider",
		TransactionType: env.Metadata.TransactionType,
		AuthorityFrame:  "payer-coverage",
		Timestamp:       s.clock().UTC().Format(time.RFC3339),
		CorrelationID:   env.Metadata.CorrelationID,
	}
	out, err := sealForProvider(meta, respPayload, s.providerEncPub, s.authzPriv,
		env.Metadata.CorrelationID, "crd-cards", "payer-coverage", env.Metadata.Recipient, reqTok.Subject, s.clock())
	if err != nil {
		return errResp("stub: sealForProvider: " + err.Error()), nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(out)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// twoPayerTestSystem builds a provider Gateway wired to a TWO-ENTRY PayerRouter
// (00001→payer-a, 00078→payer-b) and a twoPayerSubstrate that records every sealed leg's
// Recipient. Both payer holders carry DISTINCT EncPub/SignPub in the registry, matching a real
// multi-payer network (not two names for the same key material).
func twoPayerTestSystem(t *testing.T) (*Gateway, *twoPayerSubstrate) {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	_, provSignPriv := genED25519(t)
	payerAEncPub, _ := genKeyPair(t)
	payerASignPub, _ := genED25519(t)
	payerBEncPub, _ := genKeyPair(t)
	payerBSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	stub := &twoPayerSubstrate{authzPriv: authzPriv, providerEncPub: provEncPub, clock: clock}

	reg := shnsdk.NewRegistry()
	reg.Set("provider", shnsdk.RegistryEntry{ID: "provider", Role: "provider", EncPub: provEncPub, SignPub: authzPub})
	reg.Set("payer-a", shnsdk.RegistryEntry{ID: "payer-a", Role: "payer", EncPub: payerAEncPub, SignPub: payerASignPub})
	reg.Set("payer-b", shnsdk.RegistryEntry{ID: "payer-b", Role: "payer", EncPub: payerBEncPub, SignPub: payerBSignPub})

	router, err := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: shnsdk.CMSPayerIdentity.System, Value: shnsdk.CMSPayerIdentity.Value, HolderID: "payer-a"},
		{System: shnsdk.CMSPayerIdentity.System, Value: "00078", HolderID: "payer-b"},
	})
	if err != nil {
		t.Fatalf("twoPayerTestSystem: build router: %v", err)
	}

	const fakeBase = "http://stub.test"
	gw := mustNew(t, Config{
		Role:            "provider",
		HolderID:        "provider",
		PayerRouter:     router,
		Identity:        shnsdk.Identity{HolderID: "provider", SignPriv: provSignPriv, EncPub: provEncPub, EncPriv: provEncPriv},
		AuthzURL:        fakeBase,
		AuthzPub:        authzPub,
		HubTransportPub: authzPub, // not used by provider (only inbound gateways check it)
		HubURL:          fakeBase,
		Reg:             reg,
		Validator:       shnsdk.NewFakeValidator(),
		SoR:             newCensusSoR(),
		Store:           newCensusSoR(),
		Clock:           clock,
		NPI:             "1234567893",
		Client:          &http.Client{Transport: stub},
	})
	return gw, stub
}

// TestRoutesToPayerNamedByCoverage is THE core routing proof: it proves — hermetically,
// through the REAL sealed-envelope round trip (recipientFor → Seal → Hub /route), not just
// recipientFor's return value — that persona A's Coverage (CMSPayerIdentity, 00001) routes to
// payer-a and persona B's DIFFERENT Coverage (00078) routes to payer-b. FR-G40 / AI-G11.
func TestRoutesToPayerNamedByCoverage(t *testing.T) {
	gw, stub := twoPayerTestSystem(t)
	ctx := context.Background()
	req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)

	// Persona A: MBR-COVERED's stub Coverage names CMSPayerIdentity (00001, the default every
	// pre-existing persona keeps) → the 2-entry router resolves holder "payer-a".
	pciA, _, foundA := gw.cfg.SoR.ResolvePatient("MBR-COVERED")
	if !foundA {
		t.Fatal("persona A: MBR-COVERED did not resolve")
	}
	covA, ok := gw.cfg.SoR.OpenCoverage("MBR-COVERED")
	if !ok {
		t.Fatal("persona A: OpenCoverage(MBR-COVERED) = false")
	}
	recipientA, status, msg := gw.recipientFor(context.Background(), covA)
	if status != 0 {
		t.Fatalf("persona A: recipientFor failed: (%d,%q)", status, msg)
	}
	if recipientA != "payer-a" {
		t.Fatalf("persona A: recipientFor = %q, want payer-a", recipientA)
	}
	if _, err := gw.OriginateLeg(ctx, req, recipientA, "crd-order-select", pciA, "corr-persona-a", "",
		Content{WorkstreamType: workstreamPA, Payload: testRequest([]byte(`{"resourceType":"Parameters"}`))}); err != nil {
		t.Fatalf("persona A: OriginateLeg failed: %v", err)
	}

	// Persona B: MBR-PAYERB's stub Coverage names a DIFFERENT payer (00078, via
	// censusPayerOverrides) → the SAME router resolves the DIFFERENT holder "payer-b".
	pciB, _, foundB := gw.cfg.SoR.ResolvePatient("MBR-PAYERB")
	if !foundB {
		t.Fatal("persona B: MBR-PAYERB did not resolve")
	}
	covB, ok := gw.cfg.SoR.OpenCoverage("MBR-PAYERB")
	if !ok {
		t.Fatal("persona B: OpenCoverage(MBR-PAYERB) = false")
	}
	recipientB, status, msg := gw.recipientFor(context.Background(), covB)
	if status != 0 {
		t.Fatalf("persona B: recipientFor failed: (%d,%q)", status, msg)
	}
	if recipientB != "payer-b" {
		t.Fatalf("persona B: recipientFor = %q, want payer-b", recipientB)
	}
	if _, err := gw.OriginateLeg(ctx, req, recipientB, "crd-order-select", pciB, "corr-persona-b", "",
		Content{WorkstreamType: workstreamPA, Payload: testRequest([]byte(`{"resourceType":"Parameters"}`))}); err != nil {
		t.Fatalf("persona B: OriginateLeg failed: %v", err)
	}

	// THE assertion: the fake Hub's /route calls recorded the SEALED envelope's Recipient — proof
	// at the wire level, not merely recipientFor's return value — that persona A → payer-a and
	// persona B → payer-b, and that the two personas' legs never cross.
	stub.mu.Lock()
	got := append([]string(nil), stub.recipients...)
	stub.mu.Unlock()
	if len(got) != 2 || got[0] != "payer-a" || got[1] != "payer-b" {
		t.Fatalf("sealed leg recipients = %v, want [payer-a payer-b]", got)
	}
}

// TestUnknownPayerFailsClosed proves the AI-G11/OWD-G10 fail-closed contract: a member whose
// Coverage names a payer identifier ABSENT from the directory (00099, MBR-PAYERUNKNOWN) gets a
// legible 422 — and, crucially, NO leg is ever sealed/sent to the Hub (the origination call sites
// all check recipientFor's status before calling OriginateLeg; this proves the fake Hub was never
// invoked, guarding against a future regression that removed that check).
func TestUnknownPayerFailsClosed(t *testing.T) {
	gw, stub := twoPayerTestSystem(t)

	if _, _, found := gw.cfg.SoR.ResolvePatient("MBR-PAYERUNKNOWN"); !found {
		t.Fatal("MBR-PAYERUNKNOWN did not resolve")
	}
	cov, ok := gw.cfg.SoR.OpenCoverage("MBR-PAYERUNKNOWN")
	if !ok {
		t.Fatal("OpenCoverage(MBR-PAYERUNKNOWN) = false")
	}

	recipient, status, msg := gw.recipientFor(context.Background(), cov)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: msg=%q", status, msg)
	}
	if recipient != "" {
		t.Fatalf("recipient = %q on failure, want empty", recipient)
	}
	if !strings.Contains(msg, "no registered payer for identifier") || !strings.Contains(msg, "00099") {
		t.Fatalf("422 message not legible: %q", msg)
	}

	stub.mu.Lock()
	got := append([]string(nil), stub.recipients...)
	stub.mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("no leg should have been sealed/sent to the Hub, got %v", got)
	}
}

// TestRecipientForSoR_ChoosesByDirectoryNotPosition: origination routes the
// member's own Coverage by the identifier the payer directory resolves,
// wherever the payor Organization lists it, and returns that identifier (the
// one an origination site reuses as the emitted payer), never the NPI in first
// position. No registered identifier is refused naming them; two registered
// ones naming different payers are ambiguous.
func TestRecipientForSoR_ChoosesByDirectoryNotPosition(t *testing.T) {
	const (
		npi = `{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}`
		ein = `{"system":"http://terminology.hl7.org/NamingSystem/USEIN","value":"7239994834"}`
		reg = `{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00078"}`
	)
	coverage := func(identifiers string) []byte {
		return []byte(`{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/m"},` +
			`"contained":[{"resourceType":"Organization","id":"p","identifier":[` + identifiers + `]}],"payor":[{"reference":"#p"}]}`)
	}
	router, _ := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078", HolderID: "acme-health"},
		{System: "http://terminology.hl7.org/NamingSystem/USEIN", Value: "7239994834", HolderID: "other-payer"},
	})
	g := &Gateway{cfg: Config{PayerRouter: router}}
	ctx := context.Background()

	holder, pid, status, msg := g.recipientForSoR(ctx, coverage(npi+","+reg))
	if status != 0 || holder != "acme-health" || pid != (shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078"}) {
		t.Fatalf("registered identifier after an NPI: got (%q, %v, %d, %q), want acme-health by 00078", holder, pid, status, msg)
	}
	if _, _, status, msg := g.recipientForSoR(ctx, coverage(npi)); status != http.StatusUnprocessableEntity ||
		!strings.HasPrefix(msg, "no registered payer for identifier http://hl7.org/fhir/sid/us-npi|1234567893: name the payer by an identifier the network registers for that payer") {
		t.Fatalf("no registered identifier: got (%d, %q)", status, msg)
	}
	if _, _, status, msg := g.recipientForSoR(ctx, coverage(ein+","+reg)); status != http.StatusUnprocessableEntity || msg != msgCoverageNamesTwoPayers {
		t.Fatalf("two payers: got (%d, %q), want 422 %q", status, msg, msgCoverageNamesTwoPayers)
	}
}

// TestRecipientForSoR_AFailedReadIsTheSystemsWhenNoPayerIsFound: a payor known
// by its own identifier already names its payer, so its reference is never
// read: a read that would fail stops nothing, and an unregistered identifier
// is the routing refusal it always was. A payor with no identifier of its own
// whose read fails leaves the payer unknown; the Organization that failed to
// load may have named it, so the answer is the system's failure (unknown is
// not empty), never a routing refusal.
func TestRecipientForSoR_AFailedReadIsTheSystemsWhenNoPayerIsFound(t *testing.T) {
	const reg = `{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00078"}`
	router, _ := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078", HolderID: "acme-health"},
	})
	g := &Gateway{cfg: Config{PayerRouter: router, SoR: refReadFails{SystemOfRecord: newCensusSoR(), ref: "Organization/payer-org"}}}
	coverage := func(payors string) []byte {
		return []byte(`{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/m"},` +
			`"contained":[{"resourceType":"Organization","id":"p","identifier":[` + reg + `]}],"payor":[` + payors + `]}`)
	}
	unavailable, _ := SoRFailureResponse(&SoRReadError{Kind: SoRUnavailable})
	for name, row := range map[string]struct {
		payors string
		status int
		holder string
	}{
		"a registered inline identifier":                                 {`{"reference":"Organization/payer-org","identifier":` + reg + `}`, 0, "acme-health"},
		"an unregistered inline identifier, its Organization never read": {`{"reference":"Organization/payer-org","identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}}`, http.StatusUnprocessableEntity, ""},
		"a registered payor, then one whose read fails":                  {`{"reference":"#p"},{"reference":"Organization/payer-org"}`, unavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			holder, _, status, msg := g.recipientForSoR(context.Background(), coverage(row.payors))
			if status != row.status || holder != row.holder {
				t.Fatalf("got (%q, %d, %q), want (%q, %d)", holder, status, msg, row.holder, row.status)
			}
		})
	}
}

// TestPayorCandidates_SeveralEntriesSDKTwin records a known difference from the
// published SDK twin. The gateway reads a payor reference several Organization
// entries of a Bundle answer as the union of their identifiers (resolveAmong),
// and the payer directory decides; shn-sdk v1.10.0's CoveragePayerCandidates
// given the whole Bundle takes the first entry. Nothing published promises the
// first entry. When the SDK reads the union too, this row fails: make it an
// equality row in the change that moves the gateway's pin to that release.
func TestPayorCandidates_SeveralEntriesSDKTwin(t *testing.T) {
	const (
		npi = `{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}`
		ein = `{"system":"http://terminology.hl7.org/NamingSystem/USEIN","value":"7239994834"}`
		cms = `{"system":"urn:oid:2.16.840.1.113883.6.300","value":"%s"}`
	)
	bundle := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/m"},"payor":[{"reference":"Organization/p"}]}},` +
		`{"resource":{"resourceType":"Organization","id":"p","identifier":[` + npi + `,` + strings.Replace(cms, "%s", "00001", 1) + `]}},` +
		`{"resource":{"resourceType":"Organization","id":"p","identifier":[` + npi + `,` + ein + `,` + strings.Replace(cms, "%s", "00002", 1) + `]}}]}`)
	id := func(system, value string) shnsdk.PayerIdentifier {
		return shnsdk.PayerIdentifier{System: system, Value: value}
	}
	var (
		nID   = id("http://hl7.org/fhir/sid/us-npi", "1234567893")
		eID   = id("http://terminology.hl7.org/NamingSystem/USEIN", "7239994834")
		c1    = id("urn:oid:2.16.840.1.113883.6.300", "00001")
		c2    = id("urn:oid:2.16.840.1.113883.6.300", "00002")
		sdk   = [][]shnsdk.PayerIdentifier{{nID, c1}}
		ours  = [][]shnsdk.PayerIdentifier{{nID, c1, eID, c2}}
		equal = func(a, b [][]shnsdk.PayerIdentifier) bool {
			return len(a) == len(b) && (len(a) == 0 || slices.Equal(a[0], b[0]))
		}
	)
	got, err := shnsdk.CoveragePayerCandidates(bundle, nil)
	if err != nil || !equal(got, sdk) {
		t.Fatalf("the SDK twin: %v %v, want its first entry %v; if it now reads the union, make this an equality row", got, err, sdk)
	}
	refs := readBundleRefs(bundle)
	got, err = shnsdk.CoveragePayerCandidates(refs.firstCoverage(), refs.resolver())
	if err != nil || !equal(got, ours) {
		t.Fatalf("the gateway: %v %v, want the union %v", got, err, ours)
	}
}

// TestCoveragesNameOnePayer_ByThePayersOwnIdentity: a payer's gateway reading its
// own member's Coverages knows who it is (the identities its holder publishes on
// the network feed). They name it when each carries one of its own identities on
// some payor, in any position; otherwise, as with no identity known, when they
// name the same identifiers in any order. Never by an identifier's position: a
// first NPI shared, then different identifiers, is ambiguous.
func TestCoveragesNameOnePayer_ByThePayersOwnIdentity(t *testing.T) {
	const cms = "urn:oid:2.16.840.1.113883.6.300"
	p := shnsdk.PayerIdentifier{System: cms, Value: "00001"}
	npi := shnsdk.PayerIdentifier{System: "http://hl7.org/fhir/sid/us-npi", Value: "1234567893"}
	other := shnsdk.PayerIdentifier{System: cms, Value: "00078"}
	ein := shnsdk.PayerIdentifier{System: "http://terminology.hl7.org/NamingSystem/USEIN", Value: "7239994834"}
	reg := shnsdk.NewRegistry()
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", PayerIDs: []shnsdk.PayerIdentifier{p}})
	payer := &Gateway{cfg: Config{Role: "payer", HolderID: "payer", Reg: reg}}
	// candidates is a Coverage's identifiers as routing reads them, every payor.
	candidates := func(payors string) []shnsdk.PayerIdentifier {
		t.Helper()
		cands, err := shnsdk.CoveragePayerCandidates([]byte(`{"resourceType":"Coverage","id":"c","status":"active",`+
			`"beneficiary":{"reference":"Patient/m"},"payor":[`+payors+`]}`), nil)
		if err != nil || len(cands) != 1 {
			t.Fatalf("candidates of %s: %v %v", payors, cands, err)
		}
		return cands[0]
	}
	inline := func(id shnsdk.PayerIdentifier) string {
		return `{"identifier":{"system":"` + id.System + `","value":"` + id.Value + `"}}`
	}
	for name, row := range map[string]struct {
		lists [][]shnsdk.PayerIdentifier
		one   bool
	}{
		"its identity, and its identity beside an NPI":          {[][]shnsdk.PayerIdentifier{{p}, {p, npi}}, true},
		"a shared NPI only, without its identity: the same set": {[][]shnsdk.PayerIdentifier{{npi}, {npi}}, true},
		"a first NPI, then different identifiers, none its own": {[][]shnsdk.PayerIdentifier{{npi, other}, {npi, ein}}, false},
		"its identity on one, an NPI only on the other":         {[][]shnsdk.PayerIdentifier{{p}, {npi}}, false},
		"its identity at payor[1], beside one at payor[0]": {[][]shnsdk.PayerIdentifier{
			candidates(inline(npi) + "," + inline(p)), candidates(inline(p))}, true},
		"another payer's identity and its own": {[][]shnsdk.PayerIdentifier{{other}, {p}}, false},
	} {
		t.Run("payer/"+name, func(t *testing.T) {
			if got := payer.coveragesNameOnePayer(row.lists); got != row.one {
				t.Fatalf("got %v, want %v", got, row.one)
			}
		})
	}
	g := &Gateway{}
	for name, row := range map[string]struct {
		lists [][]shnsdk.PayerIdentifier
		one   bool
	}{
		"the same set in another order":       {[][]shnsdk.PayerIdentifier{{p, npi}, {npi, p}}, true},
		"the same first identifier, one more": {[][]shnsdk.PayerIdentifier{{p}, {p, npi}}, false},
		"different identifiers":               {[][]shnsdk.PayerIdentifier{{p}, {other}}, false},
	} {
		t.Run("no directory/"+name, func(t *testing.T) {
			if got := g.coveragesNameOnePayer(row.lists); got != row.one {
				t.Fatalf("got %v, want %v", got, row.one)
			}
		})
	}
}

// TestMemberPayerOrganization_ThePayorNamingTheRoutedPayer: a payor naming the
// routed payer by its identifier alone references no record, so the record is
// the Organization of a later payor that lists it, never none (a request
// naming its payer inline first and by its Organization after is not left
// without an insurer record).
func TestMemberPayerOrganization_ThePayorNamingTheRoutedPayer(t *testing.T) {
	pid := shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078"}
	coverage := []byte(`{"resourceType":"Coverage","id":"c1","contained":[{"resourceType":"Organization","id":"org",` +
		`"identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00078"}]}],` +
		`"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00078"}},{"reference":"#org"}]}`)
	org, ref, status, msg, _ := (&Gateway{}).memberPayerOrganization(context.Background(), coverage, pid)
	if status != 0 || ref != "#org" || !bytes.Contains(org, []byte(`"id":"org"`)) {
		t.Fatalf("got (%s, %q, %d, %q), want the contained Organization by #org", org, ref, status, msg)
	}
}

// TestRecipientForSoR_ATypedPayorIsNotRead: a payor stating its type already
// settles what it names (a Patient names no payer), so its reference is never
// read: a read that would fail stops nothing, and the payer beside it routes.
func TestRecipientForSoR_ATypedPayorIsNotRead(t *testing.T) {
	router, _ := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078", HolderID: "acme-health"},
	})
	g := &Gateway{cfg: Config{PayerRouter: router, SoR: refReadFails{SystemOfRecord: newCensusSoR(), ref: "Patient/m"}}}
	coverage := []byte(`{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/m"},` +
		`"contained":[{"resourceType":"Organization","id":"p","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00078"}]}],` +
		`"payor":[{"type":"Patient","reference":"Patient/m"},{"reference":"#p"}]}`)
	if holder, _, status, msg := g.recipientForSoR(context.Background(), coverage); status != 0 || holder != "acme-health" {
		t.Fatalf("got (%q, %d, %q), want acme-health", holder, status, msg)
	}
}

// TestRecipientForCoverages_EachCoverageItsOwnRefusal: what one Coverage's
// payor references left in doubt never colours another's refusal: a Coverage
// whose payor nothing resolves is refused as itself, whatever an earlier one
// read.
func TestRecipientForCoverages_EachCoverageItsOwnRefusal(t *testing.T) {
	router, _ := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078", HolderID: "acme-health"},
	})
	g := &Gateway{cfg: Config{PayerRouter: router}}
	const urn = "urn:uuid:5e7a0c1d-0000-4000-8000-0000000000a1"
	refs := &payorRefs{disagree: payorDisagreesInRequest, local: []bundleRefEntry{
		{urn, "Organization", "o", []byte(`{"resourceType":"Organization","id":"o"}`)},
		{urn, "Patient", "m", []byte(`{"resourceType":"Patient","id":"m"}`)},
	}}
	a := []byte(`{"resourceType":"Coverage","id":"a","payor":[{"reference":"` + urn + `","identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00078"}}]}`)
	b := []byte(`{"resourceType":"Coverage","id":"b","payor":[{"reference":"Organization/missing"}]}`)
	if _, _, status, msg, _ := g.recipientForCoverages([][]byte{a}, refs); status != 0 {
		t.Fatalf("Coverage a: %d %q, want routed", status, msg)
	}
	if _, _, status, msg, _ := g.recipientForCoverages([][]byte{b}, refs); status != http.StatusUnprocessableEntity || msg != noPayerIdentifier {
		t.Fatalf("Coverage b after a: %d %q, want 422 %q", status, msg, noPayerIdentifier)
	}
}

// TestFHIRServerCarriedEntries_AReferenceAnotherPrefetchValueAnswers: routing
// resolves a payor reference among the request's other prefetch values too, so
// the carry reads it so: that payor counts (not carried again: it is in the
// request already), and the Organization read for the next payor is carried.
func TestFHIRServerCarriedEntries_AReferenceAnotherPrefetchValueAnswers(t *testing.T) {
	answer := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Coverage","id":"c0","status":"active",` +
		`"beneficiary":{"reference":"Patient/m"},"payor":[{"reference":"Organization/a"},{"reference":"Organization/b"}]}}]}`)
	orgA := []byte(`{"resourceType":"Organization","id":"a","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}]}`)
	orgB := []byte(`{"resourceType":"Organization","id":"b","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}`)
	entries, total, ok := fhirServerCarriedEntries(answer, [][]byte{orgB}, []bundleRefEntry{{"", "Organization", "a", orgA}}, "https://ehr.example/fhir")
	if !ok || total != 1 {
		t.Fatalf("ok %v total %d", ok, total)
	}
	var includes int
	for _, e := range entries {
		if e.mode == "include" {
			includes++
			if e.res.Page != 1 {
				t.Fatalf("included %+v, want the Organization read (page 1)", e.res)
			}
		}
	}
	if includes != 1 {
		t.Fatalf("%d includes, want the Organization read for payor[1]: %+v", includes, entries)
	}
}

// TestMemberPayerOrganization_ATypedPayorIsNotRead: a payor stating another type
// than Organization references no payer's record, even when it carries the
// routed payer's identifier: its reference is never read, so a read that would
// fail stops nothing, and no record is named for it.
func TestMemberPayerOrganization_ATypedPayorIsNotRead(t *testing.T) {
	pid := shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078"}
	g := &Gateway{cfg: Config{SoR: refReadFails{SystemOfRecord: newCensusSoR(), ref: "Practitioner/dr1"}}}
	coverage := []byte(`{"resourceType":"Coverage","id":"c1","payor":[{"type":"Practitioner","reference":"Practitioner/dr1",` +
		`"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00078"}}]}`)
	org, ref, status, msg, _ := g.memberPayerOrganization(context.Background(), coverage, pid)
	if status != 0 || org != nil || ref != "" {
		t.Fatalf("got (%s, %q, %d, %q), want no record and no read", org, ref, status, msg)
	}
}

// TestFHIRServerCarriedEntries_ATypedPayorCarriesNothing: routing reads nothing
// from the reference of a payor stating another type than Organization, so the
// carried coverage search result includes nothing for it, even an Organization
// the answer holds under that reference.
func TestFHIRServerCarriedEntries_ATypedPayorCarriesNothing(t *testing.T) {
	answer := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Coverage","id":"c0","status":"active",` +
		`"beneficiary":{"reference":"Patient/m"},"payor":[{"type":"Practitioner","reference":"Organization/x",` +
		`"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}},` +
		`{"resource":{"resourceType":"Organization","id":"x","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}}]}`)
	entries, total, ok := fhirServerCarriedEntries(answer, nil, nil, "https://ehr.example/fhir")
	if !ok || total != 1 {
		t.Fatalf("ok %v total %d", ok, total)
	}
	for _, e := range entries {
		if e.mode == "include" {
			t.Fatalf("included %+v, want nothing for a payor typed Practitioner", e)
		}
	}
}
