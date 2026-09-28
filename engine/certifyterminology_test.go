package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	"github.com/SmartHealthNetwork/shn-gateway/internal/testrecord"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// outcomeIssues is a recorded OperationOutcome's issue list, each issue kept
// as the lane wrote it so a mutation changes only what it names.
type outcomeIssues []map[string]any

// recordedOutcome is rec's one answer to body (at any profile), as issues.
func recordedOutcome(t *testing.T, rec *testrecord.Recording, body []byte) outcomeIssues {
	t.Helper()
	var found []byte
	for _, ex := range rec.Exchanges {
		if jsonEqualForTest(ex.Request.Body, body) {
			if found != nil {
				t.Fatal("two recorded answers for the body")
			}
			found = ex.Response.Body
		}
	}
	if found == nil {
		t.Fatal("no recorded answer for the body")
	}
	var outcome struct {
		Issue outcomeIssues `json:"issue"`
	}
	if err := json.Unmarshal(found, &outcome); err != nil {
		t.Fatal(err)
	}
	return outcome.Issue
}

// clone deep-copies the issues through JSON.
func (o outcomeIssues) clone(t *testing.T) outcomeIssues {
	t.Helper()
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var out outcomeIssues
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func issueMessageID(issue map[string]any) string {
	details, _ := issue["details"].(map[string]any)
	codings, _ := details["coding"].([]any)
	for _, c := range codings {
		if coding, _ := c.(map[string]any); coding["system"] == "http://hl7.org/fhir/java-core-messageId" {
			code, _ := coding["code"].(string)
			return code
		}
	}
	return ""
}

func issueDiagnostics(issue map[string]any) string {
	d, _ := issue["diagnostics"].(string)
	return d
}

// find is the first issue of severity error with id whose diagnostics contain
// text, from the recorded answers; the row fails when the lane never sent one.
func (o outcomeIssues) find(t *testing.T, id, text string) map[string]any {
	t.Helper()
	for _, issue := range o {
		if issue["severity"] == "error" && issueMessageID(issue) == id && strings.Contains(issueDiagnostics(issue), text) {
			return issue
		}
	}
	t.Fatalf("no recorded %s error containing %q", id, text)
	return nil
}

// serveOutcome answers every $validate with the issues, at 200, as a lane does.
func serveOutcome(t *testing.T, issues outcomeIssues) *shnsdk.OperationValidator {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"resourceType": "OperationOutcome", "issue": issues})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json;charset=UTF-8")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	v := NewCertificationOperationValidator(srv.URL + "/fhir")
	t.Cleanup(v.Client.CloseIdleConnections)
	return v
}

// certifyAt is what the collector records at line for payload through v, and
// the reading of the same answer by certificationUnchecked, asserted at the
// guard as well as at the verdict.
func certifyAt(t *testing.T, v shnsdk.Validator, line string, payload []byte) (LaneVerdict, bool) {
	t.Helper()
	g := certificationGatewayByLine(t, map[string]shnsdk.Validator{line: v}, nil)
	certificationSubmitPayload(g, "terminology", payload)
	certificationFlush(t, g)
	records := g.CertificationEvidenceForTest()
	if len(records) != 1 {
		t.Fatalf("records=%d", len(records))
	}
	var got LaneVerdict
	for _, verdict := range records[0].Verdicts {
		if verdict.Line == line {
			got = verdict
		}
	}
	if slices.Contains(records[0].Certified, line) || records[0].SourceLine != "" {
		t.Fatalf("an answer with errors was certified: %+v", records[0])
	}
	species := detectSpecies(payload)
	profile, _ := profileFor(species, line, "pas-claim")
	res, err := v.Validate(context.Background(), payload, profile)
	if err != nil {
		t.Fatal(err)
	}
	_, unchecked := certificationUnchecked(res, payload, line, profile)
	return got, unchecked
}

