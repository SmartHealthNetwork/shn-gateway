package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

func boundaryContext(body []byte, leg string) ExchangeContext {
	sum := sha256.Sum256(body)
	return ExchangeContext{holder: "provider", clientID: "connector", recipient: "payer", legType: leg, bodySHA256: hex.EncodeToString(sum[:]), policy: NewConformancePolicy(EnforcementNone)}
}

func TestBoundaryPreparationPreparedCRDRemainsOpaque(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			body := []byte("{bad")
			ex := boundaryContext(body, "crd-order-select")
			ex.policy = NewConformancePolicy(level)
			ex.boundary = []BoundaryCompletion{{"E-01", "1"}}
			var events []ObserverEvent
			g := &Gateway{cfg: Config{Clock: fixedClock, Observer: func(e ObserverEvent) { events = append(events, e) }}}
			p := relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), "application/json; charset=utf-8")
			after, got, err := g.prepareBoundary(context.Background(), ex, p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(relay.BytesForTest(got), body) || got.Ownership() != relay.OwnershipRelayed || got.ContentType() != p.ContentType() || after.bodySHA256 != ex.bodySHA256 {
				t.Fatalf("prepared source changed: %v", got)
			}
			observationFlush(t, g)
			if len(events) != 1 {
				t.Fatalf("events=%d", len(events))
			}
			var detail map[string]string
			if json.Unmarshal([]byte(events[0].Detail), &detail) != nil || detail["preparer"] != "connector" || detail["source"] != "connector" || detail["id"] != "E-01" {
				t.Fatalf("event=%+v", events[0])
			}
			if len(events[0].Payload) != 0 || strings.Contains(events[0].Detail, string(body)) {
				t.Fatal("preparation event disclosed body")
			}
		})
	}
}

