package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer-identity mapping changes a relayed request only by replacing
// the payer identifier's system and value strings. These rows run the
// mapping through nativeResponder.Handle against the network's own fixture
// messages and compare what the payer's system receives with an expectation
// built independently: the original bytes with exactly the declared string
// values replaced.

var (
	// payorNPI is the identity the fixtures' payer Organization entries carry.
	payorNPI = shnsdk.PayerIdentifier{System: "http://hl7.org/fhir/sid/us-npi", Value: "1234567893"}
	// payorMapped is the identity the payer's own system expects.
	payorMapped = shnsdk.PayerIdentifier{System: "urn:example:payer-backend", Value: "BACKEND-7"}
)

func payorFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "relayfidelity", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func payorDoc(t *testing.T, b []byte) *relay.Document {
	t.Helper()
	d, err := relay.Doc(peerBody(b))
	if err != nil {
		t.Fatalf("Doc: %v", err)
	}
	return d
}

// nodeAt resolves a path of member names and array indices from the root.
func nodeAt(t *testing.T, d *relay.Document, path ...string) relay.NodeID {
	t.Helper()
	n := d.Root()
	for _, p := range path {
		switch d.Kind(n) {
		case relay.KindObject:
			m, ok := d.Member(n, p)
			if !ok {
				t.Fatalf("path %v: no member %q", path, p)
			}
			n = m
		case relay.KindArray:
			i, err := strconv.Atoi(p)
			elems := d.Elems(n)
			if err != nil || i < 0 || i >= len(elems) {
				t.Fatalf("path %v: no element %q", path, p)
			}
			n = elems[i]
		default:
			t.Fatalf("path %v: %q is below a scalar", path, p)
		}
	}
	return n
}

func jsonPath(p string) []string { return strings.Split(p, ".") }

// withoutMember returns b with the member key of the object at objPath
// removed, and checks that the removal is the only difference: b is the
// result with exactly that member (and one separating comma) put back, and
// the result is still one well-formed document without the member.
func withoutMember(t *testing.T, b []byte, key string, objPath ...string) []byte {
	t.Helper()
	d := payorDoc(t, b)
	obj := nodeAt(t, d, objPath...)
	members := d.Members(obj)
	i := slices.IndexFunc(members, func(m relay.MemberRef) bool { return m.Name == key })
	if i < 0 {
		t.Fatalf("%v has no member %q", objPath, key)
	}
	var start, end int
	switch {
	case i > 0:
		_, start = d.Span(members[i-1].Value)
		_, end = d.Span(members[i].Value)
	case len(members) > 1:
		start = members[i].KeyStart
		end = members[i+1].KeyStart
	default:
		t.Fatalf("%v: removing the only member is not needed here", objPath)
	}
	out := slices.Concat(b[:start], b[end:])
	removed := b[start:end]
	if !bytes.Equal(slices.Concat(out[:start], removed, out[start:]), b) {
		t.Fatal("the twin differs from the original by more than the removed member")
	}
	trimmed := bytes.TrimLeft(removed, ", \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte(strconv.Quote(key))) {
		t.Fatalf("the removed span is not the %q member: %.40q", key, removed)
	}
	od := payorDoc(t, out)
	if _, ok := od.Member(nodeAt(t, od, objPath...), key); ok {
		t.Fatalf("the twin still carries %q", key)
	}
	return out
}

// tokenEdit replaces the string value at path with value.
type tokenEdit struct {
	path  string
	value string
}

// expectEdits returns b with exactly the string values at the given paths
// replaced by their JSON encodings, everything else unchanged.
func expectEdits(t *testing.T, b []byte, edits ...tokenEdit) []byte {
	t.Helper()
	d := payorDoc(t, b)
	type span struct {
		s, e int
		v    []byte
	}
	var spans []span
	for _, e := range edits {
		n := nodeAt(t, d, jsonPath(e.path)...)
		if d.Kind(n) != relay.KindString {
			t.Fatalf("%s is not a string", e.path)
		}
		s, end := d.Span(n)
		v, err := json.Marshal(e.value)
		if err != nil {
			t.Fatal(err)
		}
		spans = append(spans, span{s, end, v})
	}
	slices.SortFunc(spans, func(a, b span) int { return a.s - b.s })
	var out []byte
	at := 0
	for _, s := range spans {
		out = append(out, b[at:s.s]...)
		out = append(out, s.v...)
		at = s.e
	}
	return append(out, b[at:]...)
}