// Mutation rows over each lane's recorded answer to its own line's synthetic
// PAS request bundle (testdata/recordings/lane-<line>-certify-collect.json).
// The base is asserted first: every error is X12 terminology the lane could
// not check, or follows from it, so the verdict is unavailable naming the
// code system. Each mutation adds or changes one thing the lane could have
// said, and each turns the answer into a verdict the lane did reach: invalid.
// The added issues are the lanes' own, taken from the same recordings.
func TestCertificationTerminologyUnavailableIsOnlyTerminology(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			rec := certifyRecording(t, "lane-"+line+"-certify-collect")
			rec.Subset() // the recording is read, not served
			payload, _, ok := lanequalify.CertificationRow(line)
			if !ok {
				t.Fatal("no certification row")
			}
			base := recordedOutcome(t, rec, payload)
			var all outcomeIssues
			for _, ex := range rec.Exchanges {
				var outcome struct {
					Issue outcomeIssues `json:"issue"`
				}
				if json.Unmarshal(ex.Response.Body, &outcome) == nil {
					all = append(all, outcome.Issue...)
				}
			}

			got, unchecked := certifyAt(t, serveOutcome(t, base), line, payload)
			if got.State != "unavailable" || got.Valid || got.Error != "terminology unavailable: "+x12ServiceType || !unchecked {
				t.Fatalf("base: verdict %+v unchecked=%v, want unavailable naming the X12 code system", got, unchecked)
			}

			// A structural error the lane reported on another payload: a
			// required element missing, placed on the bundle's ServiceRequest.
			structural := func(o outcomeIssues) outcomeIssues {
				issue := outcomeIssues{all.find(t, "Validation_VAL_Profile_Minimum", "minimum required")}.clone(t)[0]
				issue["expression"] = []any{"Bundle.entry[2].resource"}
				return append(o, issue)
			}
			// An invariant the lane found failed on a questionnaire answer.
			invariant := func(o outcomeIssues) outcomeIssues {
				for _, issue := range all {
					if issue["severity"] == "error" && invariantMessageID.MatchString(issueMessageID(issue)) {
						return append(o, outcomeIssues{issue}.clone(t)[0])
					}
				}
				t.Fatal("the lane recorded no failed invariant")
				return nil
			}
			// The required-binding miss names a code in a code system the lane
			// holds: a real miss, not terminology it could not check.
			loadedSystemMiss := func(o outcomeIssues) outcomeIssues {
				n := 0
				for _, issue := range o {
					if issueMessageID(issue) == "Terminology_TX_NoValid_1_CC" {
						issue["diagnostics"] = strings.Replace(issueDiagnostics(issue), "(codes = "+x12ServiceType+"#1)", "(codes = http://terminology.hl7.org/CodeSystem/claim-type#bogus)", 1)
						n++
					}
				}
				if n == 0 {
					t.Fatal("no required-binding miss to change")
				}
				return o
			}
			// The required-binding miss alone, with nothing saying the lane
			// could not check its code system.
			bareMiss := func(o outcomeIssues) outcomeIssues {
				var out outcomeIssues
				for _, issue := range o {
					if issueMessageID(issue) != "Terminology_PassThrough_TX_Message" {
						out = append(out, issue)
					}
				}
				return out
			}
			// The lane checked the code and did not find it: "Unknown code".
			unknownCode := func(o outcomeIssues) outcomeIssues {
				for _, issue := range o {
					if d := issueDiagnostics(issue); issue["severity"] == "error" && strings.HasPrefix(d, "CodeSystem is unknown and can't be validated: ") {
						issue["diagnostics"] = "Unknown code '" + x12ServiceType + "#1'"
						return o
					}
				}
				t.Fatal("no unchecked code system to change")
				return nil
			}
			// The miss sits on another element than the code system the lane
			// could not check.
			otherElement := func(o outcomeIssues) outcomeIssues {
				for _, issue := range o {
					if issueMessageID(issue) == "Terminology_TX_NoValid_1_CC" {
						issue["expression"] = []any{"Bundle.entry[0].resource.type"}
						return o
					}
				}
				t.Fatal("no required-binding miss to move")
				return nil
			}
			// A fatal issue is never terminology the lane could not check.
			fatal := func(o outcomeIssues) outcomeIssues {
				for _, issue := range o {
					if issue["severity"] == "error" && issueMessageID(issue) == "Terminology_PassThrough_TX_Message" {
						issue["severity"] = "fatal"
						return o
					}
				}
				t.Fatal("no passed-through verdict to change")
				return nil
			}
			// codes rewrites every required-binding miss's code list.
			codes := func(list string) func(outcomeIssues) outcomeIssues {
				return func(o outcomeIssues) outcomeIssues {
					n := 0
					for _, issue := range o {
						if issueMessageID(issue) == "Terminology_TX_NoValid_1_CC" {
							issue["diagnostics"] = strings.Replace(issueDiagnostics(issue), "(codes = "+x12ServiceType+"#1)", "(codes = "+list+")", 1)
							n++
						}
					}
					if n == 0 {
						t.Fatal("no required-binding miss to change")
					}
					return o
				}
			}
			// The lane says the coding's system is unknown but never that the
			// bound value set could not be expanded: the miss is then one it
			// decided.
			noUnexpandable := func(o outcomeIssues) outcomeIssues {
				var out outcomeIssues
				for _, issue := range o {
					if !strings.HasPrefix(issueDiagnostics(issue), "Unable to expand ValueSet because CodeSystem could not be found: ") {
						out = append(out, issue)
					}
				}
				return out
			}
			// A code of a system the lane does not know (a CRD code system the
			// lanes do not load) under a value set the lane can expand, in the
			// lane's own words (the questionnaire answer's DocReason, recorded on
			// the 2.0 and 2.1 lanes): "CodeSystem is unknown" and the miss, with
			// no "Unable to expand". The miss is decided.
			foreignSystem := func(o outcomeIssues) outcomeIssues {
				for _, issue := range all {
					if issue["severity"] == "error" && strings.HasPrefix(issueDiagnostics(issue), "CodeSystem is unknown and can't be validated: http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes") {
						return append(o, outcomeIssues{issue, all.find(t, "Terminology_TX_NoValid_1_CC", "CRD Coverage Information Documentation Reason Value Set")}.clone(t)...)
					}
				}
				// The 2.2 lane holds that CRD code system (it answered "Unknown code"
				// there), so on 2.2 the same pair is written in the other lanes' words.
				return append(o, outcomeIssues{
					{"severity": "error", "code": "processing", "details": map[string]any{"coding": []any{map[string]any{"system": "http://hl7.org/fhir/java-core-messageId", "code": "Terminology_PassThrough_TX_Message"}}}, "expression": []any{"Bundle.entry[1].resource.extension[2].value.ofType(CodeableConcept)"}, "diagnostics": "CodeSystem is unknown and can't be validated: http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes for 'http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes#withpa'"},
					{"severity": "error", "code": "processing", "details": map[string]any{"coding": []any{map[string]any{"system": "http://hl7.org/fhir/java-core-messageId", "code": "Terminology_TX_NoValid_1_CC"}}}, "expression": []any{"Bundle.entry[1].resource.extension[2].value.ofType(CodeableConcept)"}, "diagnostics": "None of the codings provided are in the value set 'CRD Coverage Information Documentation Reason Value Set' (http://hl7.org/fhir/us/davinci-crd/ValueSet/DocReason|2.0.1), and a coding from this value set is required) (codes = http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes#withpa)"},
				}...)
			}
			rows := map[string]func(outcomeIssues) outcomeIssues{
				"a binding miss with no value set the lane could not expand":  noUnexpandable,
				"a foreign code system under a value set the lane can expand": foreignSystem,
				"a second code behind a ')' in the code list":                 codes(x12ServiceType + "#1), http://terminology.hl7.org/CodeSystem/claim-type#bogus"),
				"a code list named twice":                                     codes(x12ServiceType + "#1) (codes = " + x12ServiceType + "#1"),
				"a code list with a code without a system":                    codes(x12ServiceType + "#1, bogus"),
				"a structural error":                                          structural,
				"a failed invariant":                                          invariant,
				"a binding miss on a code system the lane holds":              loadedSystemMiss,
				"a binding miss with no unchecked code system":                bareMiss,
				"a code the lane checked and did not find":                    unknownCode,
				"a binding miss on another element":                           otherElement,
				"a fatal passed-through verdict":                              fatal,
			}
			if line != "2.0" {
				// On 2.1 and 2.2 the Claim matches neither Claim profile. A
				// structural requirement of the Claim's own profile on that entry
				// (the lane's own, from the 2.0 bundle it was asked at this line)
				// leaves the no-match summaries counting too.
				rows["a summary on an entry with a structural sibling"] = func(o outcomeIssues) outcomeIssues {
					return append(o, outcomeIssues{all.find(t, "Validation_VAL_Profile_Minimum", "Claim.item.location[x]: minimum required = 1, but only found 0 (from http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|")}.clone(t)[0])
				}
				// The Claim's no-match summaries and the update profile's own
				// requirement, with nothing on the entry the lane could not check:
				// they follow from no unchecked terminology.
				rows["a summary with no unchecked terminology on its entry"] = func(o outcomeIssues) outcomeIssues {
					var out outcomeIssues
					for _, issue := range o {
						if id := issueMessageID(issue); issue["severity"] != "error" || (id != "Terminology_PassThrough_TX_Message" && id != "Terminology_TX_NoValid_1_CC") {
							out = append(out, issue)
						}
					}
					return out
				}
			}
			// A ")" inside a code is part of the code: the list is read to the
			// ")" that closes it, and the code's system is still one the lane
			// could not expand the value set for.
			t.Run("a ')' inside a code", func(t *testing.T) {
				got, unchecked := certifyAt(t, serveOutcome(t, codes(x12ServiceType+"#1)")(base.clone(t))), line, payload)
				if got.State != "unavailable" || !unchecked || got.Error != "terminology unavailable: "+x12ServiceType {
					t.Fatalf("verdict %+v unchecked=%v, want unavailable", got, unchecked)
				}
			})
			for name, mutate := range rows {
				t.Run(name, func(t *testing.T) {
					mutated := mutate(base.clone(t))
					raw, _ := json.Marshal(mutated)
					if b, _ := json.Marshal(base); bytes.Equal(raw, b) {
						t.Fatal("the mutation changed nothing")
					}
					got, unchecked := certifyAt(t, serveOutcome(t, mutated), line, payload)
					if got.State != "invalid" || got.Valid || got.Error != "" || unchecked {
						t.Fatalf("verdict %+v unchecked=%v, want invalid", got, unchecked)
					}
				})
			}
		})
	}
}

