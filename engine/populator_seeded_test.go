package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// buildSeededPackage builds a minimal $questionnaire-package Parameters wrapper (the
// shape br-payer returns) around a bare Questionnaire with the given canonical url and
// a single group item (id "1") containing a single required leaf item of the given type
// ("boolean" or "choice") at linkId leafLinkID.
func buildSeededPackage(t *testing.T, questionnaireURL, groupLinkID, leafLinkID, leafType string) []byte {
	t.Helper()
	q := map[string]any{
		"resourceType": "Questionnaire",
		"id":           "test-q",
		"url":          questionnaireURL,
		"status":       "active",
		"item": []map[string]any{
			{
				"linkId": groupLinkID,
				"type":   "group",
				"item": []map[string]any{
					{
						"linkId":   leafLinkID,
						"type":     leafType,
						"required": true,
					},
				},
			},
		},
	}
	qBytes, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal questionnaire: %v", err)
	}
	// Wrap in a Parameters shape (br-payer's dtr-qpackage-output-parameters format).
	pkg := map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{
				"name": "packagebundle",
				"resource": map[string]any{
					"resourceType": "Bundle",
					"type":         "collection",
					"entry": []map[string]any{
						{"resource": json.RawMessage(qBytes)},
					},
				},
			},
		},
	}
	pkgBytes, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal package: %v", err)
	}
	return pkgBytes
}

// TestSeededPopulator_L8000Fill verifies that the seeded populator fills the
// PriorAuthRequired (L8000) questionnaire's required boolean item 1.2 with true,
// sets the correct QR subject, and reports a fill summary entry with Origin="manual".
func TestSeededPopulator_L8000Fill(t *testing.T) {
	const (
		canonical  = "http://example.org/fhir/Questionnaire/PriorAuthRequired"
		patientRef = "Patient/X"
		author     = "Practitioner/1234567890"
	)
	pkg := buildSeededPackage(t, canonical, "1", "1.2", "boolean")
	pop := NewSeededPopulator(author)
	pc := PopulateContext{
		PatientRef:  patientRef,
		CoverageRef: "Coverage/cov1",
		OrderRef:    "ServiceRequest/order1",
		Authored:    time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC),
	}

	qrJSON, fill, err := pop.Populate(context.Background(), pkg, pc)
	if err != nil {
		t.Fatalf("Populate L8000: unexpected error: %v", err)
	}

	// Assert QR subject == PatientRef.
	subj, err := questionnaireResponseSubject(qrJSON)
	if err != nil {
		t.Fatalf("questionnaireResponseSubject: %v", err)
	}
	if subj != patientRef {
		t.Errorf("QR subject = %q, want %q", subj, patientRef)
	}

	// Assert QR item 1.2 is answered with valueBoolean = true.
	var qr struct {
		Item []struct {
			LinkID string `json:"linkId"`
			Item   []struct {
				LinkID string `json:"linkId"`
				Answer []struct {
					ValueBoolean *bool `json:"valueBoolean"`
				} `json:"answer"`
			} `json:"item"`
		} `json:"item"`
	}
	if err := json.Unmarshal(qrJSON, &qr); err != nil {
		t.Fatalf("unmarshal QR: %v", err)
	}
	found := false
	for _, grp := range qr.Item {
		for _, leaf := range grp.Item {
			if leaf.LinkID == "1.2" {
				if len(leaf.Answer) == 0 {
					t.Fatal("item 1.2 has no answers")
				}
				if leaf.Answer[0].ValueBoolean == nil || !*leaf.Answer[0].ValueBoolean {
					t.Errorf("item 1.2 valueBoolean = %v, want true", leaf.Answer[0].ValueBoolean)
				}
				found = true
			}
		}
	}
	if !found {
		t.Error("item 1.2 not found in QR")
	}

	// Assert fill summary has a 1.2 entry with Origin="manual".
	foundFill := false
	for _, fi := range fill {
		if fi.LinkID == "1.2" {
			if fi.Origin != "manual" {
				t.Errorf("fill[1.2].Origin = %q, want \"manual\"", fi.Origin)
			}
			foundFill = true
		}
	}
	if !foundFill {
		t.Error("fill summary has no entry for linkId 1.2")
	}
}