// insertIntoObject returns b with text inserted just after the opening
// brace of the object at objPath.
func insertIntoObject(t *testing.T, b []byte, text string, objPath ...string) []byte {
	t.Helper()
	d := payorDoc(t, b)
	s, _ := d.Span(nodeAt(t, d, objPath...))
	return slices.Concat(b[:s+1], []byte(text), b[s+1:])
}

// replaceValue returns b with the value at valuePath replaced by value.
func replaceValue(t *testing.T, b []byte, value []byte, valuePath ...string) []byte {
	t.Helper()
	d := payorDoc(t, b)
	s, e := d.Span(nodeAt(t, d, valuePath...))
	return slices.Concat(b[:s], value, b[e:])
}

// valueBytes returns the bytes of the value at valuePath.
func valueBytes(t *testing.T, b []byte, valuePath ...string) []byte {
	t.Helper()
	d := payorDoc(t, b)
	s, e := d.Span(nodeAt(t, d, valuePath...))
	return bytes.Clone(b[s:e])
}

// unsignedPASSubmit is the fixture PAS submit without its Bundle.signature.
func unsignedPASSubmit(t *testing.T) []byte {
	t.Helper()
	return withoutMember(t, payorFixture(t, "valid/pas-submit-2.0.json"), "signature")
}

// unsignedCRDRequest is the fixture CRD request without the signature on
// its prefetch coverage Bundle.
func unsignedCRDRequest(t *testing.T) []byte {
	t.Helper()
	return withoutMember(t, payorFixture(t, "valid/crd-order-sign-request.json"), "signature", "prefetch", "coverage")
}

// PAS fixture paths.
const (
	pasCoverageInlineSystem = "entry.4.resource.payor.0.identifier.system"
	pasCoverageInlineValue  = "entry.4.resource.payor.0.identifier.value"
	pasInsurerOrgSystem     = "entry.2.resource.identifier.0.system"
	pasInsurerOrgValue      = "entry.2.resource.identifier.0.value"
)

