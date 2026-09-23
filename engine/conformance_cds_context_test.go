package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// PCV-01/02/03/04: signed routing remains usable when the content omits
// patientId. A case change is the same missing canonical member; extra keys
// beside a valid canonical member are retained as participant bytes.
func TestCDSRequestContextSignedPairPolicy(t *testing.T) {
	base := conformantCRDRequest("MBR-COVERED")
	base = bytes.Replace(base, []byte(`"fhirServer":"https://provider.example/fhir",`), nil, 1)
	base = bytes.Replace(base, []byte(`"fhirAuthorization":{"token_type":"Bearer","access_token":"tok"},`), nil, 1)
	if bytes.Contains(base, []byte(`"fhirServer"`)) || bytes.Contains(base, []byte(`"fhirAuthorization"`)) {
		t.Fatal("request still needs a boundary edit")
	}
	for _, tc := range []struct{ name, from, to string }{
		{"absent", `"patientId":"MBR-COVERED",`, ""},
		{"wrong-case", `"patientId"`, `"PatientId"`},
		{"wrong-type", `"patientId":"MBR-COVERED"`, `"patientId":17`},
		{"contradictory-body-hook", `"hook":"order-select"`, `"hook":"order-sign"`},
		{"contradictory-dispatch-hook", `"hook":"order-select"`, `"hook":"order-dispatch"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := bytes.Replace(base, []byte(tc.from), []byte(tc.to), 1)
			if bytes.Equal(bad, base) || !json.Valid(bad) {
				t.Fatal("mutation did not produce one valid-JSON change")
			}
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
				t.Run(level.String(), func(t *testing.T) {
					env := newTransportExchangeWithPolicy(t, level)
					response := []byte(`{"cards":[]}`)
					env.payerReturns(framedCRDReply(t, http.StatusOK, "application/json", response))
					rec := ingressAt(t, env, "shn-order-select", bad)
					if level == EnforcementStrict {
						if rec.Code != http.StatusUnprocessableEntity || !bytes.Contains(rec.Body.Bytes(), []byte(`"rule":"cds.request.context"`)) || env.routeHitCount() != 0 {
							t.Fatalf("strict refusal %d %s, route hits %d", rec.Code, rec.Body.String(), env.routeHitCount())
						}
						return
					}
					if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), response) || !bytes.Equal(sentRequest(t, env), bad) {
						t.Fatalf("%s changed carried request or answer: %d %s", level, rec.Code, rec.Body.String())
					}
					observationFlush(t, env.originator)
					findings, _ := env.originator.ConformanceObservationsForTest()
					found := false
					for _, f := range findings {
						if f.Rule == "cds.request.context" && f.State == CheckInvalid && f.Action == "not_enforced" {
							found = true
						}
					}
					if level == EnforcementNone && len(findings) != 0 || level != EnforcementNone && !found {
						t.Fatalf("%s context observation: %+v", level, findings)
					}
				})
			}
		})
	}
}

// PCV-04: each supported hook has a valid control before one required
// context shape is removed or changed. The authenticated route is independent
// of these bytes; no patient linkage is inferred from patientId.
func TestStrictCDSRequestContextPublishedShapes(t *testing.T) {
	rows := []struct {
		name, leg, hook, good, bad string
	}{
		{"select patient absent", "crd-order-select", "order-select", `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`, `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`},
		{"select wrong case", "crd-order-select", "order-select", `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`, `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","PatientId":"p","selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`},
		{"select patient type", "crd-order-select", "order-select", `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`, `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":3,"selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`},
		{"select user", "crd-order-select", "order-select", `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`, `{"hook":"order-select","hookInstance":"i","context":{"patientId":"p","selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`},
		{"select selections", "crd-order-select", "order-select", `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`, `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","selections":[1],"draftOrders":{"resourceType":"Bundle"}}}`},
		{"select draftOrders", "crd-order-select", "order-select", `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","selections":["ServiceRequest/s"],"draftOrders":{"resourceType":"Bundle"}}}`, `{"hook":"order-select","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","selections":["ServiceRequest/s"],"draftOrders":[]}}`},
		{"sign draftOrders", "crd-order-select", "order-sign", `{"hook":"order-sign","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p","draftOrders":{"resourceType":"Bundle"}}}`, `{"hook":"order-sign","hookInstance":"i","context":{"userId":"Practitioner/p","patientId":"p"}}`},
		{"dispatch patient", "crd-order-dispatch", "order-dispatch", `{"hook":"order-dispatch","hookInstance":"i","context":{"patientId":"p","dispatchedOrders":["DeviceRequest/d"],"performer":"Organization/o"}}`, `{"hook":"order-dispatch","hookInstance":"i","context":{"dispatchedOrders":["DeviceRequest/d"],"performer":"Organization/o"}}`},
		{"dispatch orders", "crd-order-dispatch", "order-dispatch", `{"hook":"order-dispatch","hookInstance":"i","context":{"patientId":"p","dispatchedOrders":["DeviceRequest/d"],"performer":"Organization/o"}}`, `{"hook":"order-dispatch","hookInstance":"i","context":{"patientId":"p","dispatchedOrders":{},"performer":"Organization/o"}}`},
		{"dispatch performer", "crd-order-dispatch", "order-dispatch", `{"hook":"order-dispatch","hookInstance":"i","context":{"patientId":"p","dispatchedOrders":["DeviceRequest/d"],"performer":"Organization/o"}}`, `{"hook":"order-dispatch","hookInstance":"i","context":{"patientId":"p","dispatchedOrders":["DeviceRequest/d"]}}`},
	}
	g := &Gateway{cfg: Config{HolderID: "provider"}}
	r := deepRule(t, g, "cds.request.context")
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			in := CheckInput{Exchange: ExchangeContext{legType: row.leg, crdHook: row.hook, policy: NewConformancePolicy(EnforcementStrict)}, Direction: "request", Status: 200, DeclaredVersion: "pa.crd@2.0", Body: []byte(row.good)}
			if !r.Applies(in) || r.Class != CheckDeep {
				t.Fatal("supported request did not select deep rule")
			}
			if got := r.Check(context.Background(), in); got.State != CheckValid {
				t.Fatalf("valid control: %+v", got)
			}
			in.Body = []byte(row.bad)
			if got := r.Check(context.Background(), in); got.State != CheckInvalid || got.Code != r.ID {
				t.Fatalf("mutation: %+v", got)
			}
			wantStructuralError(t, g.enforceRules(context.Background(), in, []ConformanceRule{r}), http.StatusUnprocessableEntity, r.ID)
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic} {
				in.Exchange.policy = NewConformancePolicy(level)
				if err := g.enforceRules(context.Background(), in, []ConformanceRule{r}); err != nil {
					t.Fatalf("%s blocked context: %v", level, err)
				}
			}
		})
	}
}