// TestSeededPopulator_G0151Fill verifies that the seeded populator fills the
// HomeHealthAssessment (G0151) questionnaire's required choice item 1.1 with the
// SNOMED code 91251008 (Physical therapy procedure), and that the fill summary
// records Origin="manual".
func TestSeededPopulator_G0151Fill(t *testing.T) {
	const (
		canonical  = "http://example.org/fhir/Questionnaire/HomeHealthAssessment"
		patientRef = "Patient/Y"
		author     = "Practitioner/1234567890"
	)
	pkg := buildSeededPackage(t, canonical, "1", "1.1", "choice")
	pop := NewSeededPopulator(author)
	pc := PopulateContext{
		PatientRef:  patientRef,
		CoverageRef: "Coverage/cov2",
		OrderRef:    "ServiceRequest/order2",
		Authored:    time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC),
	}

	qrJSON, fill, err := pop.Populate(context.Background(), pkg, pc)
	if err != nil {
		t.Fatalf("Populate G0151: unexpected error: %v", err)
	}

	// Assert QR subject == PatientRef.
	subj, err := questionnaireResponseSubject(qrJSON)
	if err != nil {
		t.Fatalf("questionnaireResponseSubject: %v", err)
	}
	if subj != patientRef {
		t.Errorf("QR subject = %q, want %q", subj, patientRef)
	}

	// Assert QR item 1.1 has the expected SNOMED valueCoding.
	var qr struct {
		Item []struct {
			LinkID string `json:"linkId"`
			Item   []struct {
				LinkID string `json:"linkId"`
				Answer []struct {
					ValueCoding *struct {
						System  string `json:"system"`
						Code    string `json:"code"`
						Display string `json:"display"`
					} `json:"valueCoding"`
				} `json:"answer"`
			} `json:"item"`
		} `json:"item"`
	}
	if err := json.Unmarshal(qrJSON, &qr); err != nil {
		t.Fatalf("unmarshal QR: %v", err)
	}
	found := false
	for _, grp := range qr.Item {
		for _, leaf := range grp.Item {
			if leaf.LinkID == "1.1" {
				if len(leaf.Answer) == 0 {
					t.Fatal("item 1.1 has no answers")
				}
				c := leaf.Answer[0].ValueCoding
				if c == nil {
					t.Fatal("item 1.1 answer has no valueCoding")
				}
				if c.System != "http://snomed.info/sct" {
					t.Errorf("item 1.1 coding.system = %q, want SNOMED URI", c.System)
				}
				if c.Code != "91251008" {
					t.Errorf("item 1.1 coding.code = %q, want 91251008", c.Code)
				}
				found = true
			}
		}
	}
	if !found {
		t.Error("item 1.1 not found in QR")
	}

	// Assert fill summary has a 1.1 entry with Origin="manual".
	foundFill := false
	for _, fi := range fill {
		if fi.LinkID == "1.1" {
			if fi.Origin != "manual" {
				t.Errorf("fill[1.1].Origin = %q, want \"manual\"", fi.Origin)
			}
			if fi.Answer != "91251008" {
				t.Errorf("fill[1.1].Answer = %q, want \"91251008\"", fi.Answer)
			}
			foundFill = true
		}
	}
	if !foundFill {
		t.Error("fill summary has no entry for linkId 1.1")
	}
}

// TestSeededPopulator_HonestyGuard_UnknownQuestionnaire is the rejection test for the
// honesty guard: a package whose questionnaire canonical is NOT in the answer book must
// cause Populate to return an error. The gateway must never fabricate answers for an
// unseeded questionnaire.
func TestSeededPopulator_HonestyGuard_UnknownQuestionnaire(t *testing.T) {
	const canonical = "http://example.org/fhir/Questionnaire/UnseededQuestionnaire"
	pkg := buildSeededPackage(t, canonical, "1", "1.1", "boolean")
	pop := NewSeededPopulator("Practitioner/1234567890")
	pc := PopulateContext{
		PatientRef: "Patient/Z",
		Authored:   time.Now(),
	}

	_, _, err := pop.Populate(context.Background(), pkg, pc)
	if err == nil {
		t.Fatal("Populate with unseeded questionnaire: expected error (honesty guard), got nil")
	}
	// The error message must mention the canonical so the operator knows what to seed.
	if !strings.Contains(err.Error(), canonical) {
		t.Errorf("honesty guard error %q does not mention the unknown canonical %q", err.Error(), canonical)
	}
}

// TestSeededPopulator_AuthorStamped verifies the dtrx-1 chain: the filled QR carries
// source="manual" + the author reference on each answered item. This proves the
// honesty contract is end-to-end through the gateway layer (not just the SDK).
func TestSeededPopulator_AuthorStamped(t *testing.T) {
	const (
		canonical = "http://example.org/fhir/Questionnaire/PriorAuthRequired"
		author    = "Practitioner/9999999999"
	)
	pkg := buildSeededPackage(t, canonical, "1", "1.2", "boolean")
	pop := NewSeededPopulator(author)
	pc := PopulateContext{
		PatientRef: "Patient/A",
		Authored:   time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC),
	}

	qrJSON, fill, err := pop.Populate(context.Background(), pkg, pc)
	if err != nil {
		t.Fatalf("Populate: %v", err)
	}

	// Assert source="manual" extension is present somewhere in the QR JSON.
	if !strings.Contains(string(qrJSON), `"manual"`) {
		t.Error("QR does not contain source=manual extension")
	}
	// Assert the author reference appears in the QR.
	if !strings.Contains(string(qrJSON), author) {
		t.Errorf("QR does not contain author reference %q", author)
	}

	// Assert fill summary SourceRef is the author.
	for _, fi := range fill {
		if fi.LinkID == "1.2" {
			if fi.SourceRef != author {
				t.Errorf("fill[1.2].SourceRef = %q, want %q", fi.SourceRef, author)
			}
		}
	}
}