// The reason names a code system only when it is an X12 canonical; any other
// name, which comes from the payload's own coding, is its size and digest, and
// the reason stays bounded however many there are.
func TestCertificationTerminologyReasonIsBounded(t *testing.T) {
	const sentinel = "PRIVATE-SYNTHETIC-SENTINEL"
	crd := "http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes"
	crdHash := sha256.Sum256([]byte(crd))
	for _, row := range []struct {
		systems []string
		want    string
	}{
		{[]string{x12ServiceType}, "terminology unavailable: " + x12ServiceType},
		{[]string{x12ServiceType, "https://codesystem.x12.org/005010/306"}, "terminology unavailable: " + x12ServiceType + ", https://codesystem.x12.org/005010/306"},
		{[]string{crd, x12ServiceType}, fmt.Sprintf("terminology unavailable: code system bytes=%d sha256=%x, %s", len(crd), crdHash, x12ServiceType)},
	} {
		if got := terminologyUnavailableReason(row.systems); got != row.want {
			t.Errorf("%v: %q, want %q", row.systems, got, row.want)
		}
	}
	for _, system := range []string{
		"https://example.org/" + sentinel,
		"https://codesystem.x12.org/005010/" + sentinel,
		"https://codesystem.x12.org/005010/1365?" + sentinel,
		"http://terminology.hl7.org/CodeSystem/" + sentinel,
		"http://hl7.org/fhir/" + sentinel,
		"urn:oid:" + sentinel,
		"https://codesystem.x12.org/005010/1365/" + sentinel,
	} {
		got := terminologyUnavailableReason([]string{system, system + "2", system + "3"})
		if strings.Contains(got, sentinel) || !strings.Contains(got, "sha256=") || !strings.HasSuffix(got, " and 1 more") || len(got) > 256 {
			t.Errorf("%q: reason %q, want digests only, bounded", system, got)
		}
	}
}