func pasSubmitResponder(t *testing.T, opts ...NativeOption) (*nativeResponder, *stubPartner) {
	t.Helper()
	p := newStubPartner(t)
	p.respByPath["/Claim/$submit"] = fixturePASResponse(t,
		[]byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`), true)
	return NewNativeResponder(p.srv.Client(), p.srv.URL, "order-sign", newCensusSoR(), fixedClock, opts...), p
}

func crdResponder(t *testing.T, opts ...NativeOption) (*nativeResponder, *stubPartner) {
	t.Helper()
	p := newStubPartner(t)
	p.respByPath["/cds-services/order-sign"] = crdPartnerCoverageCard
	return NewNativeResponder(p.srv.Client(), p.srv.URL, "order-sign", nil, nil, opts...), p
}

func handlePAS(t *testing.T, n *nativeResponder, body []byte) LegResult {
	t.Helper()
	res, err := n.Handle(context.Background(), "pas-claim", "corr-payor-edge", "MBR-COVERED", body)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return res
}

func handleCRD(t *testing.T, n *nativeResponder, body []byte) LegResult {
	t.Helper()
	res, err := n.Handle(context.Background(), "crd-order-select", "corr-payor-edge", "pci", body)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return res
}

func assertSent(t *testing.T, p *stubPartner, want []byte) {
	t.Helper()
	if p.lastBody == nil {
		t.Fatal("nothing reached the payer's system")
	}
	if !bytes.Equal(p.lastBody, want) {
		t.Fatalf("the payer's system received bytes other than the expected ones:\n got %d bytes\nwant %d bytes\nfirst difference at %d",
			len(p.lastBody), len(want), firstDifference(p.lastBody, want))
	}
}

func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func assertRefused(t *testing.T, res LegResult, p *stubPartner, status int, contains string) {
	t.Helper()
	if res.Status != status || !strings.Contains(res.Message, contains) {
		t.Fatalf("want %d containing %q, got %d %q", status, contains, res.Status, res.Message)
	}
	if p.lastBody != nil {
		t.Fatal("a refused request reached the payer's system")
	}
}

// requestOwnership is the ownership of the request the mapping produces for
// body on carrier.
func requestOwnership(t *testing.T, n *nativeResponder, body []byte, carrier payorEdgeCarrier) (relay.Payload, LegResult) {
	t.Helper()
	p, lr, err := n.payorEdgeRequest(peerBody(body), carrier, "application/fhir+json")
	if err != nil {
		t.Fatalf("payorEdgeRequest: %v", err)
	}
	return p, lr
}

func TestPayorEdge_NoOpByteIdentical(t *testing.T) {
	signed := payorFixture(t, "valid/pas-submit-2.0.json")
	t.Run("mapping off", func(t *testing.T) {
		n, p := pasSubmitResponder(t)
		if res := handlePAS(t, n, signed); res.Status != 0 {
			t.Fatalf("status %d: %s", res.Status, res.Message)
		}
		assertSent(t, p, signed)
	})
	t.Run("mapping to the identity the request already carries", func(t *testing.T) {
		// The Bundle is signed: a mapping that changes nothing is not an edit,
		// so it is relayed exactly instead of refused.
		n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, shnsdk.CMSPayerIdentity))
		if res := handlePAS(t, n, signed); res.Status != 0 {
			t.Fatalf("status %d: %s", res.Status, res.Message)
		}
		assertSent(t, p, signed)
		pl, _ := requestOwnership(t, n, signed, payorEdgePASBundle)
		if pl.Ownership() != relay.OwnershipRelayed {
			t.Fatalf("ownership %v, want relayed", pl)
		}
	})
	t.Run("CRD request, mapping to the carried identity", func(t *testing.T) {
		crd := payorFixture(t, "valid/crd-order-sign-request.json")
		n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, shnsdk.CMSPayerIdentity))
		if res := handleCRD(t, n, crd); res.Status != 0 {
			t.Fatalf("status %d: %s", res.Status, res.Message)
		}
		assertSent(t, p, crd)
	})
}

func TestPayorEdge_EditTouchesOnlyIdentityTokens(t *testing.T) {
	body := unsignedPASSubmit(t)
	n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
	if res := handlePAS(t, n, body); res.Status != 0 {
		t.Fatalf("status %d: %s", res.Status, res.Message)
	}
	// The Coverage names the payer inline; the Claim's insurer is an
	// Organization identified by NPI, not the mapped identity, so it is
	// left as sent.
	assertSent(t, p, expectEdits(t, body,
		tokenEdit{pasCoverageInlineSystem, payorMapped.System},
		tokenEdit{pasCoverageInlineValue, payorMapped.Value},
	))
	pl, _ := requestOwnership(t, n, body, payorEdgePASBundle)
	if pl.Ownership() != relay.OwnershipEdited || !slices.Equal(pl.Edits(), []relay.EditID{relay.EditPayorEdgeRestamp}) {
		t.Fatalf("payload %v, want edited by the payer-identity mapping only", pl)
	}
	t.Run("only the value differs", func(t *testing.T) {
		sameSystem := shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "00777"}
		n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, sameSystem))
		if res := handlePAS(t, n, body); res.Status != 0 {
			t.Fatalf("status %d: %s", res.Status, res.Message)
		}
		assertSent(t, p, expectEdits(t, body, tokenEdit{pasCoverageInlineValue, "00777"}))
	})
}

func TestPayorEdge_EntryMetadataPreserved(t *testing.T) {
	body := unsignedPASSubmit(t)
	// Every entry carries search, request, response and an unknown member
	// in addition to the fixture's own entry id and extension.
	const members = `"search":{"mode":"match","score":1.50},"request":{"method":"POST","url":"Claim"},` +
		`"response":{"status":"201 Created"},"x-unknown-entry-member":[1e2,{"nested":true}],`
	d := payorDoc(t, body)
	entries := len(d.Elems(nodeAt(t, d, "entry")))
	for i := entries - 1; i >= 0; i-- {
		body = insertIntoObject(t, body, members, "entry", strconv.Itoa(i))
	}
	d = payorDoc(t, body)
	for _, m := range []string{"id", "extension"} {
		if _, ok := d.Member(nodeAt(t, d, "entry", "0"), m); !ok {
			t.Fatalf("the fixture's first entry lost its %s", m)
		}
	}
	n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
	if res := handlePAS(t, n, body); res.Status != 0 {
		t.Fatalf("status %d: %s", res.Status, res.Message)
	}
	assertSent(t, p, expectEdits(t, body,
		tokenEdit{pasCoverageInlineSystem, payorMapped.System},
		tokenEdit{pasCoverageInlineValue, payorMapped.Value},
	))
	sent := payorDoc(t, p.lastBody)
	for i := range entries {
		var names []string
		for _, m := range sent.Members(nodeAt(t, sent, "entry", strconv.Itoa(i))) {
			names = append(names, m.Name)
		}
		for _, want := range []string{"search", "request", "response", "x-unknown-entry-member", "fullUrl", "resource"} {
			if !slices.Contains(names, want) {
				t.Errorf("entry %d lost %q: %v", i, want, names)
			}
		}
	}
}

func TestPayorEdge_DecimalsAndOrderPreserved(t *testing.T) {
	body := unsignedPASSubmit(t)
	n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
	if res := handlePAS(t, n, body); res.Status != 0 {
		t.Fatalf("status %d: %s", res.Status, res.Message)
	}
	for _, lexeme := range []string{"1.50", "9007199254740993", "1e2"} {
		if !bytes.Contains(p.lastBody, []byte(lexeme)) {
			t.Errorf("the number %s did not survive", lexeme)
		}
	}
	names := func(b []byte) []string {
		d := payorDoc(t, b)
		var out []string
		for i := range d.Len() {
			for _, m := range d.Members(relay.NodeID(i)) {
				out = append(out, m.Name)
			}
		}
		return out
	}
	if !slices.Equal(names(p.lastBody), names(body)) {
		t.Fatal("member order changed")
	}
	assertSent(t, p, expectEdits(t, body,
		tokenEdit{pasCoverageInlineSystem, payorMapped.System},
		tokenEdit{pasCoverageInlineValue, payorMapped.Value},
	))
}

func TestPayorEdge_ResolvesInlineTarget(t *testing.T) {
	// A bare CRD coverage naming the payer inline.
	crd := unsignedCRDRequest(t)
	cov := []byte(`{"resourceType":"Coverage","id":"inline-cov","status":"active",` +
		`"beneficiary":{"reference":"Patient/example"},` +
		`"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"},"display":"Payer"}]}`)
	body := replaceValue(t, crd, cov, "prefetch", "coverage")
	n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
	if res := handleCRD(t, n, body); res.Status != 0 {
		t.Fatalf("status %d: %s", res.Status, res.Message)
	}
	assertSent(t, p, expectEdits(t, body,
		tokenEdit{"prefetch.coverage.payor.0.identifier.system", payorMapped.System},
		tokenEdit{"prefetch.coverage.payor.0.identifier.value", payorMapped.Value},
	))
}

func TestPayorEdge_ResolvesContainedTarget(t *testing.T) {
	// The DTR fixture's Coverage references its contained payer ("#id"),
	// carried as a bare CRD coverage prefetch.
	cov := valueBytes(t, payorFixture(t, "valid/dtr-package-params-2.0.json"), "parameter", "0", "resource")
	body := replaceValue(t, unsignedCRDRequest(t), cov, "prefetch", "coverage")
	n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
	if res := handleCRD(t, n, body); res.Status != 0 {
		t.Fatalf("status %d: %s", res.Status, res.Message)
	}
	assertSent(t, p, expectEdits(t, body,
		tokenEdit{"prefetch.coverage.contained.0.identifier.0.system", payorMapped.System},
		tokenEdit{"prefetch.coverage.contained.0.identifier.0.value", payorMapped.Value},
	))
	t.Run("a missing contained target is refused naming the reference", func(t *testing.T) {
		broken := replaceValue(t, body, []byte(`"#no-such-org"`), "prefetch", "coverage", "payor", "0", "reference")
		n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		assertRefused(t, handleCRD(t, n, broken), p, http.StatusUnprocessableEntity, `"#no-such-org"`)
	})
}

func TestPayorEdge_ResolvesEntryTarget(t *testing.T) {
	// A PAS Coverage whose payor is only a reference to the payer
	// Organization entry, which the Claim's insurer references too: the
	// shared entry's identifier is mapped once.
	body := withoutMember(t, unsignedPASSubmit(t), "identifier", jsonPath("entry.4.resource.payor.0")...)
	n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(payorNPI, payorMapped))
	if res := handlePAS(t, n, body); res.Status != 0 {
		t.Fatalf("status %d: %s", res.Status, res.Message)
	}
	assertSent(t, p, expectEdits(t, body,
		tokenEdit{pasInsurerOrgSystem, payorMapped.System},
		tokenEdit{pasInsurerOrgValue, payorMapped.Value},
	))
	t.Run("an entry naming another payer is refused", func(t *testing.T) {
		n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		assertRefused(t, handlePAS(t, n, body), p, http.StatusBadRequest, "does not match")
	})
	t.Run("an insurer reference to no entry is refused naming the reference", func(t *testing.T) {
		broken := replaceValue(t, unsignedPASSubmit(t), []byte(`"Organization/absent-insurer"`), jsonPath("entry.0.resource.insurer.reference")...)
		n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		assertRefused(t, handlePAS(t, n, broken), p, http.StatusUnprocessableEntity, `"Organization/absent-insurer"`)
	})
	t.Run("a reference to no entry is refused naming the reference", func(t *testing.T) {
		broken := replaceValue(t, body, []byte(`"Organization/absent"`), jsonPath("entry.4.resource.payor.0.reference")...)
		n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(payorNPI, payorMapped))
		assertRefused(t, handlePAS(t, n, broken), p, http.StatusUnprocessableEntity, `"Organization/absent"`)
	})
}

func TestPayorEdge_ResolvesSiblingTarget(t *testing.T) {
	// A bare CRD coverage whose payor references another prefetch resource.
	cov := []byte(`{"resourceType":"Coverage","id":"sib-cov","status":"active",` +
		`"beneficiary":{"reference":"Patient/example"},"payor":[{"reference":"Organization/sib-payer"}]}`)
	body := replaceValue(t, unsignedCRDRequest(t), cov, "prefetch", "coverage")
	body = insertIntoObject(t, body, `"payer":{"resourceType":"Organization","id":"sib-payer","identifier":[`+
		`{"system":"","value":"ignored"},{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}],"name":"Payer"},`,
		"prefetch")
	n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
	if res := handleCRD(t, n, body); res.Status != 0 {
		t.Fatalf("status %d: %s", res.Status, res.Message)
	}
	// The first identifier with a non-empty system and value is the one
	// mapped.
	assertSent(t, p, expectEdits(t, body,
		tokenEdit{"prefetch.payer.identifier.1.system", payorMapped.System},
		tokenEdit{"prefetch.payer.identifier.1.value", payorMapped.Value},
	))
	t.Run("a sibling in DTR Parameters", func(t *testing.T) {
		params := []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"coverage","resource":` + string(cov) + `},` +
			`{"name":"referenced","resource":{"resourceType":"Organization","id":"sib-payer","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}}]}`)
		n, _ := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		pl, lr := requestOwnership(t, n, params, payorEdgeDTRParameters)
		if lr.Status != 0 {
			t.Fatalf("refused: %d %s", lr.Status, lr.Message)
		}
		want := expectEdits(t, params,
			tokenEdit{"parameter.1.resource.identifier.0.system", payorMapped.System},
			tokenEdit{"parameter.1.resource.identifier.0.value", payorMapped.Value},
		)
		if !bytes.Equal(relay.BytesForTest(pl), want) {
			t.Fatalf("got %s", relay.BytesForTest(pl))
		}
	})
}

