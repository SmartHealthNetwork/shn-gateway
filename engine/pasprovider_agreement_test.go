package engine

// pasprovider_agreement_test.go — the submission and the inquiry about it name
// the SAME party, for every persona this repository seeds.
//
// A payer matches a prior-authorization inquiry on the member id PLUS the
// ordering or rendering provider identifier. Two ways of getting that wrong end
// in the same place — an authorization nobody can find again — and only one of
// them is visible in a single message:
//
//   - the submission identifies NOBODY (a Claim.provider carrying display text
//     alone, which satisfies the element at every PAS line), or
//   - the submission and the inquiry each name a real party, and a DIFFERENT
//     one. That one is invisible in either message on its own.
//
// The row below drives the PUBLISHED provider-data seed — the personas partners
// actually load — through the one selection rule (shnsdk.SelectPASProvider) and
// then through BOTH builders, and compares what each put on the wire.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// seededRecords indexes the published provider-data seed by "Type/id".
func seededRecords(t *testing.T) map[string][]byte {
	t.Helper()
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(fhirseed.ProviderDataSeedBundle(), &bundle); err != nil {
		t.Fatalf("read the published provider-data seed: %v", err)
	}
	byRef := map[string][]byte{}
	for _, e := range bundle.Entry {
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if json.Unmarshal(e.Resource, &head) != nil || head.ID == "" {
			continue
		}
		byRef[head.ResourceType+"/"+head.ID] = append([]byte(nil), e.Resource...)
	}
	if len(byRef) == 0 {
		t.Fatal("the published provider-data seed carries no resources")
	}
	return byRef
}

// seededPASOrders returns every seeded order a prior authorization is submitted
// for, keyed by the member it belongs to. The set is read out of the seed rather
// than listed here, so a persona added later is covered without an edit.
func seededPASOrders(t *testing.T, byRef map[string][]byte) map[string][]byte {
	t.Helper()
	// The coverage-check-only personas never reach a prior authorization: UC-02
	// stops at the CRD card, and its second-payer twin is a routing fixture no
	// scenario drives.
	coverageCheckOnly := map[string]bool{"MBR-PD-UC02": true, "MBR-PD-UC02-PB": true}
	out := map[string][]byte{}
	for ref, raw := range byRef {
		typ, _, _ := strings.Cut(ref, "/")
		if typ != "ServiceRequest" && typ != "DeviceRequest" {
			continue
		}
		var probe struct {
			Subject struct {
				Reference string `json:"reference"`
			} `json:"subject"`
		}
		if json.Unmarshal(raw, &probe) != nil {
			continue
		}
		member := strings.TrimPrefix(probe.Subject.Reference, "Patient/")
		if member == "" || coverageCheckOnly[member] {
			continue
		}
		out[member] = raw
	}
	if len(out) == 0 {
		t.Fatal("the published provider-data seed carries no prior-authorization orders")
	}
	return out
}

// TestSeededPersona_SubmitAndInquiryNameTheSameProvider: for EVERY seeded
// persona that submits a prior authorization, the party the submission
// identifies and the party the inquiry about it identifies are the same one.
//
// Both are driven from the persona's OWN seeded records, through the shared
// selection rule; nothing here chooses an identity.
func TestSeededPersona_SubmitAndInquiryNameTheSameProvider(t *testing.T) {
	byRef := seededRecords(t)
	resolve := func(ref string) ([]byte, bool, error) {
		raw, ok := byRef[ref]
		return raw, ok, nil
	}
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	for member, order := range seededPASOrders(t, byRef) {
		t.Run(member, func(t *testing.T) {
			ref, provider, err := shnsdk.SelectPASProvider(order, resolve)
			if err != nil {
				t.Fatalf("this persona submits a prior authorization and names no party a request can carry: %v", err)
			}
			if npi := shnsdk.PASProviderNPI(provider); npi == "" {
				t.Fatalf("the seeded provider %s carries no NPI, so a payer has nothing to match an inquiry on: %s", ref, provider)
			}

			// The records the participant's own system holds for this member —
			// the SAME ones the submission and the inquiry are both built from,
			// which is the whole point: a payer matches an inquiry against the
			// coverage it stored, so the two must name one record.
			patient, ok := byRef["Patient/"+member]
			if !ok {
				t.Fatalf("the seed holds no Patient/%s", member)
			}
			coverage, insurer := seededCoverageAndPayer(t, byRef, member)

			// The submission, built exactly as every shipped originator lane
			// builds it (those lanes all relay reference-payer bytes).
			submit, err := buildPASSubmitBundle("2.0", true, order, nil, provider, coverage, insurer, shnsdk.MemberSystem,
				"Patient/"+member, "Coverage/"+member, member, "corr-"+member, created, shnsdk.CMSPayerIdentity)
			if err != nil {
				t.Fatalf("build the submission: %v", err)
			}
			inquiry, err := shnsdk.BuildPASInquiryBundle("2.0", shnsdk.PASInquiryInputs{
				ID:              "inq-" + strings.ToLower(strings.ReplaceAll(member, "-", "")),
				Identifier:      shnsdk.PASIdentifier{System: shnsdk.PASInquiryIdentifierSystem, Value: "inq-" + member},
				ClaimIdentifier: shnsdk.PASIdentifier{System: shnsdk.PASInquiryIdentifierSystem, Value: "inq-" + member},
				Timestamp:       created,
				ClaimType:       shnsdk.PASCoding{System: "http://terminology.hl7.org/CodeSystem/claim-type", Code: "professional"},
				Priority:        shnsdk.PASCoding{Code: "normal"},
				MemberID:        member,
				Patient:         patient,
				Coverage:        coverage,
				Provider:        provider,
				Insurer:         insurer,
				Items: []shnsdk.PASInquiryItem{{
					Sequence:         1,
					ProductOrService: shnsdk.PASCoding{System: "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets", Code: "E0424"},
					TraceNumber:      shnsdk.PASIdentifier{System: shnsdk.PASItemTraceSystem, Value: "trace-1"},
				}},
			})
			if err != nil {
				t.Fatalf("build the inquiry: %v", err)
			}

			submitRef := bundleClaimProvider(t, submit)
			inquiryRef := bundleClaimProvider(t, inquiry.Body)
			if submitRef != inquiryRef {
				t.Fatalf("the submission names %q and the inquiry about it names %q — a payer matching on the provider identifier would find nothing",
					submitRef, inquiryRef)
			}
			// And the party the submission names rides it, so the payer that
			// stores the authorization has an identity to match against.
			carried, ok := bundleEntryAt(t, submit, submitRef)
			if !ok {
				t.Fatalf("the submission names %q and does not carry it", submitRef)
			}
			if got := shnsdk.PASProviderNPI(carried); got != shnsdk.PASProviderNPI(provider) {
				t.Fatalf("the carried party's NPI is %q, the persona's own record says %q", got, shnsdk.PASProviderNPI(provider))
			}
		})
	}
}