// Every reader of the evidence sees an unavailable line as not certified. The
// target line 2.1 answers the lane's X12 terminology alone (its recorded
// answer to its own bundle), 2.2 answers invalid (its recorded answer to the
// same bundle) and 2.0 answers a clean result: only 2.0 is certified, the
// source line is 2.0 rather than the unavailable target, and the observer
// event and the certify: log line carry the same record, the unavailable
// verdict with valid false.
func TestCertificationUnavailableIsNeverCertified(t *testing.T) {
	payload, _, _ := lanequalify.CertificationRow("2.1")
	served := func(line string) shnsdk.Validator {
		rec := certifyRecording(t, "lane-"+line+"-certify-collect")
		rec.Subset() // one bundle's answer is served
		return serveOutcome(t, recordedOutcome(t, rec, payload))
	}
	var output lockedBuffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	var observed []string
	g := certificationGatewayByLine(t, map[string]shnsdk.Validator{"2.0": &shnsdk.FakeValidator{}, "2.1": served("2.1"), "2.2": served("2.2")}, func(e ObserverEvent) {
		if e.Kind == "leg.certified" {
			observed = append(observed, e.Detail)
		}
	})
	certificationSubmitPayload(g, "consumers", payload)
	certificationFlush(t, g)
	e := g.CertificationEvidenceForTest()[0]
	if e.TargetLine != "2.1" || !slices.Equal(e.Certified, []string{"2.0"}) || e.SourceLine != "2.0" {
		t.Fatalf("target %s certified %v source %q, want only 2.0 certified and the source", e.TargetLine, e.Certified, e.SourceLine)
	}
	for line, state := range map[string]string{"2.0": "valid", "2.1": "unavailable", "2.2": "invalid"} {
		if got := laneVerdict(t, e, line); got.State != state || got.Valid != (state == "valid") {
			t.Fatalf("%s: %+v, want %s", line, got, state)
		}
	}
	ring, _ := json.Marshal(e)
	if len(observed) != 1 || observed[0] != string(ring) || !strings.Contains(output.String(), "certify: "+string(ring)) {
		t.Fatal("the observer event or the log line is not the stored record")
	}
	var decoded CertificationEvidence
	if err := json.Unmarshal([]byte(observed[0]), &decoded); err != nil || laneVerdict(t, decoded, "2.1").Valid || slices.Contains(decoded.Certified, "2.1") {
		t.Fatalf("the observed record reads the unavailable line as valid: %s", observed[0])
	}
}

