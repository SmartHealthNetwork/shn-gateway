package relay

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// Mutation rows: each starts from a legitimate transmit and changes one
// thing a faulty or hostile code path could change. Every row must be
// refused, either when the payload is built or when it is transmitted.
//
// boundary stands in for a transmit boundary's permission check: it admits
// the listed ownership values, edit ids and builder ids and nothing else.
// A boundary with a key uses the real ownership table instead.
type boundary struct {
	name     string
	allowed  []Ownership
	edits    []EditID
	builders []BuilderID
	key      *Key
}

var errBoundaryRefused = errors.New("payload not permitted at this boundary")

func (b boundary) check(p Payload) error {
	if b.key != nil {
		if err := Check(*b.key)(p); err != nil {
			return errors.Join(errBoundaryRefused, err)
		}
		return nil
	}
	if !slices.Contains(b.allowed, p.Ownership()) {
		return errBoundaryRefused
	}
	for _, e := range p.Edits() {
		if !slices.Contains(b.edits, e) {
			return errBoundaryRefused
		}
	}
	if p.Ownership() == OwnershipAuthored && !slices.Contains(b.builders, p.Builder()) {
		return errBoundaryRefused
	}
	return nil
}

var (
	// relayOnly is a boundary that forwards a peer's message unchanged,
	// such as a payer gateway answering the network with its payer's
	// decision.
	relayOnly = boundary{name: "relay-only", allowed: []Ownership{OwnershipRelayed}}
	// payerRequest is a payer gateway sending a request to its payer: exact,
	// or with the payer identity mapped.
	payerRequest = boundary{name: "payer-request", allowed: []Ownership{OwnershipRelayed, OwnershipEdited},
		edits: []EditID{EditPayorEdgeRestamp}}
	// providerCRD is the provider gateway forwarding an EHR's CDS Hooks
	// request.
	providerCRD = boundary{name: "provider-crd-request", allowed: []Ownership{OwnershipRelayed, OwnershipEdited},
		edits: []EditID{EditCDSCallbackStrip, EditCDSPrefetchObtain}}
	// refusalWriter answers with the gateway's own refusal.
	refusalWriter = boundary{name: "refusal", allowed: []Ownership{OwnershipAuthored},
		builders: []BuilderID{BuilderGatewayRefusal}}
	// realRelay is a real relay-only transmit from the ownership table: a
	// payer gateway answering the network with its payer's questionnaire
	// package.
	realRelay = boundary{name: "table: questionnaire package answer",
		key: &Key{"dtr-questionnaire-fetch", RoleRecipient, DirectionResponse, OutcomeAnswered}}
	// realRelayToEHR is the provider gateway handing the payer's
	// prior-authorization answer to its EHR.
	realRelayToEHR = boundary{name: "table: prior-authorization answer to the EHR",
		key: &Key{"pas-claim", RoleRequester, DirectionResponse, OutcomeAnswered}}
	// realPayerRequest is a payer gateway sending the network's
	// prior-authorization request to its payer, from the ownership table.
	realPayerRequest = boundary{name: "table: prior-authorization request to the payer",
		key: &Key{"pas-claim", RoleRecipient, DirectionRequest, OutcomeCarried}}
	// realQuestionnaireRequest is a payer gateway sending a questionnaire
	// operation's input to its payer, from the ownership table: exact, with
	// the payer identity mapped, or the older envelope's rebuilt request.
	realQuestionnaireRequest = boundary{name: "table: questionnaire request to the payer",
		key: &Key{"dtr-questionnaire-fetch", RoleRecipient, DirectionRequest, OutcomeCarried}}
	// realProviderCRD is the provider gateway sending its EHR's CDS Hooks
	// request to the network, from the ownership table: exact, or with the
	// callback removed and absent prefetch values added.
	realProviderCRD = boundary{name: "table: the EHR's CDS Hooks request to the network",
		key: &Key{"crd-order-select", RoleRequester, DirectionRequest, OutcomeCarried}}
	// realPayerCRDAnswer and realPayerDispatchAnswer are a payer gateway
	// answering the network with its payer's CDS Hooks answer, from the
	// ownership table: relayed exactly, never rebuilt.
	realPayerCRDAnswer = boundary{name: "table: the payer's CDS Hooks answer",
		key: &Key{"crd-order-select", RoleRecipient, DirectionResponse, OutcomeAnswered}}
	realPayerDispatchAnswer = boundary{name: "table: the payer's order-dispatch answer",
		key: &Key{"crd-order-dispatch", RoleRecipient, DirectionResponse, OutcomeAnswered}}
	// realPayerCRDRequest and realPayerDispatchRequest are a payer gateway
	// sending the network's CDS Hooks request to its payer, from the
	// ownership table: exact, or with the payer identity mapped.
	realPayerCRDRequest = boundary{name: "table: the CDS Hooks request to the payer",
		key: &Key{"crd-order-select", RoleRecipient, DirectionRequest, OutcomeCarried}}
	realPayerDispatchRequest = boundary{name: "table: the order-dispatch request to the payer",
		key: &Key{"crd-order-dispatch", RoleRecipient, DirectionRequest, OutcomeCarried}}
	// realProviderDispatch is the provider gateway sending its EHR's
	// order-dispatch request to the network.
	realProviderDispatch = boundary{name: "table: the EHR's order-dispatch request to the network",
		key: &Key{"crd-order-dispatch", RoleRequester, DirectionRequest, OutcomeCarried}}
	// realProviderDTR is the provider gateway sending its EHR's
	// questionnaire package request to the network: exact, or with the
	// patient's Coverage added.
	realProviderDTR = boundary{name: "table: the EHR's questionnaire package request to the network",
		key: &Key{"dtr-questionnaire-fetch", RoleRequester, DirectionRequest, OutcomeCarried}}
	boundaries = []boundary{relayOnly, payerRequest, providerCRD, refusalWriter, realRelay, realRelayToEHR, realPayerRequest, realQuestionnaireRequest, realProviderCRD,
		realPayerCRDAnswer, realPayerDispatchAnswer, realPayerCRDRequest, realPayerDispatchRequest, realProviderDispatch, realProviderDTR}
)

