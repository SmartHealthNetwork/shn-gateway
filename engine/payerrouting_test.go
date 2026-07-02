package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

	cov, _ := shnsdk.BuildCoverageWithPayer("Patient/m", "Coverage/m",
		shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00078"})
	h, status, _ := g.recipientFor(cov)
	if status != 0 || h != "acme-health" {
		t.Fatalf("resolve: got (%q,%d)", h, status)
	}

	miss, _ := shnsdk.BuildCoverageWithPayer("Patient/m", "Coverage/m",
		shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "99999"})
	_, status, msg := g.recipientFor(miss)
	if status != http.StatusUnprocessableEntity || msg == "" {
		t.Fatalf("miss must be 422 + legible: got (%d,%q)", status, msg)
	}
}

// TestBundleRefResolver proves the inbound-payload resolver matches a present "<Type>/<id>" against
// an inbound Bundle's entries and misses an absent one (the Finding-1 fix core).
func TestBundleRefResolver(t *testing.T) {
	bundle := []byte(`{"resourceType":"Bundle","entry":[
		{"resource":{"resourceType":"Organization","id":"payer-org","identifier":[{"system":"s","value":"v"}]}},
		{"resource":{"resourceType":"Patient","id":"p-1"}}
	]}`)
	r := bundleRefResolver(bundle)
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
	if _, ok := bundleRefResolver([]byte("not json"))("Organization/x"); ok {
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
// routing expression — recipientForWith(pasBundleCoverage(body), bundleRefResolver(body)) — resolves
// an EXTERNAL bundle-entry payor Organization against the inbound bundle (NOT the provider SoR) and
// routes to the mapped holder; the SAME bundle without the sibling Organization, or with an unmapped
// identifier, fails closed 422 with no route. Before the fix this used the provider SoR resolver and
// MISSED the external form → 422 even for a mapped payer.
func TestPASIngressExternalPayorEntryRoutes(t *testing.T) {
	gw, _ := twoPayerTestSystem(t)

	// External entry-form carrying CMSPayerIdentity (00001 → payer-a in the 2-entry directory).
	bundle := externalPayorPASBundle(shnsdk.CMSPayerIdentity.System, shnsdk.CMSPayerIdentity.Value, true)
	recipient, pid, status, msg := gw.recipientForWith(pasBundleCoverage(bundle), bundleRefResolver(bundle))
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
	if r, _, s, _ := gw.recipientForWith(pasBundleCoverage(noOrg), bundleRefResolver(noOrg)); s != http.StatusUnprocessableEntity || r != "" {
		t.Fatalf("missing payor Organization must fail closed 422 with no route, got (%q,%d)", r, s)
	}

	// Present Organization but an UNMAPPED identifier (00099) → resolves the Org but no directory
	// entry → 422 "no registered payer".
	unmapped := externalPayorPASBundle(shnsdk.CMSPayerIdentity.System, "00099", true)
	if _, _, s, m := gw.recipientForWith(pasBundleCoverage(unmapped), bundleRefResolver(unmapped)); s != http.StatusUnprocessableEntity || !strings.Contains(m, "no registered payer") {
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
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		return errResp("stub: decode envelope: " + err.Error()), nil
	}
	s.mu.Lock()
	s.recipients = append(s.recipients, env.Metadata.Recipient)
	s.mu.Unlock()

	var reqTok shnsdk.Token
	_ = json.Unmarshal([]byte(env.Metadata.AuthzToken), &reqTok)

	respPayload, err := shnsdk.BuildCards(shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededNoAuth})
	if err != nil {
		return errResp("stub: BuildCards: " + err.Error()), nil
	}
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
	gw := New(Config{
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
		SoR:             NewStubHolderData(),
		Store:           NewStubHolderData(),
		Clock:           clock,
		NPI:             "1234567890",
		Client:          &http.Client{Transport: stub},
	})
	return gw, stub
}

// TestRoutesToPayerNamedByCoverage is THE Slice-1 core deliverable: it proves — hermetically,
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
	recipientA, status, msg := gw.recipientFor(covA)
	if status != 0 {
		t.Fatalf("persona A: recipientFor failed: (%d,%q)", status, msg)
	}
	if recipientA != "payer-a" {
		t.Fatalf("persona A: recipientFor = %q, want payer-a", recipientA)
	}
	if _, err := gw.OriginateLeg(ctx, req, recipientA, "crd-order-select", pciA, "corr-persona-a", "",
		Content{WorkstreamType: workstreamPA, Bytes: []byte(`{"resourceType":"Parameters"}`)}); err != nil {
		t.Fatalf("persona A: OriginateLeg failed: %v", err)
	}

	// Persona B: MBR-PAYERB's stub Coverage names a DIFFERENT payer (00078, via
	// stubPayerOverrides) → the SAME router resolves the DIFFERENT holder "payer-b".
	pciB, _, foundB := gw.cfg.SoR.ResolvePatient("MBR-PAYERB")
	if !foundB {
		t.Fatal("persona B: MBR-PAYERB did not resolve")
	}
	covB, ok := gw.cfg.SoR.OpenCoverage("MBR-PAYERB")
	if !ok {
		t.Fatal("persona B: OpenCoverage(MBR-PAYERB) = false")
	}
	recipientB, status, msg := gw.recipientFor(covB)
	if status != 0 {
		t.Fatalf("persona B: recipientFor failed: (%d,%q)", status, msg)
	}
	if recipientB != "payer-b" {
		t.Fatalf("persona B: recipientFor = %q, want payer-b", recipientB)
	}
	if _, err := gw.OriginateLeg(ctx, req, recipientB, "crd-order-select", pciB, "corr-persona-b", "",
		Content{WorkstreamType: workstreamPA, Bytes: []byte(`{"resourceType":"Parameters"}`)}); err != nil {
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

	recipient, status, msg := gw.recipientFor(cov)
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
