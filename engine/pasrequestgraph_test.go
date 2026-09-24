package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestPASRequestEvidenceClosure(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"resourceType":"Patient","id":"p"}},{"fullUrl":"https://example.test/fhir/ServiceRequest/s","resource":{"resourceType":"ServiceRequest","id":"s","subject":{"reference":"Patient/p"},"supportingInfo":[{"reference":"ClinicalImpression/c"}]}}]}`)
	good := []byte(`{"resourceType":"ClinicalImpression","id":"c","subject":{"reference":"Patient/p"},"problem":[{"reference":"Condition/d"}]}`)
	for _, tc := range []struct {
		name     string
		clinical []byte
		found    bool
		wantErr  bool
	}{
		{"selected transitive evidence", good, true, false},
		{"missing", nil, false, true},
		{"wrong patient", []byte(`{"resourceType":"ClinicalImpression","id":"c","subject":{"reference":"Patient/other"}}`), true, true},
		{"wrong identity", []byte(`{"resourceType":"ClinicalImpression","id":"other","subject":{"reference":"Patient/p"}}`), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			got, err := retainPASRequestEvidence(context.Background(), body, func(_ context.Context, ref string) ([]byte, bool, error) {
				calls = append(calls, ref)
				if ref == "ClinicalImpression/c" {
					return tc.clinical, tc.found, nil
				}
				if ref == "Condition/d" {
					return []byte(`{"resourceType":"Condition","id":"d","subject":{"reference":"Patient/p"},"evidence":[{"detail":[{"reference":"ClinicalImpression/c"}]}]}`), true, nil
				}
				return nil, false, fmt.Errorf("unexpected read")
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if err == nil {
				var b struct {
					Entry []json.RawMessage `json:"entry"`
				}
				json.Unmarshal(got, &b)
				if len(b.Entry) != 4 || len(calls) != 2 {
					t.Fatalf("entries=%d reads=%v", len(b.Entry), calls)
				}
			}
		})
	}
}

func TestPASRequestEvidenceRefusals(t *testing.T) {
	makeBody := func(ref string) []byte {
		return []byte(fmt.Sprintf(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"resourceType":"Patient","id":"p"}},{"fullUrl":"https://example.test/fhir/ServiceRequest/s","resource":{"resourceType":"ServiceRequest","id":"s","subject":{"reference":"Patient/p"},"supportingInfo":[{"reference":%q}]}}]}`, ref))
	}
	for _, ref := range []string{"https://other.test/Condition/c", "Condition/c?x=1", "Condition%2Fc", "Condition/c/_history/1", "Organization/o", "Condition/../c"} {
		t.Run(ref, func(t *testing.T) {
			calls := 0
			_, err := retainPASRequestEvidence(context.Background(), makeBody(ref), func(context.Context, string) ([]byte, bool, error) { calls++; return nil, false, nil })
			if err == nil || calls != 0 {
				t.Fatalf("error=%v reads=%d", err, calls)
			}
		})
	}
	t.Run("read failure is sanitized", func(t *testing.T) {
		_, err := retainPASRequestEvidence(context.Background(), makeBody("Condition/c"), func(context.Context, string) ([]byte, bool, error) {
			return nil, false, fmt.Errorf("private backend credentials")
		})
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("resource budget", func(t *testing.T) {
		calls := 0
		_, err := retainPASRequestEvidence(context.Background(), makeBody("Condition/c0"), func(_ context.Context, ref string) ([]byte, bool, error) {
			calls++
			return []byte(fmt.Sprintf(`{"resourceType":"Condition","id":%q,"subject":{"reference":"Patient/p"},"evidence":[{"detail":[{"reference":"Condition/c%d"}]}]}`, strings.TrimPrefix(ref, "Condition/"), calls)), true, nil
		})
		if err == nil || calls != pasGraphMaxResources-2 {
			t.Fatalf("error=%v reads=%d", err, calls)
		}
	})
	t.Run("byte budget", func(t *testing.T) {
		_, err := retainPASRequestEvidence(context.Background(), makeBody("Condition/c"), func(context.Context, string) ([]byte, bool, error) {
			return []byte(strings.Repeat(" ", pasGraphMaxBytes)), true, nil
		})
		if err == nil {
			t.Fatal("accepted oversized evidence")
		}
	})
	t.Run("reference budget", func(t *testing.T) {
		var b map[string]any
		json.Unmarshal(makeBody("Condition/c"), &b)
		sr := b["entry"].([]any)[1].(map[string]any)["resource"].(map[string]any)
		refs := make([]any, pasGraphMaxReferences+1)
		for i := range refs {
			refs[i] = map[string]any{"reference": "Patient/p"}
		}
		sr["supportingInfo"] = refs
		raw, _ := json.Marshal(b)
		_, err := retainPASRequestEvidence(context.Background(), raw, func(context.Context, string) ([]byte, bool, error) {
			t.Fatal("unexpected read")
			return nil, false, nil
		})
		if err == nil {
			t.Fatal("accepted reference overflow")
		}
	})
	t.Run("depth budget", func(t *testing.T) {
		raw := []byte(`{"resourceType":"Bundle","nested":` + strings.Repeat(`[`, pasGraphMaxDepth+1) + `0` + strings.Repeat(`]`, pasGraphMaxDepth+1) + `}`)
		_, err := retainPASRequestEvidence(context.Background(), raw, nil)
		if err == nil {
			t.Fatal("accepted depth overflow")
		}
	})
}

func TestPASRequestRejectsOtherPatientContainedEvidence(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://example.test/fhir/Claim/q","resource":{"resourceType":"Claim","id":"q","patient":{"reference":"Patient/p"}}},{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"resourceType":"Patient","id":"p"}},{"fullUrl":"https://example.test/fhir/Coverage/v","resource":{"resourceType":"Coverage","id":"v","beneficiary":{"reference":"Patient/p"}}},{"fullUrl":"https://example.test/fhir/ServiceRequest/s","resource":{"resourceType":"ServiceRequest","id":"s","subject":{"reference":"Patient/p"},"supportingInfo":[{"reference":"Condition/c"}]}}]}`)
	evidence := []byte(`{"resourceType":"Condition","id":"c","subject":{"reference":"Patient/p"},"contained":[{"resourceType":"Patient","id":"other"},{"resourceType":"Observation","id":"o","status":"final","code":{"text":"Other patient's result"},"subject":{"reference":"#other"},"valueString":"Other patient's clinical data"}],"evidence":[{"detail":[{"reference":"#o"}]}]}`)
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%v", foreign), func(t *testing.T) {
			selected := evidence
			if !foreign {
				var resource map[string]any
				if err := json.Unmarshal(evidence, &resource); err != nil {
					t.Fatal(err)
				}
				observation := resource["contained"].([]any)[1].(map[string]any)
				observation["subject"] = map[string]any{"reference": "Patient/p"}
				resource["contained"] = []any{observation}
				selected, _ = json.Marshal(resource)
			}
			got, err := retainPASRequestEvidence(context.Background(), body, func(context.Context, string) ([]byte, bool, error) { return selected, true, nil })
			if (err != nil) != foreign {
				_, status, msg := parseConformantPASSubjects(got)
				t.Fatalf("foreign=%v error=%v; subsequent request guard status=%d msg=%q", foreign, err, status, msg)
			}
		})
	}
}