func TestStrictCDSRequestContextApplicabilityAndAvailability(t *testing.T) {
	r := deepRule(t, &Gateway{}, "cds.request.context")
	good := `{"hook":"order-select","hookInstance":"i","context":{"patientId":"p","userId":"Practitioner/p","selections":[],"draftOrders":{"resourceType":"Bundle"},"PatientId":"extra"}}`
	in := CheckInput{Exchange: ExchangeContext{legType: "crd-order-select", crdHook: "order-select"}, Direction: "request", DeclaredVersion: "pa.crd@2.1", Body: []byte(good)}
	if got := r.Check(context.Background(), in); got.State != CheckValid {
		t.Fatalf("extra differently cased key beside canonical key: %+v", got)
	}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		in.DeclaredVersion = "pa.crd@" + line
		if got := r.Check(context.Background(), in); got.State != CheckValid {
			t.Fatalf("supported line %s: %+v", line, got)
		}
	}
	for _, version := range []string{"", "pa.crd@9.9", "pa.pas@2.0"} {
		in.DeclaredVersion = version
		if got := r.Check(context.Background(), in); got.State != CheckUnavailable {
			t.Fatalf("unknown line %q: %+v", version, got)
		}
	}
	in.DeclaredVersion = "pa.crd@2.0"
	in.Exchange.crdHook = ""
	in.Body = []byte(strings.Replace(good, `"order-select"`, `"unknown-hook"`, 1))
	if got := r.Check(context.Background(), in); got.State != CheckUnavailable {
		t.Fatalf("unsupported hook: %+v", got)
	}
	in.Exchange.crdHook = "order-select"
	if got := r.Check(context.Background(), in); got.State != CheckUnavailable {
		t.Fatalf("unsupported body hook under signed routing: %+v", got)
	}
	in.Body = []byte(strings.Replace(good, `"order-select"`, `"order-sign"`, 1))
	if got := r.Check(context.Background(), in); got.State != CheckInvalid {
		t.Fatalf("contradictory supported body hook: %+v", got)
	}
	in.Body = []byte("{bad")
	if got := r.Check(context.Background(), in); got.State != CheckUnavailable {
		t.Fatalf("unreadable envelope: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := r.Check(ctx, in); got.State != CheckUnavailable {
		t.Fatalf("canceled checker: %+v", got)
	}
	in.Direction = "response"
	if r.Applies(in) {
		t.Fatal("request rule applied to response")
	}
}