func TestBoundaryPreparationDoesNotComparePatients(t *testing.T) {
	body := []byte(`{ "fhirServer":"https://source.invalid", "fhirAuthorization":{"access_token":"secret"}, "context":{"patientId":"a","draftOrders":{"entry":[{"resource":{"subject":{"reference":"Patient/b"}}}]}}, "opaque":[ 1.00, "keep" ] }`)
	ex := boundaryContext(body, "crd-order-select")
	var events []ObserverEvent
	g := &Gateway{cfg: Config{Clock: fixedClock, Observer: func(e ObserverEvent) { events = append(events, e) }}}
	p := relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), "application/json; profile=\"urn:custom\"")
	after, got, err := g.prepareBoundary(context.Background(), ex, p)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{ "context":{"patientId":"a","draftOrders":{"entry":[{"resource":{"subject":{"reference":"Patient/b"}}}]}}, "opaque":[ 1.00, "keep" ] }`)
	if !bytes.Equal(relay.BytesForTest(got), want) {
		t.Fatalf("got %s\nwant %s", relay.BytesForTest(got), want)
	}
	if got.ContentType() != p.ContentType() || !slices.Equal(got.Edits(), []relay.EditID{relay.EditCDSCallbackStrip}) {
		t.Fatalf("proof/media changed: %v", got)
	}
	sum := sha256.Sum256(want)
	if after.bodySHA256 != hex.EncodeToString(sum[:]) || after.bodySHA256 == ex.bodySHA256 || len(after.boundary) != 0 {
		t.Fatalf("edited bytes retain source evidence: %+v", after)
	}
	observationFlush(t, g)
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	var detail map[string]string
	if json.Unmarshal([]byte(events[0].Detail), &detail) != nil || detail["source"] != "gateway" || detail["preparer"] != ex.holder || detail["version"] != "1" || len(events[0].Payload) != 0 {
		t.Fatalf("gateway preparation metadata: %+v", events[0])
	}
	if _, err := relay.Transmit(got, relay.Check(requestKey(ex.legType, true))); err != nil {
		t.Fatal(err)
	}
}

func TestBoundaryPreparationRejectsUnavailableEdit(t *testing.T) {
	for _, body := range []string{`{bad`, `[]`, `null`, `{"fhirServer":"a","fhirServer":"b"}`} {
		t.Run(body, func(t *testing.T) {
			ex := boundaryContext([]byte(body), "crd-order-select")
			_, p, err := (&Gateway{}).prepareBoundary(context.Background(), ex, relay.Exact(relay.NewBody([]byte(body), relay.OriginIngressRequest), "application/json"))
			contextFailure(t, err, http.StatusServiceUnavailable, "adaptation_unavailable")
			if p.Ownership() != 0 {
				t.Fatal("refusal returned payload")
			}
		})
	}
}

func TestBoundaryPreparationDoesNotAssembleSourceData(t *testing.T) {
	for _, row := range []struct{ leg, body string }{
		{"crd-order-select", `{"context":{"patientId":"a"}}`},
		{"crd-order-dispatch", `{}`},
		{"dtr-questionnaire-fetch", `{bad`},
		{"pas-claim", `{bad`},
	} {
		t.Run(row.leg, func(t *testing.T) {
			ex := boundaryContext([]byte(row.body), row.leg)
			g := &Gateway{cfg: Config{AcceptUnknownMembers: true}}
			p := relay.Exact(relay.NewBody([]byte(row.body), relay.OriginIngressRequest), "application/fhir+json; charset=utf-8")
			after, got, err := g.prepareBoundary(context.Background(), ex, p)
			if err != nil || !bytes.Equal(relay.BytesForTest(got), []byte(row.body)) || got.Ownership() != relay.OwnershipRelayed || got.ContentType() != p.ContentType() || after.bodySHA256 != ex.bodySHA256 {
				t.Fatalf("native preparation: %v %v", got, err)
			}
		})
	}
}

func TestBoundaryPreparationDigestMismatch(t *testing.T) {
	ex := boundaryContext([]byte(`{}`), "crd-order-select")
	ex.boundary = []BoundaryCompletion{{"E-01", "1"}}
	_, p, err := (&Gateway{}).prepareBoundary(context.Background(), ex, relay.Exact(relay.NewBody([]byte(`{bad`), relay.OriginIngressRequest), "application/json"))
	contextFailure(t, err, http.StatusForbidden, "boundary_evidence_invalid")
	if p.Ownership() != 0 {
		t.Fatal("refusal returned payload")
	}
}

func TestBoundaryPreparationSignedEditRefused(t *testing.T) {
	body := []byte(`{"fhirServer":"https://source.invalid","resourceType":"Bundle","signature":{"data":"signed"}}`)
	ex := boundaryContext(body, "crd-order-select")
	_, p, err := (&Gateway{}).prepareBoundary(context.Background(), ex, relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), "application/json"))
	contextFailure(t, err, http.StatusUnprocessableEntity, "adaptation_unavailable")
	if !errors.Is(err, relay.ErrSignedContent) || p.Ownership() != 0 {
		t.Fatalf("signed edit=%v payload=%v", err, p)
	}
}

func TestBoundaryPreparationPreservesExistingProof(t *testing.T) {
	for _, callbacks := range []bool{false, true} {
		name := "no new edit"
		raw := []byte(`{"prefetch":{}}`)
		if callbacks {
			name = "new edit would lose proof"
			raw = []byte(`{"fhirServer":"source","prefetch":{}}`)
		}
		t.Run(name, func(t *testing.T) {
			body := relay.NewBody(raw, relay.OriginIngressRequest)
			doc, err := relay.Doc(body)
			if err != nil {
				t.Fatal(err)
			}
			prefetch, _ := doc.Member(doc.Root(), "prefetch")
			p, err := relay.Apply(body, "application/json", relay.EditCDSPrefetchObtain, doc.InsertMember(prefetch, "patient", []byte(`null`)))
			if err != nil {
				t.Fatal(err)
			}
			ex := boundaryContext(relay.BytesForTest(p), "crd-order-select")
			after, got, err := (&Gateway{}).prepareBoundary(context.Background(), ex, p)
			if callbacks {
				contextFailure(t, err, http.StatusServiceUnavailable, "adaptation_unavailable")
				if got.Ownership() != 0 {
					t.Fatal("refusal returned payload")
				}
			} else if err != nil || !slices.Equal(got.Edits(), p.Edits()) || !bytes.Equal(relay.BytesForTest(got), relay.BytesForTest(p)) || after.bodySHA256 != ex.bodySHA256 {
				t.Fatalf("lost existing proof: %v %v", got, err)
			}
		})
	}
}

func TestBoundaryPreparationDoesNotTrustPartialEditID(t *testing.T) {
	raw := []byte(`{"fhirServer":"source","fhirAuthorization":{"access_token":"secret"}}`)
	body := relay.NewBody(raw, relay.OriginIngressRequest)
	doc, err := relay.Doc(body)
	if err != nil {
		t.Fatal(err)
	}
	p, err := relay.Apply(body, "application/json", relay.EditCDSCallbackStrip, doc.RemoveMember(doc.Root(), "fhirServer"))
	if err != nil {
		t.Fatal(err)
	}
	ex := boundaryContext(relay.BytesForTest(p), "crd-order-select")
	_, got, err := (&Gateway{}).prepareBoundary(context.Background(), ex, p)
	contextFailure(t, err, http.StatusServiceUnavailable, "adaptation_unavailable")
	if got.Ownership() != 0 {
		t.Fatal("partial E-01 proof admitted remaining credential")
	}
}

func TestBoundaryPreparationCallbackMutationRows(t *testing.T) {
	for _, row := range []struct{ name, body, want string }{
		{"absent", `{ "nested":{"fhirServer":"keep","fhirAuthorization":"keep"} }`, `{ "nested":{"fhirServer":"keep","fhirAuthorization":"keep"} }`},
		{"server only", `{"fhirServer":null,"nested":{"fhirServer":"keep"}}`, `{"nested":{"fhirServer":"keep"}}`},
		{"authorization only", `{"fhirAuthorization":["opaque"],"prefetch":false}`, `{"prefetch":false}`},
		{"both", `{"fhirServer":12,"fhirAuthorization":false,"prefetch":null}`, `{"prefetch":null}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			ex := boundaryContext([]byte(row.body), "crd-order-dispatch")
			p := relay.Exact(relay.NewBody([]byte(row.body), relay.OriginIngressRequest), "application/json")
			_, got, err := (&Gateway{}).prepareBoundary(context.Background(), ex, p)
			if err != nil || !bytes.Equal(relay.BytesForTest(got), []byte(row.want)) {
				t.Fatalf("got=%s err=%v", relay.BytesForTest(got), err)
			}
		})
	}
}

func TestBoundaryPreparationInvalidOwnershipAndMedia(t *testing.T) {
	raw := []byte(`{bad`)
	ex := boundaryContext(raw, "crd-order-select")
	ex.boundary = []BoundaryCompletion{{"E-01", "1"}}
	t.Run("unset ownership", func(t *testing.T) {
		var unset relay.Payload
		_, p, err := (&Gateway{}).prepareBoundary(context.Background(), ex, unset)
		if !errors.Is(err, relay.ErrUnsetOwnership) || p.Ownership() != 0 {
			t.Fatalf("%v %v", p, err)
		}
	})
	t.Run("changed declared media", func(t *testing.T) {
		ex.contentType = "application/json"
		_, p, err := (&Gateway{}).prepareBoundary(context.Background(), ex, relay.Exact(relay.NewBody(raw, relay.OriginIngressRequest), "text/plain"))
		contextFailure(t, err, http.StatusForbidden, "boundary_evidence_invalid")
		if p.Ownership() != 0 {
			t.Fatal("refusal returned payload")
		}
	})
}