func TestAuthoredPASOptionalEvidenceScope(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, orderType := range []string{"ServiceRequest", "DeviceRequest"} {
			for _, optionalType := range []string{"ServiceRequest", "DeviceRequest"} {
				for _, absolute := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/%t", line, orderType, optionalType, absolute), func(t *testing.T) {
						in := attachmentInputs(orderType, line, absolute)
						in.SR = bytes.ReplaceAll(in.SR, []byte("Patient/MBR-PD-UC04"), []byte(in.PatientRef))
						qc := contextInputs()
						qc.PatientRef = in.PatientRef
						qc.CoverageRef = in.CoverageRef
						qc.OrderRef = orderType + "/source-order"
						optional := `{"extension":[{"url":"https://example.test/precise","valueDecimal":9007199254740993.2300}],"reference":"` + optionalType + `/optional-order","type":"` + optionalType + `"}`
						if absolute {
							optional = `{"type":"` + optionalType + `","reference":"` + optionalType + `/optional-order","extension":[{"valueDecimal":9007199254740993.2300,"url":"https://example.test/precise"}]}`
						}
						raw := appendAuthoredTestContext(bytes.ReplaceAll([]byte(contextQR), []byte("Patient/synthetic"), []byte(in.PatientRef)), optional)
						var err error
						in.QR, err = composeDTRContextAtLine(raw, line, qc)
						if err != nil {
							t.Fatal(err)
						}
						check := func(body []byte, err error) {
							t.Helper()
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Contains(body, []byte(optional)) {
								t.Fatal("raw authored assembly changed optional Reference bytes")
							}
							if orderType == "DeviceRequest" {
								supplier, e := buildHomeOxygenSupplier("org-dme-ox")
								if e != nil {
									t.Fatal(e)
								}
								body, e = retainPASSubmitSupplier(body, supplier)
								if e != nil {
									t.Fatal(e)
								}
							}
							policy, err := authoredPASReferencePolicyFor(body, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs)
							if err != nil {
								t.Fatal("source verification", err)
							}
							calls := 0
							read := func(context.Context, string) ([]byte, bool, error) {
								calls++
								return nil, false, fmt.Errorf("unexpected read")
							}
							got, err := retainPASRequestEvidenceWithPolicy(context.Background(), body, read, policy)
							if err != nil {
								t.Fatal("authored closure", err)
							}
							if calls != 0 || !optionalReferenceEquivalent(t, got, []byte(optional)) {
								t.Fatal("optional context fetched or changed")
							}
							graph := optionalRequestGraph(t, got)
							if err := graph.validateWithReferencePolicy(policy); err != nil {
								t.Fatal("final graph rejected verified occurrence", err)
							}
							if err := optionalRequestGraph(t, got).validate(); err == nil {
								t.Fatal("generic final graph accepted unresolved optional reference")
							}
							// A retained policy cannot authorize a different QR, even when its references match.
							var changed map[string]any
							decodePASObject(body, &changed)
							for _, entry := range changed["entry"].([]any) {
								r := entry.(map[string]any)["resource"].(map[string]any)
								if r["resourceType"] == "QuestionnaireResponse" {
									r["status"] = "amended"
								}
							}
							changedBody, _ := json.Marshal(changed)
							if _, err := retainPASRequestEvidenceWithPolicy(context.Background(), changedBody, read, policy); err == nil {
								t.Fatal("policy accepted changed non-reference QR member")
							}
							if _, err := retainPASRequestEvidence(context.Background(), body, read); err == nil {
								t.Fatal("generic completion accepted unresolved context")
							}
							// The same literal outside the authorized occurrence remains a required link.
							var b map[string]any
							decodePASObject(body, &b)
							for _, entry := range b["entry"].([]any) {
								r := entry.(map[string]any)["resource"].(map[string]any)
								if r["resourceType"] == "QuestionnaireResponse" {
									r["other"] = map[string]any{"reference": optionalType + "/optional-order"}
								}
							}
							bad, _ := json.Marshal(b)
							if _, err := retainPASRequestEvidenceWithPolicy(context.Background(), bad, read, policy); err == nil {
								t.Fatal("same literal outside optional occurrence accepted")
							}
							if _, err := authoredPASReferencePolicyFor(bad, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs); err == nil {
								t.Fatal("post-build source mutation accepted")
							}
						}
						check(buildAuthoredPASSubmit(line, in))
						if orderType == "ServiceRequest" {
							for _, diagnostic := range []bool{false, true} {
								u := attachmentUpdateInputs(line, absolute, diagnostic)
								u.QR = in.QR
								u.SR = in.SR
								check(buildAuthoredPASUpdate(line, u))
							}
						}
					})
				}
			}
		}
	}
}

