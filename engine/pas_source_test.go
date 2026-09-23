package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type misScopedPASSoR struct {
	*censusSoR
	order, claim []byte
	patientRef   string
}

func (s misScopedPASSoR) OpenOrder(string) ([]byte, bool) { return s.order, true }

func (s misScopedPASSoR) PatientFHIRRef(member string) (string, bool) {
	if s.patientRef != "" {
		return s.patientRef, true
	}
	return s.censusSoR.PatientFHIRRef(member)
}

func (s misScopedPASSoR) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	if resourceType != "Claim" {
		return s.censusSoR.SearchPatientContext(ctx, resourceType, id, dates...)
	}
	page := append([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"https://census.invalid/fhir/Claim/wrong-patient","resource":`), s.claim...)
	page = append(page, []byte(`,"search":{"mode":"match"}}]}`)...)
	parsed, err := ParseSearchPage(page, "Claim")
	if err != nil {
		return SearchResult{}, err
	}
	return SearchResult{Pages: [][]byte{page}, Entries: parsed.Entries, Total: len(parsed.Entries)}, nil
}

// PCV-14: the participant's held draft Claim is the source of 2.1+ PAS
// authoring values, including priority. Search order and missing values never
// turn into builder defaults.
func TestPASFactsFromParticipantSource(t *testing.T) {
	sor := newCensusSoR()
	order, ok := sor.OpenOrder("MBR-COVERED")
	if !ok {
		t.Fatal("synthetic participant order absent")
	}
	claim, err := syntheticPASDraftClaimForOrder(order)
	if err != nil {
		t.Fatal(err)
	}
	orderType, orderID, err := resourceTypeAndID(order)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := (&Gateway{cfg: Config{SoR: sor}}).pasFactsFromSource(t.Context(), "MBR-COVERED", order, order)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(facts.Priority, []byte(`"code":"normal"`)) || !bytes.Contains(facts.CertificationType, []byte(`"code":"I"`)) {
		t.Fatalf("participant facts lost: %+v", facts)
	}
	changedOrder := []byte(strings.Replace(string(order), orderID, "other-order", 1))
	if _, err := (&Gateway{cfg: Config{SoR: sor}}).pasFactsFromSource(t.Context(), "MBR-COVERED", order, changedOrder); err == nil {
		t.Fatal("Claim facts attached to an order other than the participant's held order")
	}
	if _, err := (&Gateway{cfg: Config{SoR: sor}}).pasFactsFromSource(t.Context(), "MBR-COVERED", nil, order); err == nil {
		t.Fatal("PAS source accepted without the captured held order")
	}
	for _, row := range []struct {
		name   string
		claims [][]byte
	}{
		{"no Claim", nil},
		{"another order", [][]byte{[]byte(strings.Replace(string(claim), orderType+"/"+orderID, orderType+"/other", 1))}},
		{"two Claims", [][]byte{claim, claim}},
		{"incomplete priority", [][]byte{[]byte(strings.Replace(string(claim), `"priority":`, `"discardedPriority":`, 1))}},
		{"same order wrong patient plus valid", [][]byte{claim, []byte(strings.Replace(string(claim), "Patient/MBR-COVERED", "Patient/MBR-NOTCOVERED", 1))}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := pasFactsForOrder(row.claims, order); err == nil {
				t.Fatal("unavailable or ambiguous participant source accepted")
			}
		})
	}
	unrelated := []byte(strings.Replace(string(claim), orderType+"/"+orderID, orderType+"/unrelated", 1))
	if _, err := pasFactsForOrder([][]byte{unrelated, claim}, order); err != nil {
		t.Fatalf("unrelated Claim hid a valid matching Claim: %v", err)
	}
}

func TestPASFactsFromParticipantSourceRejectsMisScopedHeldOrder(t *testing.T) {
	base := newCensusSoR()
	order, ok := base.OpenOrder("MBR-COVERED")
	if !ok {
		t.Fatal("synthetic order absent")
	}
	wrongOrder := []byte(strings.Replace(string(order), "Patient/MBR-COVERED", "Patient/MBR-NOTCOVERED", 1))
	claim, err := syntheticPASDraftClaimForOrder(wrongOrder)
	if err != nil {
		t.Fatal(err)
	}
	sor := misScopedPASSoR{censusSoR: base, order: wrongOrder, claim: claim}
	if _, err := (&Gateway{cfg: Config{SoR: sor}}).pasFactsFromSource(t.Context(), "MBR-COVERED", wrongOrder, wrongOrder); err == nil || !strings.Contains(err.Error(), "patient") {
		t.Fatalf("mis-scoped held order and internally matching Claim accepted for another member: %v", err)
	}
}

func TestPASFactsFromCapturedOrderChecksPatientRewrite(t *testing.T) {
	base := newCensusSoR()
	order, ok := base.OpenOrder("MBR-COVERED")
	if !ok {
		t.Fatal("synthetic order absent")
	}
	sourceOrder := []byte(strings.Replace(string(order), "Patient/MBR-COVERED", "Patient/source-id", 1))
	claim, err := syntheticPASDraftClaimForOrder(sourceOrder)
	if err != nil {
		t.Fatal(err)
	}
	sor := misScopedPASSoR{censusSoR: base, order: sourceOrder, claim: claim, patientRef: "Patient/source-id"}
	submitted, err := NamePatientByMember(sourceOrder, "source-id", "MBR-COVERED")
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{cfg: Config{SoR: sor}}
	if _, err := g.pasFactsFromSource(t.Context(), "MBR-COVERED", sourceOrder, submitted); err != nil {
		t.Fatalf("captured source order and exact registered patient rewrite refused: %v", err)
	}
	mutated := []byte(strings.Replace(string(submitted), "Patient/MBR-COVERED", "Patient/other", 1))
	if _, err := g.pasFactsFromSource(t.Context(), "MBR-COVERED", sourceOrder, mutated); err == nil {
		t.Fatal("caller-mutated submitted order accepted against captured source")
	}
	if _, err := g.pasFactsFromSource(t.Context(), "MBR-COVERED", nil, submitted); err == nil {
		t.Fatal("missing captured source order accepted")
	}
	_, productCode, _, err := shnsdk.ParseOrderProductCoding(sourceOrder)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, old, replacement string
	}{
		{"Claim wrong patient", `"patient":{"reference":"Patient/source-id"}`, `"patient":{"reference":"Patient/other"}`},
		{"Claim wrong product", `"code":"` + productCode + `"`, `"code":"other-product"`},
	} {
		t.Run(row.name, func(t *testing.T) {
			changed := []byte(strings.Replace(string(claim), row.old, row.replacement, 1))
			if bytes.Equal(changed, claim) {
				t.Fatal("Claim mutation did not change the source")
			}
			conflicting := misScopedPASSoR{censusSoR: base, claim: changed, patientRef: "Patient/source-id"}
			if _, err := (&Gateway{cfg: Config{SoR: conflicting}}).pasFactsFromSource(t.Context(), "MBR-COVERED", sourceOrder, submitted); err == nil {
				t.Fatal("contradictory Claim accepted through the captured-order path")
			}
		})
	}
}
