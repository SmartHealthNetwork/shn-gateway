package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestDTRIdentityWrapperScopes(t *testing.T) {
	own := `{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}`
	wrap := func(r string) string {
		return `{"resourceType":"Parameters","parameter":[{"name":"coverage","resource":` + r + `}]}`
	}
	patient := `{"resourceType":"Patient","id":"p","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-COVERED"}]}`
	contained := `{"resourceType":"Coverage","contained":[` + patient + `],"beneficiary":{"reference":"#p"}}`
	bundle := `{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"urn:uuid:p","resource":` + patient + `},{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"urn:uuid:p"}}}]}`
	for _, row := range []struct {
		name, body, leg, op, direction, version string
		want                                    CheckState
	}{
		{"package input", wrap(own), "dtr-questionnaire-fetch", "questionnaire-package", "request", "pa.dtr@2.0", CheckValid},
		{"foreign patient", wrap(strings.ReplaceAll(own, "MBR-COVERED", "MBR-NOTCOVERED")), "dtr-questionnaire-fetch", "questionnaire-package", "request", "pa.dtr@2.0", CheckInvalid},
		{"unresolved patient", wrap(strings.ReplaceAll(own, "MBR-COVERED", "unknown")), "dtr-questionnaire-fetch", "questionnaire-package", "request", "pa.dtr@2.0", CheckUnavailable},
		{"contained", wrap(contained), "dtr-questionnaire-fetch", "questionnaire-package", "request", "pa.dtr@2.0", CheckValid},
		{"bundle scope", strings.Replace(wrap(bundle), `"coverage"`, `"PackageBundle"`, 1), "dtr-questionnaire-fetch", "questionnaire-package", "response", "pa.dtr@2.0", CheckValid},
		{"sibling cannot close", `{"resourceType":"Parameters","parameter":[{"name":"PackageBundle","resource":` + bundle + `},{"name":"PackageBundle","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"urn:uuid:p"}}}]}}]}`, "dtr-questionnaire-fetch", "questionnaire-package", "response", "pa.dtr@2.0", CheckUnavailable},
		{"contained sibling cannot close", strings.Replace(wrap(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":`+contained+`},{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"#p"}}}]}`), `"coverage"`, `"PackageBundle"`, 1), "dtr-questionnaire-fetch", "questionnaire-package", "response", "pa.dtr@2.0", CheckUnavailable},
		{"unknown resource", wrap(`{"resourceType":"Unrecognized","patient":{"reference":"Patient/MBR-COVERED"}}`), "dtr-questionnaire-fetch", "questionnaire-package", "request", "pa.dtr@2.0", CheckUnavailable},
		{"wrong leg", wrap(own), "pas-claim", "questionnaire-package", "request", "pa.pas@2.0", CheckUnavailable},
		{"wrong operation", wrap(own), "dtr-questionnaire-fetch", "unknown", "request", "pa.dtr@2.0", CheckUnavailable},
		{"wrong direction", wrap(own), "dtr-questionnaire-fetch", "questionnaire-package", "unknown", "pa.dtr@2.0", CheckUnavailable},
		{"missing declaration", wrap(own), "dtr-questionnaire-fetch", "questionnaire-package", "request", "", CheckUnavailable},
		{"malformed wrapper", strings.Replace(wrap(own), `"name":"coverage"`, `"name":1`, 1), "dtr-questionnaire-fetch", "questionnaire-package", "request", "pa.dtr@2.0", CheckUnavailable},
		{"nested unsupported wrapper", wrap(wrap(own)), "dtr-questionnaire-fetch", "questionnaire-package", "request", "pa.dtr@2.0", CheckUnavailable},
		{"next question", string(dtrParams(resourceParam("questionnaire-response", nextQuestionQR("MBR-COVERED")))), "dtr-questionnaire-fetch", "next-question", "request", "pa.dtr@2.0", CheckValid},
		{"patient free", `{"resourceType":"Parameters","parameter":[{"name":"PackageBundle","resource":{"resourceType":"Bundle","type":"collection","entry":[]}}]}`, "dtr-questionnaire-fetch", "questionnaire-package", "response", "pa.dtr@2.0", CheckUnavailable},
	} {
		t.Run(row.name, func(t *testing.T) {
			g := &Gateway{cfg: Config{SubjectReferenceResolver: censusSubjectResolver("requester", "payer")}}
			pci, _, _ := newCensusSoR().ResolvePatient("MBR-COVERED")
			in := CheckInput{Exchange: ExchangeContext{holder: "requester", recipient: "payer", subjectPCI: pci, legType: row.leg, operation: row.op, policy: NewConformancePolicy(EnforcementStrict)}, Direction: row.direction, Status: 200, DeclaredVersion: row.version, Body: []byte(row.body)}
			if row.name == "bundle scope" || row.name == "sibling cannot close" || row.name == "contained sibling cannot close" {
				root, _ := deepDocument(in)
				if _, ok := dtrIdentityParameters(in, root); !ok {
					t.Fatal("scope control did not reach a recognized output envelope")
				}
			}
			if got := g.checkSubjectConsistency(context.Background(), in); got.State != row.want {
				t.Fatalf("got %+v want %s", got, row.want)
			}
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
				in.Exchange.policy = NewConformancePolicy(level)
				calls := 0
				rule := ConformanceRule{ID: "patient.consistency", Class: CheckDeep, Applies: func(CheckInput) bool { return true }, Check: func(ctx context.Context, input CheckInput) CheckResult {
					calls++
					return g.checkSubjectConsistency(ctx, input)
				}}
				err := g.enforceRules(context.Background(), in, []ConformanceRule{rule})
				wantCalls := 0
				if level == EnforcementStrict {
					wantCalls = 1
				}
				if calls != wantCalls || (err != nil) != (level == EnforcementStrict && row.want != CheckValid) {
					t.Fatalf("%s calls=%d err=%v", level, calls, err)
				}
			}
		})
	}
}

func TestDTRIdentityWrapperNativeStrictBoundary(t *testing.T) {
	d := newDTRPayer(t)
	body := dtrParams(resourceParam("coverage", dtrCoverage("cov", dtrFrameMember)), questionnaireParam)
	// A backend error requires no patient-bearing response or clinical writes.
	answer := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"business-rule"}]}`)
	var received []byte
	hits := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != packagePath {
			t.Errorf("path=%s", r.URL.Path)
		}
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(422)
		w.Write(answer)
	}))
	defer backend.Close()
	d.g.cfg.Responder = declaredDTRFixtureResponder{NewNativeResponder(backend.Client(), backend.URL, "", nil, nil)}
	got := d.send(t, shnsdk.FrameOperationQuestionnairePackage, body)
	if got.status != 422 || !got.framed || hits != 1 || !bytes.Equal(received, body) || !bytes.Equal(got.body, answer) {
		t.Fatalf("status=%d framed=%v hits=%d body=%s", got.status, got.framed, hits, got.body)
	}
}

// Recognition is independent of the profile validator and of valid child identity.
func TestDTRIdentityPackageOutputEnvelope(t *testing.T) {
	coverage := `{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}`
	bundle := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + coverage + `}]}`
	wrap := func(name, resource string) string {
		return `{"resourceType":"Parameters","parameter":[{"name":"` + name + `","resource":` + resource + `}]}`
	}
	pci, _, _ := newCensusSoR().ResolvePatient("MBR-COVERED")
	g := &Gateway{cfg: Config{SubjectReferenceResolver: censusSubjectResolver("requester", "payer")}}
	for _, contract := range []struct{ line, name string }{{"2.0", "return"}, {"2.0", "PackageBundle"}, {"2.1", "PackageBundle"}, {"2.2", "packagebundle"}} {
		t.Run(contract.line+"/"+contract.name, func(t *testing.T) {
			good := wrap(contract.name, bundle)
			for _, row := range []struct {
				name, body string
				want       CheckState
			}{
				{"recognized", good, CheckValid},
				{"unknown name", wrap("arbitrary", bundle), CheckUnavailable},
				{"coverage name", wrap("coverage", bundle), CheckUnavailable},
				{"wrong line spelling", wrap(map[bool]string{true: "PackageBundle", false: "packagebundle"}[contract.line == "2.2"], bundle), CheckUnavailable},
				{"wrong primary type", wrap(contract.name, coverage), CheckUnavailable},
				{"malformed primary", wrap(contract.name, `[]`), CheckUnavailable},
				{"wrong Bundle type", wrap(contract.name, strings.Replace(bundle, `"collection"`, `"searchset"`, 1)), CheckUnavailable},
				{"malformed parameter", strings.Replace(good, `"name":"`+contract.name+`"`, `"name":3`, 1), CheckUnavailable},
				{"conflicting value", strings.Replace(good, `"resource":`, `"valueString":"ambiguous","resource":`, 1), CheckUnavailable},
				{"foreign child", strings.ReplaceAll(good, "MBR-COVERED", "MBR-NOTCOVERED"), CheckInvalid},
				{"unresolved child", strings.ReplaceAll(good, "MBR-COVERED", "unknown"), CheckUnavailable},
			} {
				t.Run(row.name, func(t *testing.T) {
					in := CheckInput{Exchange: ExchangeContext{holder: "requester", recipient: "payer", subjectPCI: pci, legType: "dtr-questionnaire-fetch", operation: "questionnaire-package", policy: NewConformancePolicy(EnforcementStrict)}, Direction: "response", Status: 200, DeclaredVersion: "pa.dtr@" + contract.line, Body: []byte(row.body)}
					if got := g.checkSubjectConsistency(context.Background(), in); got.State != row.want {
						t.Fatalf("got %+v want %s", got, row.want)
					}
					for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
						in.Exchange.policy = NewConformancePolicy(level)
						calls := 0
						rule := ConformanceRule{ID: "patient.consistency", Class: CheckDeep, Applies: func(CheckInput) bool { return true }, Check: func(context.Context, CheckInput) CheckResult { calls++; return deepUnavailable("identity_unavailable") }}
						if err := g.enforceRules(context.Background(), in, []ConformanceRule{rule}); err != nil || calls != 0 {
							t.Fatalf("native %s err=%v calls=%d", level, err, calls)
						}
					}
				})
			}
		})
	}
}