func optionalReferenceEquivalent(t *testing.T, body, expected []byte) bool {
	t.Helper()
	var want map[string]any
	if decodePASObject(expected, &want) != nil {
		t.Fatal("invalid expected Reference")
	}
	graph := optionalRequestGraph(t, body)
	for _, entry := range graph.byURL {
		if entry.resource["resourceType"] != "QuestionnaireResponse" {
			continue
		}
		for _, ext := range entry.resource["extension"].([]any) {
			fields := ext.(map[string]any)
			node, ok := fields["valueReference"].(map[string]any)
			if ok && reflect.DeepEqual(node, want) {
				return true
			}
		}
	}
	return false
}
func optionalRequestGraph(t *testing.T, body []byte) *pasGraph {
	t.Helper()
	var bundle map[string]any
	if decodePASObject(body, &bundle) != nil {
		t.Fatal("invalid graph fixture")
	}
	graph := &pasGraph{bundle: bundle, entries: bundle["entry"].([]any), byURL: map[string]*pasGraphEntry{}}
	for _, raw := range graph.entries {
		entry := raw.(map[string]any)
		full := entry["fullUrl"].(string)
		graph.byURL[full] = &pasGraphEntry{fullURL: full, resource: entry["resource"].(map[string]any)}
	}
	return graph
}