type mutationRow struct {
	name string
	// build makes the payload; a refusal here satisfies the row when
	// wantBuild matches.
	build     func(t *testing.T) (Payload, error)
	wantBuild error
	// at lists the boundaries that must refuse the payload (all of them
	// when nil).
	at           []boundary
	wantTransmit error
}

func mutationRows() []mutationRow {
	signedBody := func(t *testing.T) (Body, *Document) {
		b := NewBody(readFixture(t, "valid/pas-submit-2.0.json"), OriginPeerFrame)
		return b, mustDoc(t, b)
	}
	return []mutationRow{
		{
			name: "authored payload at a relay-only boundary",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderSDKPASSubmit, []byte(`{"resourceType":"Bundle"}`), fhirJSON)
			},
			at:           []boundary{relayOnly, payerRequest, providerCRD, realRelay, realRelayToEHR, realPayerRequest, realQuestionnaireRequest, realProviderCRD, realProviderDTR},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "refusal builder at a relay-only boundary",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderGatewayRefusal, []byte(`{"resourceType":"OperationOutcome"}`), fhirJSON)
			},
			at:           []boundary{relayOnly, payerRequest, providerCRD, realRelay, realRelayToEHR, realPayerRequest, realQuestionnaireRequest, realProviderCRD, realProviderDTR},
			wantTransmit: errBoundaryRefused,
		},
		{
			name:         "zero payload",
			build:        func(t *testing.T) (Payload, error) { return Payload{}, nil },
			wantTransmit: ErrUnsetOwnership,
		},
		{
			name: "edit the boundary does not permit",
			build: func(t *testing.T) (Payload, error) {
				b := NewBody([]byte(`{"hook":"order-sign","fhirServer":"https://ehr.example"}`), OriginIngressRequest)
				d := mustDoc(t, b)
				return Apply(b, "application/json", EditPayorEdgeRestamp, d.Replace(at(t, d, "hook"), []byte(`"order-select"`)))
			},
			at:           []boundary{relayOnly, providerCRD, refusalWriter, realRelay, realRelayToEHR, realProviderCRD},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "a gateway-built CDS Hooks request in place of the EHR's",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderSDKCRDRequest, []byte(`{"hook":"order-select"}`), "application/json")
			},
			at:           []boundary{realProviderCRD},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "a system-of-record searchset sent as the EHR's CDS Hooks request",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderSoRSearchset, []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"entry":[]}`), fhirJSON)
			},
			at:           []boundary{realProviderCRD},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "the questionnaire projection at the EHR's CDS Hooks request",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderInterimDTRProjection, []byte(`{"resourceType":"Parameters"}`), fhirJSON)
			},
			at:           []boundary{realProviderCRD},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "a gateway-built questionnaire request in place of the EHR's",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderSDKDTRPackage, []byte(`{"resourceType":"Parameters"}`), fhirJSON)
			},
			at:           []boundary{realProviderDTR},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "the questionnaire projection in place of the EHR's questionnaire request",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderInterimDTRProjection, []byte(`{"canonical":"q"}`), "application/json")
			},
			at:           []boundary{realProviderDTR},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "an edit other than the coverage addition on the EHR's questionnaire request",
			build: func(t *testing.T) (Payload, error) {
				b := NewBody([]byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire","valueCanonical":"q|1"}]}`), OriginIngressRequest)
				d := mustDoc(t, b)
				v := d.Elems(at(t, d, "parameter"))[0]
				canonical, _ := d.Member(v, "valueCanonical")
				return Apply(b, fhirJSON, EditCDSPrefetchObtain, d.Replace(canonical, []byte(`"q"`)))
			},
			at:           []boundary{realProviderDTR},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "cards the gateway built in place of the payer's CDS Hooks answer",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderGatewayRefusal, []byte(`{"cards":[{"summary":"Covered","indicator":"info"}]}`), "application/json")
			},
			at:           []boundary{realPayerCRDAnswer, realPayerDispatchAnswer},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "any gateway-built message on a CDS Hooks transmit",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderSDKCRDRequest, []byte(`{"hook":"order-sign"}`), "application/json")
			},
			at:           []boundary{realPayerCRDAnswer, realPayerDispatchAnswer, realPayerCRDRequest, realPayerDispatchRequest, realProviderCRD, realProviderDispatch},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "the payer's CDS Hooks answer with a merged member",
			build: func(t *testing.T) (Payload, error) {
				b := NewBody([]byte(`{"cards":[]}`), OriginUpstreamResponse)
				d := mustDoc(t, b)
				return Apply(b, "application/json", EditPayorEdgeRestamp, d.InsertMember(d.Root(), "systemActions", []byte(`[]`)))
			},
			at:           []boundary{realPayerCRDAnswer, realPayerDispatchAnswer},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "the hook rewritten on the request to the payer",
			build: func(t *testing.T) (Payload, error) {
				b := NewBody([]byte(`{"hook":"order-select","hookInstance":"h"}`), OriginPeerFrame)
				d := mustDoc(t, b)
				return Apply(b, "application/json", EditCDSPrefetchObtain, d.Replace(at(t, d, "hook"), []byte(`"order-sign"`)))
			},
			at:           []boundary{realPayerCRDRequest, realPayerDispatchRequest},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "the callback strip on the request to the payer",
			build: func(t *testing.T) (Payload, error) {
				b := NewBody([]byte(`{"hook":"order-sign","fhirServer":"https://ehr.example"}`), OriginPeerFrame)
				d := mustDoc(t, b)
				return Apply(b, "application/json", EditCDSCallbackStrip, d.RemoveMember(d.Root(), "fhirServer"))
			},
			at:           []boundary{realPayerCRDRequest, realPayerDispatchRequest},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "an edit other than the payer-identity mapping on a request to the payer",
			build: func(t *testing.T) (Payload, error) {
				b := NewBody([]byte(`{"resourceType":"Bundle","type":"collection","fhirServer":"https://ehr.example"}`), OriginPeerFrame)
				d := mustDoc(t, b)
				return Apply(b, fhirJSON, EditCDSCallbackStrip, d.RemoveMember(d.Root(), "fhirServer"))
			},
			at:           []boundary{payerRequest, realPayerRequest, realQuestionnaireRequest},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "the questionnaire envelope's rebuild at a prior-authorization request to the payer",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderInterimDTRProjection, []byte(`{"resourceType":"Parameters"}`), fhirJSON)
			},
			at:           []boundary{payerRequest, realPayerRequest, realRelay},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "a requester's questionnaire envelope sent on to the payer",
			build: func(t *testing.T) (Payload, error) {
				return Authored(BuilderLegacyDTREnvelope, []byte(`{"canonical":"q"}`), "application/json")
			},
			at:           []boundary{realQuestionnaireRequest},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "relayed payload at the refusal writer",
			build: func(t *testing.T) (Payload, error) {
				return Exact(NewBody([]byte(`{}`), OriginUpstreamResponse), "application/json"), nil
			},
			at:           []boundary{refusalWriter},
			wantTransmit: errBoundaryRefused,
		},
		{
			name: "splice changes a byte outside its span",
			build: func(t *testing.T) (Payload, error) {
				b := NewBody([]byte(`{"payor":"old","status":"active"}`), OriginPeerFrame)
				d := mustDoc(t, b)
				old := spliceHook
				spliceHook = func(sd *splice.Doc, ops ...splice.Op) ([]byte, []splice.Edit, error) {
					out, spans, err := sd.Apply(ops...)
					if err == nil {
						out[len(out)-3] = 'X'
					}
					return out, spans, err
				}
				defer func() { spliceHook = old }()
				return Apply(b, fhirJSON, EditPayorEdgeRestamp, d.Replace(at(t, d, "payor"), []byte(`"new"`)))
			},
			wantBuild: ErrVerifyMismatch,
		},
		{
			name: "edit inside signed content",
			build: func(t *testing.T) (Payload, error) {
				b, d := signedBody(t)
				return Apply(b, fhirJSON, EditPayorEdgeRestamp, d.Replace(at(t, d, "entry", "4", "resource", "id"), []byte(`"x"`)))
			},
			wantBuild: ErrSignedContent,
		},
		{
			name: "forged embed span",
			build: func(t *testing.T) (Payload, error) {
				src, d := signedBody(t)
				s, e := d.Span(at(t, d, "entry", "0", "resource"))
				return Authored(BuilderCDexFulfillment, []byte(`{"resourceType":"Bundle","entry":[]}`), fhirJSON,
					Embed{Source: src, Start: s, End: e, At: 0})
			},
			wantBuild: ErrEmbedMismatch,
		},
		{
			name: "decoded and re-encoded body under an unregistered builder",
			build: func(t *testing.T) (Payload, error) {
				src, _ := signedBody(t)
				var v map[string]any
				if err := Decode(src, &v); err != nil {
					t.Fatal(err)
				}
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				return Authored("reencoded-request", b, fhirJSON)
			},
			wantBuild: ErrUnknownBuilder,
		},
		{
			name: "decoded and re-encoded body under the reserved test id",
			build: func(t *testing.T) (Payload, error) {
				return Authored(builderTestInjected, []byte(`{}`), fhirJSON)
			},
			wantBuild: ErrReservedBuilder,
		},
	}
}