func TestPayorEdge_AmbiguousTargetRefused(t *testing.T) {
	body := payorFixture(t, "invalid/ambiguous-payor-ref.json")
	n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(payorNPI, payorMapped))
	assertRefused(t, handlePAS(t, n, body), p, http.StatusUnprocessableEntity,
		`"Organization/InsurerExample" resolves to more than one resource`)
	t.Run("two contained resources with one id", func(t *testing.T) {
		cov := valueBytes(t, payorFixture(t, "valid/dtr-package-params-2.0.json"), "parameter", "0", "resource")
		body := replaceValue(t, unsignedCRDRequest(t), cov, "prefetch", "coverage")
		body = insertIntoObject(t, body, `{"resourceType":"Organization","id":"payor-org","identifier":[`+
			`{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00002"}]},`, "prefetch", "coverage", "contained")
		n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		assertRefused(t, handleCRD(t, n, body), p, http.StatusUnprocessableEntity, `"#payor-org" resolves to more than one resource`)
	})
}

func TestPayorEdge_MixedPayersRefused(t *testing.T) {
	body := payorFixture(t, "invalid/mixed-payer-coverages.json")
	for _, own := range []shnsdk.PayerIdentifier{shnsdk.CMSPayerIdentity, {System: shnsdk.CMSPayerIdentity.System, Value: "00002"}} {
		n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(own, payorMapped))
		assertRefused(t, handlePAS(t, n, body), p, http.StatusUnprocessableEntity, "name more than one payer")
	}
}