func TestAuthoredPASReferenceOccurrenceRefusals(t *testing.T) {
	in := attachmentInputs("ServiceRequest", "2.2", false)
	in.SR = bytes.ReplaceAll(in.SR, []byte("Patient/MBR-PD-UC04"), []byte(in.PatientRef))
	qc := contextInputs()
	qc.PatientRef = in.PatientRef
	qc.CoverageRef = in.CoverageRef
	qc.OrderRef = "ServiceRequest/source-order"
	raw := appendAuthoredTestContext(bytes.ReplaceAll([]byte(contextQR), []byte("Patient/synthetic"), []byte(in.PatientRef)), `{"reference":"DeviceRequest/optional-order","type":"DeviceRequest"}`)
	var err error
	in.QR, err = composeDTRContextAtLine(raw, "2.2", qc)
	if err != nil {
		t.Fatal(err)
	}
	body, err := buildAuthoredPASSubmit("2.2", in)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := authoredPASReferencePolicyFor(body, in.QR, in.SR, in.CoverageRef, false)
	if err != nil {
		t.Fatal(err)
	}
	graph := optionalRequestGraph(t, body)
	owner := graph.byURL[policy.owner]
	for path, encoded := range policy.references {
		var node map[string]any
		decodePASObject([]byte(encoded), &node)
		if !policy.allows(owner, path, node) {
			t.Fatal("verified occurrence refused")
		}
		for name, test := range map[string]struct {
			owner *pasGraphEntry
			path  string
			node  map[string]any
		}{
			"owner absent": {nil, path, node}, "different resource": {&pasGraphEntry{fullURL: "https://example.test/fhir/QuestionnaireResponse/other"}, path, node},
			"other extension": {owner, "/extension/999/valueReference", node}, "nested occurrence": {owner, path + "/extra", node}, "escaped member": {owner, strings.Replace(path, "/valueReference", "~1valueReference", 1), node},
			"same reference different extras": {owner, path, map[string]any{"reference": "DeviceRequest/optional-order", "type": "DeviceRequest", "display": "added"}},
		} {
			t.Run(name, func(t *testing.T) {
				if policy.allows(test.owner, test.path, test.node) {
					t.Fatal("unauthorized occurrence accepted")
				}
			})
		}
	}
	for _, field := range []string{"status", "id", "extension"} {
		t.Run("changed QR "+field, func(t *testing.T) {
			changed := optionalRequestGraph(t, body)
			qr := changed.byURL[policy.owner].resource
			switch field {
			case "status":
				qr[field] = "amended"
			case "id":
				qr[field] = "other"
			case "extension":
				ext := qr[field].([]any)
				ext[0], ext[len(ext)-1] = ext[len(ext)-1], ext[0]
			}
			if changed.validateWithReferencePolicy(policy) == nil {
				t.Fatal("final graph accepted stale descriptor")
			}
			mutated, _ := json.Marshal(changed.bundle)
			reads := 0
			if _, err := retainPASRequestEvidenceWithPolicy(context.Background(), mutated, func(context.Context, string) ([]byte, bool, error) { reads++; return nil, false, nil }, policy); err == nil || reads != 0 {
				t.Fatalf("error=%v reads=%d", err, reads)
			}
		})
	}
	missing := optionalRequestGraph(t, body)
	delete(missing.byURL, policy.owner)
	if missing.validateWithReferencePolicy(policy) == nil {
		t.Fatal("policy accepted missing QR owner")
	}
}