// A required-binding miss's code list is read to the ")" that closes it, each
// code split at its last "#": a ")" or "#" inside the system or the code does
// not move where the list ends, and a list it cannot read unambiguously is
// not read at all.
func TestMissedCodingSystems(t *testing.T) {
	const tail = " (validating against http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|2.2.1 [PASClaim])"
	for _, row := range []struct {
		diagnostics string
		want        []string
	}{
		{"… is required) (codes = " + x12ServiceType + "#1)", []string{x12ServiceType}},
		{"… is required) (codes = " + x12ServiceType + "#1)" + tail, []string{x12ServiceType}},
		{"… is required) (codes = " + x12ServiceType + "#1), http://terminology.hl7.org/CodeSystem/claim-type#x)" + tail, []string{x12ServiceType, "http://terminology.hl7.org/CodeSystem/claim-type"}},
		{"… is required) (codes = https://example.org/cs#part#1)", []string{"https://example.org/cs#part"}},
		{"… is required) (codes = " + x12ServiceType + "#1)(x)", []string{x12ServiceType}},
		{"… is required)", nil},
		{"… (codes = a#1) (codes = b#2)", nil},
		{"… (codes = #1)", nil},
		{"… (codes = nohash)", nil},
		{"… (codes = " + x12ServiceType + "#1) trailing", nil},
	} {
		got, ok := missedCodingSystems(row.diagnostics)
		if ok != (row.want != nil) || !slices.Equal(got, row.want) {
			t.Errorf("%q: %v %v, want %v", row.diagnostics, got, ok, row.want)
		}
	}
}

// The PAS Claim exception excuses the other Claim profile's own cardinality
// requirements on any element, not only Claim.related, when they are
// attributed to that profile alone. The base is each lane's recorded answer to
// its own line's request bundle (a submit, so the other profile is
// profile-claim-update); the added issue is the lane's own
// Claim.item.location[x] minimum attributed only to profile-claim-update (from
// the 2.0 bundle it was asked at this line). Attributed to both profiles, the
// same miss is the Claim's own and keeps the answer invalid.
func TestCertificationClaimExceptionScope(t *testing.T) {
	for _, line := range []string{"2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			rec := certifyRecording(t, "lane-"+line+"-certify-collect")
			rec.Subset() // the recording is read, not served
			payload, _, _ := lanequalify.CertificationRow(line)
			base := recordedOutcome(t, rec, payload)
			var all outcomeIssues
			for _, ex := range rec.Exchanges {
				var outcome struct {
					Issue outcomeIssues `json:"issue"`
				}
				if json.Unmarshal(ex.Response.Body, &outcome) == nil {
					all = append(all, outcome.Issue...)
				}
			}
			update := "(validating against http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update|"
			miss := outcomeIssues{all.find(t, "Validation_VAL_Profile_Minimum", "Claim.item.location[x]: minimum required = 1, but only found 0 (from http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update|")}.clone(t)[0]
			if d := issueDiagnostics(miss); strings.Count(d, "(validating against ") != 1 || !strings.Contains(d, update) {
				t.Fatalf("the recorded miss is no longer attributed to profile-claim-update alone: %q", d)
			}

			got, unchecked := certifyAt(t, serveOutcome(t, append(base.clone(t), miss)), line, payload)
			if got.State != "unavailable" || !unchecked {
				t.Fatalf("the other profile's own location[x] minimum: %+v unchecked=%v, want unavailable", got, unchecked)
			}

			both := outcomeIssues{miss}.clone(t)[0]
			both["diagnostics"] = issueDiagnostics(both) + " (validating against http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim|" + map[string]string{"2.1": "2.1.0", "2.2": "2.2.1"}[line] + " [PASClaim])"
			got, unchecked = certifyAt(t, serveOutcome(t, append(base.clone(t), both)), line, payload)
			if got.State != "invalid" || unchecked {
				t.Fatalf("the same miss attributed to both profiles: %+v unchecked=%v, want invalid", got, unchecked)
			}
		})
	}
}