func TestMutationRowsAreRefused(t *testing.T) {
	for _, r := range mutationRows() {
		t.Run(r.name, func(t *testing.T) {
			p, err := r.build(t)
			if r.wantBuild != nil {
				if !errors.Is(err, r.wantBuild) {
					t.Fatalf("build: want %v, got %v", r.wantBuild, err)
				}
				if p.Ownership() != 0 {
					t.Fatal("a refused build must not yield a usable payload")
				}
				// Whatever a caller does with the result, no boundary sends it.
				for _, bd := range boundaries {
					if _, terr := Transmit(p, bd.check); !errors.Is(terr, ErrUnsetOwnership) {
						t.Fatalf("%s: transmitted a refused build: %v", bd.name, terr)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			targets := r.at
			if targets == nil {
				targets = boundaries
			}
			for _, bd := range targets {
				out, terr := Transmit(p, bd.check)
				if !errors.Is(terr, r.wantTransmit) || out != nil {
					t.Errorf("%s: want %v, got %v (%d bytes)", bd.name, r.wantTransmit, terr, len(out))
				}
				if bd.key != nil && r.wantTransmit == errBoundaryRefused && !errors.Is(terr, ErrOwnershipRefused) {
					t.Errorf("%s: the table did not refuse: %v", bd.name, terr)
				}
			}
		})
	}
}

// TestMutationBaselinesAreAdmitted proves the stub boundaries refuse for the
// reason a row names, not because they refuse everything.
func TestMutationBaselinesAreAdmitted(t *testing.T) {
	body := NewBody([]byte(`{"hook":"order-sign","fhirServer":"https://ehr.example"}`), OriginIngressRequest)
	d := mustDoc(t, body)
	edited, err := Apply(body, "application/json", EditCDSCallbackStrip, d.RemoveMember(d.Root(), "fhirServer"))
	if err != nil {
		t.Fatal(err)
	}
	obtainOp, prefetch := d.EnsureObjectMember(d.Root(), "prefetch")
	obtained, err := ApplyChanges(body, "application/json",
		Change{Edit: EditCDSCallbackStrip, Ops: []Op{d.RemoveMember(d.Root(), "fhirServer")}},
		Change{Edit: EditCDSPrefetchObtain, Ops: []Op{obtainOp, d.InsertMember(NodeID(prefetch), "serviceHistory", []byte("null"))}})
	if err != nil {
		t.Fatal(err)
	}
	restamped, err := Apply(body, "application/json", EditPayorEdgeRestamp, d.Replace(at(t, d, "fhirServer"), []byte(`"x"`)))
	if err != nil {
		t.Fatal(err)
	}
	refusal, err := Authored(BuilderGatewayRefusal, []byte(`{"resourceType":"OperationOutcome"}`), fhirJSON)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Authored(BuilderInterimDTRProjection, []byte(`{"resourceType":"Parameters"}`), fhirJSON)
	if err != nil {
		t.Fatal(err)
	}
	params := NewBody([]byte(`{"resourceType":"Parameters","parameter":[{"name":"order","resource":{}}]}`), OriginIngressRequest)
	pd := mustDoc(t, params)
	coverageAdded, err := Apply(params, fhirJSON, EditDTRCoverageObtain,
		pd.AppendElement(at(t, pd, "parameter"), []byte(`{"name":"coverage","resource":{"resourceType":"Coverage"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	patientAdded, err := Apply(params, fhirJSON, EditDTRPatientObtain,
		pd.AppendElement(at(t, pd, "parameter"), []byte(`{"name":"referenced","resource":{"resourceType":"Patient","id":"p1"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		bd boundary
		p  Payload
	}{
		{realProviderDTR, Exact(params, fhirJSON)},
		{realProviderDTR, coverageAdded},
		{realProviderDTR, patientAdded},
		{relayOnly, Exact(body, "application/json")},
		{payerRequest, Exact(body, "application/json")},
		{payerRequest, restamped},
		{providerCRD, edited},
		{refusalWriter, refusal},
		{realRelay, Exact(body, "application/json")},
		{realRelayToEHR, Exact(body, "application/json")},
		{realPayerRequest, Exact(body, "application/json")},
		{realPayerRequest, restamped},
		{realQuestionnaireRequest, Exact(body, "application/json")},
		{realQuestionnaireRequest, restamped},
		{realQuestionnaireRequest, projection},
		{realProviderCRD, Exact(body, "application/json")},
		{realProviderCRD, edited},
		{realProviderCRD, obtained},
		{realProviderDispatch, Exact(body, "application/json")},
		{realProviderDispatch, obtained},
		{realPayerCRDAnswer, Exact(body, "application/json")},
		{realPayerDispatchAnswer, Exact(body, "application/json")},
		{realPayerCRDRequest, Exact(body, "application/json")},
		{realPayerCRDRequest, restamped},
		{realPayerDispatchRequest, Exact(body, "application/json")},
		{realPayerDispatchRequest, restamped},
	} {
		if _, err := Transmit(c.p, c.bd.check); err != nil {
			t.Errorf("%s refused %v: %v", c.bd.name, c.p, err)
		}
	}
}