func TestAuthoredPASOptionalContextKeepsNestedReferencesStrict(t *testing.T) {
	for _, kind := range []string{"ServiceRequest", "DeviceRequest"} {
		t.Run(kind, func(t *testing.T) {
			in := attachmentInputs("ServiceRequest", "2.2", false)
			in.SR = bytes.ReplaceAll(in.SR, []byte("Patient/MBR-PD-UC04"), []byte(in.PatientRef))
			qc := contextInputs()
			qc.PatientRef = in.PatientRef
			qc.CoverageRef = in.CoverageRef
			qc.OrderRef = "ServiceRequest/source-order"
			raw := appendAuthoredTestContext(bytes.ReplaceAll([]byte(contextQR), []byte("Patient/synthetic"), []byte(in.PatientRef)), `{"reference":"`+kind+`/optional-order","type":"`+kind+`","extension":[{"url":"https://example.test/nested","valueReference":{"reference":"`+kind+`/optional-order"}}]}`)
			var err error
			in.QR, err = composeDTRContextAtLine(raw, "2.2", qc)
			if err != nil {
				t.Fatal(err)
			}
			body, err := buildAuthoredPASSubmit("2.2", in)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := authoredPASReferencePolicyFor(body, in.QR, in.SR, in.CoverageRef, false)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			if _, err := retainPASRequestEvidenceWithPolicy(context.Background(), body, func(context.Context, string) ([]byte, bool, error) { calls++; return nil, false, nil }, policy); err == nil || calls != 0 {
				t.Fatalf("nested order reference bypassed clinical-only lookup: %v reads%d", err, calls)
			}
			if optionalRequestGraph(t, body).validateWithReferencePolicy(policy) == nil {
				t.Fatal("final graph skipped nested Reference")
			}
		})
	}
}
func TestAuthoredPASOptionalContextNeedsActiveIdentities(t *testing.T) {
	for _, missing := range []string{"ServiceRequest/source-order", "Coverage/cov-MBR-OX"} {
		t.Run(missing, func(t *testing.T) {
			in := attachmentInputs("ServiceRequest", "2.2", false)
			in.SR = bytes.ReplaceAll(in.SR, []byte("Patient/MBR-PD-UC04"), []byte(in.PatientRef))
			qc := contextInputs()
			qc.PatientRef = in.PatientRef
			qc.CoverageRef = in.CoverageRef
			qc.OrderRef = "ServiceRequest/source-order"
			raw := appendAuthoredTestContext(bytes.ReplaceAll([]byte(contextQR), []byte("Patient/synthetic"), []byte(in.PatientRef)), `{"reference":"DeviceRequest/optional-order","type":"DeviceRequest"}`)
			var err error
			in.QR, err = composeDTRContextAtLine(raw, "2.2", qc)
			if err != nil {
				t.Fatal(err)
			}
			var qr map[string]json.RawMessage
			json.Unmarshal(in.QR, &qr)
			var ext []json.RawMessage
			json.Unmarshal(qr["extension"], &ext)
			target := missing
			if strings.HasPrefix(missing, "Coverage/") {
				target = in.CoverageRef
			}
			kept := ext[:0]
			removed := 0
			for _, e := range ext {
				if bytes.Contains(e, []byte(`"reference":"`+target+`"`)) {
					removed++
					continue
				}
				kept = append(kept, e)
			}
			if removed != 1 {
				t.Fatalf("removed%d", removed)
			}
			qr["extension"], _ = json.Marshal(kept)
			in.QR, _ = json.Marshal(qr)
			body, err := buildAuthoredPASSubmit("2.2", in)
			if err != nil {
				t.Fatal("assembly fixture", err)
			}
			if _, err := authoredPASReferencePolicyFor(body, in.QR, in.SR, in.CoverageRef, false); err == nil {
				t.Fatal("optional policy accepted missing active administrative identity")
			}
		})
	}
}

func TestAuthoredPASOptionalPolicyRejectsDotSegments(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, kind := range []string{"ServiceRequest", "DeviceRequest"} {
			for _, id := range []string{".", "..", ".clinical", "..clinical", "a..b", "a-b"} {
				t.Run(line+"/"+kind+"/"+id, func(t *testing.T) {
					in := attachmentInputs("ServiceRequest", line, false)
					in.SR = bytes.ReplaceAll(in.SR, []byte("Patient/MBR-PD-UC04"), []byte(in.PatientRef))
					qc := contextInputs()
					qc.PatientRef = in.PatientRef
					qc.CoverageRef = in.CoverageRef
					qc.OrderRef = "ServiceRequest/source-order"
					var err error
					in.QR, err = composeDTRContextAtLine(bytes.ReplaceAll([]byte(contextQR), []byte("Patient/synthetic"), []byte(in.PatientRef)), line, qc)
					if err != nil {
						t.Fatal(err)
					}
					// Add the tested source context after composition so the policy is independently exercised.
					in.QR = appendAuthoredTestContext(in.QR, `{"reference":"`+kind+`/`+id+`","type":"`+kind+`"}`)
					body, err := buildAuthoredPASSubmit(line, in)
					if err != nil {
						t.Fatal("assembly fixture", err)
					}
					reads := 0
					policy, policyErr := authoredPASReferencePolicyFor(body, in.QR, in.SR, in.CoverageRef, false)
					reject := id == "." || id == ".."
					if policyErr != nil {
						if !reject || policyErr.Error() != "invalid optional order identity" {
							t.Fatal("unexpected policy refusal", policyErr)
						}
						return
					}
					_, err = retainPASRequestEvidenceWithPolicy(context.Background(), body, func(context.Context, string) ([]byte, bool, error) { reads++; return nil, false, nil }, policy)
					if reject {
						t.Fatalf("dot-segment policy admitted %s/%s: completion error=%v reads=%d", kind, id, err, reads)
					}
					if err != nil || reads != 0 {
						t.Fatalf("valid dotted ID error=%v reads=%d", err, reads)
					}
				})
			}
		}
	}
}