func TestPayorEdge_SignedCoverageRefused(t *testing.T) {
	t.Run("signed PAS Bundle", func(t *testing.T) {
		n, p := pasSubmitResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		assertRefused(t, handlePAS(t, n, payorFixture(t, "valid/pas-submit-2.0.json")), p,
			http.StatusUnprocessableEntity, "signed content cannot be edited (E-03, Bundle.signature)")
	})
	t.Run("signed CRD coverage prefetch Bundle", func(t *testing.T) {
		n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		assertRefused(t, handleCRD(t, n, payorFixture(t, "valid/crd-order-sign-request.json")), p,
			http.StatusUnprocessableEntity, "signed content cannot be edited (E-03, Bundle.signature)")
	})
	t.Run("Coverage carrying a Signature element", func(t *testing.T) {
		cov := []byte(`{"resourceType":"Coverage","id":"signed-cov","status":"active",` +
			`"extension":[{"url":"http://example.org/fhir/StructureDefinition/coverage-attestation","valueSignature":` +
			`{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-06-19T21:53:54+00:00","who":{"reference":"Practitioner/p1"}}}],` +
			`"beneficiary":{"reference":"Patient/example"},` +
			`"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}`)
		body := replaceValue(t, unsignedCRDRequest(t), cov, "prefetch", "coverage")
		n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		assertRefused(t, handleCRD(t, n, body), p,
			http.StatusUnprocessableEntity, "signed content cannot be edited (E-03, Signature in Coverage)")
	})
}