// TestOriginator_SubmitRefusesAnOrderNamingNoCarryableProvider is the
// SUBMIT-side rejection row for the shared selection rule (the inquiry side has
// its own, TestOriginator_ProviderAnInquiryCannotCarryIsRefusedByName).
//
// The refusal happens before anything is sent. A request the payer would store
// with no provider identity is worse than a refused one: it would be adjudicated
// and answered, and only a later inquiry — days or weeks on — would report that
// no authorization matches.
func TestOriginator_SubmitRefusesAnOrderNamingNoCarryableProvider(t *testing.T) {
	for _, tc := range []struct{ name, order, want string }{
		{"names nobody at all",
			`{"resourceType":"ServiceRequest","id":"sr-follow","status":"active","intent":"order",` +
				`"subject":{"reference":"Patient/` + pasFollowMember + `"},` +
				`"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}`,
			"names no requesting provider"},
		{"names only the ordering clinician",
			`{"resourceType":"ServiceRequest","id":"sr-follow","status":"active","intent":"order",` +
				`"subject":{"reference":"Patient/` + pasFollowMember + `"},` +
				`"requester":{"reference":"Practitioner/prac-follow"},` +
				`"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}`,
			"Organization or a PractitionerRole"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, stub := pasFollowSystem(t, "approved")
			sor := gw.cfg.SoR.(*pasFollowSoR)
			sor.orderOverride = []byte(tc.order)
			in := pasFollowSubmit(0)
			in.orderJSON = []byte(tc.order)

			req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
			_, status, msg, _ := gw.submitClaimAndFollow(req.Context(), req, in)
			if status == 0 {
				t.Fatal("a request that would identify no provider must be refused, not sent")
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("the refusal must name what it found: %q", msg)
			}
			if len(stub.legTypes) != 0 {
				t.Fatalf("nothing may be sent for a request that cannot name its provider: %v", stub.legTypes)
			}
		})
	}
}

// seededCoverageAndPayer returns a member's own Coverage and the payer
// Organization it names.
func seededCoverageAndPayer(t *testing.T, byRef map[string][]byte, member string) (coverage, insurer []byte) {
	t.Helper()
	for ref, raw := range byRef {
		if !strings.HasPrefix(ref, "Coverage/") {
			continue
		}
		var c struct {
			Beneficiary struct {
				Reference string `json:"reference"`
			} `json:"beneficiary"`
			Payor []struct {
				Reference string `json:"reference"`
			} `json:"payor"`
		}
		if json.Unmarshal(raw, &c) != nil || c.Beneficiary.Reference != "Patient/"+member {
			continue
		}
		for _, p := range c.Payor {
			if org, ok := byRef[p.Reference]; ok {
				return raw, org
			}
		}
		t.Fatalf("%s names a payer the seed does not carry", ref)
	}
	t.Fatalf("the seed holds no Coverage for %s", member)
	return nil, nil
}

// bundleClaimProvider reads the first Claim's provider reference from a bundle.
func bundleClaimProvider(t *testing.T, body []byte) string {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.Entry {
		var c struct {
			ResourceType string `json:"resourceType"`
			Provider     struct {
				Reference string `json:"reference"`
				Display   string `json:"display"`
			} `json:"provider"`
		}
		if json.Unmarshal(e.Resource, &c) != nil || c.ResourceType != "Claim" {
			continue
		}
		if c.Provider.Reference == "" {
			t.Fatalf("the Claim names its provider by display text alone (%q), which identifies nobody", c.Provider.Display)
		}
		return c.Provider.Reference
	}
	t.Fatal("the bundle carries no Claim")
	return ""
}

// bundleEntryAt returns the entry a reference resolves to, by fullUrl or by the
// entry's own relative identity — the two spellings a payer resolves.
func bundleEntryAt(t *testing.T, body []byte, ref string) ([]byte, bool) {
	t.Helper()
	var b struct {
		Entry []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.Entry {
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		_ = json.Unmarshal(e.Resource, &head)
		if e.FullURL == ref || head.ResourceType+"/"+head.ID == ref {
			return e.Resource, true
		}
	}
	return nil, false
}