func TestPayorEdge_CRDBundlePrefetch(t *testing.T) {
	body := unsignedCRDRequest(t)
	n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
	if res := handleCRD(t, n, body); res.Status != 0 {
		t.Fatalf("status %d: %s", res.Status, res.Message)
	}
	assertSent(t, p, expectEdits(t, body,
		tokenEdit{"prefetch.coverage.entry.1.resource.identifier.0.system", payorMapped.System},
		tokenEdit{"prefetch.coverage.entry.1.resource.identifier.0.value", payorMapped.Value},
	))
	t.Run("a Bundle naming another payer is refused", func(t *testing.T) {
		n, p := crdResponder(t, WithPayorEdgeIdentity(payorNPI, payorMapped))
		assertRefused(t, handleCRD(t, n, body), p, http.StatusBadRequest, "does not match")
	})
	t.Run("a Bundle with no Coverage is refused", func(t *testing.T) {
		empty := replaceValue(t, body, []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`), "prefetch", "coverage")
		n, p := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		assertRefused(t, handleCRD(t, n, empty), p, http.StatusBadRequest, "no resolvable payor identifier")
	})
}

func TestPayorEdge_RepeatedDTRCoverages(t *testing.T) {
	fixture := payorFixture(t, "valid/dtr-package-params-2.0.json")
	cov := valueBytes(t, fixture, "parameter", "0", "resource")
	second := []byte(`{"name":"coverage","resource":{"resourceType":"Coverage","id":"coverage-2","status":"active",` +
		`"beneficiary":{"reference":"Patient/example"},` +
		`"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}},`)
	d := payorDoc(t, fixture)
	s, _ := d.Span(nodeAt(t, d, "parameter"))
	body := slices.Concat(fixture[:s+1], second, fixture[s+1:])
	n, _ := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
	pl, lr := requestOwnership(t, n, body, payorEdgeDTRParameters)
	if lr.Status != 0 {
		t.Fatalf("refused: %d %s", lr.Status, lr.Message)
	}
	want := expectEdits(t, body,
		tokenEdit{"parameter.0.resource.payor.0.identifier.system", payorMapped.System},
		tokenEdit{"parameter.0.resource.payor.0.identifier.value", payorMapped.Value},
		tokenEdit{"parameter.1.resource.contained.0.identifier.0.system", payorMapped.System},
		tokenEdit{"parameter.1.resource.contained.0.identifier.0.value", payorMapped.Value},
	)
	if got := relay.BytesForTest(pl); !bytes.Equal(got, want) {
		t.Fatalf("both coverages must be mapped and nothing else changed; first difference at %d", firstDifference(got, want))
	}
	if !bytes.Equal(valueBytes(t, body, "parameter", "1", "resource"), cov) {
		t.Fatal("the fixture's coverage moved")
	}
	t.Run("coverages naming two payers are refused", func(t *testing.T) {
		mixed := replaceValue(t, body, []byte(`"00002"`), jsonPath("parameter.0.resource.payor.0.identifier.value")...)
		n, _ := crdResponder(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, payorMapped))
		_, lr := requestOwnership(t, n, mixed, payorEdgeDTRParameters)
		if lr.Status != http.StatusUnprocessableEntity || !strings.Contains(lr.Message, "name more than one payer") {
			t.Fatalf("got %d %q", lr.Status, lr.Message)
		}
	})
	t.Run("a coverage naming another payer is refused, not skipped", func(t *testing.T) {
		n, _ := crdResponder(t, WithPayorEdgeIdentity(payorNPI, payorMapped))
		_, lr := requestOwnership(t, n, body, payorEdgeDTRParameters)
		if lr.Status != http.StatusBadRequest {
			t.Fatalf("got %d %q", lr.Status, lr.Message)
		}
	})
	t.Run("no coverage parameter is sent exactly", func(t *testing.T) {
		none := []byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire","valueCanonical":"http://example.org/q"}]}`)
		pl, lr := requestOwnership(t, n, none, payorEdgeDTRParameters)
		if lr.Status != 0 || pl.Ownership() != relay.OwnershipRelayed {
			t.Fatalf("got %v %d %q", pl, lr.Status, lr.Message)
		}
	})
}

func TestPayorEdgePreparationOpaqueWithoutMapping(t *testing.T) {
	body := []byte(`{bad`)
	n := NewNativeResponder(nil, "", "order-sign", nil, nil)
	p, lr, err := n.payorEdgeRequest(peerBody(body), payorEdgeCRDRequest, "application/json; charset=utf-8")
	if err != nil || lr.Status != 0 || !bytes.Equal(relay.BytesForTest(p), body) || p.ContentType() != "application/json; charset=utf-8" {
		t.Fatalf("unconfigured mapping: %v %+v %v", p, lr, err)
	}
}

func TestPayorEdgePreparationMalformedIsAdaptationFailure(t *testing.T) {
	n := NewNativeResponder(nil, "", "order-sign", nil, nil, WithPayorEdgeIdentity(ownIdentity, backendIdentity))
	p, lr, err := n.payorEdgeRequest(peerBody([]byte(`{bad`)), payorEdgeCRDRequest, "application/json")
	if err != nil || p.Ownership() != 0 || lr.Status != http.StatusServiceUnavailable || lr.Message != "adaptation_unavailable" {
		t.Fatalf("configured mapping: %v %+v %v", p, lr, err)
	}
}
