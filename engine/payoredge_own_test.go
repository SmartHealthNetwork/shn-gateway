package engine

// The payer-edge ownership rows: which inbound Coverage payor identities this
// gateway answers for. A payer publishes its identities itself, on the network
// feed, and the routing directory is many-to-many — a provider's request routes
// here on ANY of them. So the ownership check reads the identities this holder
// publishes, union the configured one; an identity published by another holder,
// or by nobody, is still refused.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// payorConformance is the payer identity this gateway carries in configuration.
var payorConformance = shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00300"}

// payorEHRAssigned is a SECOND identity the same payer publishes on the feed —
// an EHR-assigned payer id, in an entirely different namespace.
var payorEHRAssigned = shnsdk.PayerIdentifier{System: "http://example.org/fhir/ehr-assigned-payer-id", Value: "204"}

// payorUnpublished is named by nobody on the feed.
var payorUnpublished = shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00999"}

// ownFeed builds a registry the way the converged /holders feed populates one:
// each holder's operator-attested payer-identity claims under its own id.
func ownFeed(t *testing.T, holders map[string][]shnsdk.PayerIdentifier) shnsdk.Registry {
	t.Helper()
	reg := shnsdk.NewRegistry()
	for id, ids := range holders {
		reg.Set(id, shnsdk.RegistryEntry{ID: id, Role: "payer", PayerIDs: ids})
	}
	return reg
}

// publishing returns the two options a payer gateway boots with: the configured
// identity pair, and the live view of what this holder publishes on the feed.
// notes receives the one-time "cannot see my own feed entry" note.
func publishing(reg shnsdk.Registry, holderID string, notes *[]string) []NativeOption {
	return []NativeOption{
		WithPayorEdgeIdentity(payorConformance, payorMapped),
		WithPayorEdgePublishedIdentities(reg, holderID, func(s string) { *notes = append(*notes, s) }),
	}
}

// submitNaming is the network's PAS submit with its Coverage's routing payor
// naming p — the request a provider gateway routes here off the feed.
func submitNaming(t *testing.T, p shnsdk.PayerIdentifier) []byte {
	t.Helper()
	return expectEdits(t, unsignedPASSubmit(t),
		tokenEdit{pasCoverageInlineSystem, p.System},
		tokenEdit{pasCoverageInlineValue, p.Value},
	)
}

// restampedTo is submitNaming(p) as the payer's own system must receive it:
// the same bytes with the Coverage's payer identifier re-stamped to backend.
func restampedTo(t *testing.T, body []byte, backend shnsdk.PayerIdentifier) []byte {
	t.Helper()
	return expectEdits(t, body,
		tokenEdit{pasCoverageInlineSystem, backend.System},
		tokenEdit{pasCoverageInlineValue, backend.Value},
	)
}

// TestPayorEdgeOwn_PublishedIdentityIsOwned is the defect: a payer that
// publishes a second, EHR-assigned identity is routable to on it at the
// network and must be answerable on it at its own edge. Both published
// identities are accepted and both are re-stamped to the backend identity.
func TestPayorEdgeOwn_PublishedIdentityIsOwned(t *testing.T) {
	reg := ownFeed(t, map[string][]shnsdk.PayerIdentifier{
		"payer": {payorConformance, payorEHRAssigned},
	})
	for _, inbound := range []shnsdk.PayerIdentifier{payorConformance, payorEHRAssigned} {
		t.Run(inbound.Value, func(t *testing.T) {
			var notes []string
			body := submitNaming(t, inbound)
			n, p := pasSubmitResponder(t, publishing(reg, "payer", &notes)...)
			if res := handlePAS(t, n, body); res.Status != 0 {
				t.Fatalf("status %d: %s", res.Status, res.Message)
			}
			assertSent(t, p, restampedTo(t, body, payorMapped))
			if len(notes) != 0 {
				t.Fatalf("the gateway CAN see its own feed entry, yet noted %q", notes)
			}
		})
	}
}

// TestPayorEdgeOwn_SecondIdentityReachesTheBackendIdentity pins the reason the
// re-stamp is not optional: the payer's own system knows itself by the BACKEND
// identity alone and refuses anything else. A mapping that accepted the second
// published identity but skipped the re-stamp would 400 at the payer — exactly
// the failure the network-side fix must not trade for.
func TestPayorEdgeOwn_SecondIdentityReachesTheBackendIdentity(t *testing.T) {
	answer := fixturePASResponse(t,
		[]byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`), true)
	var seen shnsdk.PayerIdentifier
	// The payer's own system: it adjudicates for payorMapped and nothing else.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cds-services" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"services": stubCDSServices})
			return
		}
		body, _ := io.ReadAll(r.Body)
		seen = submittedCoveragePayor(body)
		if seen != payorMapped {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error",` +
				`"code":"processing","diagnostics":"this system adjudicates only for its own payer identity"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
	}))
	t.Cleanup(srv.Close)

	var notes []string
	reg := ownFeed(t, map[string][]shnsdk.PayerIdentifier{"payer": {payorConformance, payorEHRAssigned}})
	n := NewNativeResponder(srv.Client(), srv.URL, "order-sign", newCensusSoR(), fixedClock,
		publishing(reg, "payer", &notes)...)
	if res := handlePAS(t, n, submitNaming(t, payorEHRAssigned)); res.Status != 0 {
		t.Fatalf("the payer's own system refused the request: %d %s (it received payor %s|%s)",
			res.Status, res.Message, seen.System, seen.Value)
	}
	if seen != payorMapped {
		t.Fatalf("the payer's own system received payor %s|%s, want its own %s|%s",
			seen.System, seen.Value, payorMapped.System, payorMapped.Value)
	}
}

// submittedCoveragePayor reads the routing payor identifier off the first
// Coverage entry of a PAS request Bundle, the way the payer's own system
// decides whether the claim is addressed to it.
func submittedCoveragePayor(b []byte) shnsdk.PayerIdentifier {
	var bundle struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				Payor        []struct {
					Identifier shnsdk.PayerIdentifier `json:"identifier"`
				} `json:"payor"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if json.Unmarshal(b, &bundle) != nil {
		return shnsdk.PayerIdentifier{}
	}
	for _, e := range bundle.Entry {
		if e.Resource.ResourceType == "Coverage" && len(e.Resource.Payor) > 0 {
			return e.Resource.Payor[0].Identifier
		}
	}
	return shnsdk.PayerIdentifier{}
}

// TestPayorEdgeOwn_UnownedIdentityRefused is the rejection row: an inbound payor
// in NO owned identity is refused unchanged, and the refusal names what this
// gateway does own so the mismatch is diagnosable.
func TestPayorEdgeOwn_UnownedIdentityRefused(t *testing.T) {
	var notes []string
	reg := ownFeed(t, map[string][]shnsdk.PayerIdentifier{"payer": {payorConformance, payorEHRAssigned}})
	n, p := pasSubmitResponder(t, publishing(reg, "payer", &notes)...)
	res := handlePAS(t, n, submitNaming(t, payorUnpublished))
	assertRefused(t, res, p, http.StatusBadRequest, "does not match any of this gateway's own payer identities")
	for _, want := range []string{
		payorUnpublished.System + "|" + payorUnpublished.Value,
		payorConformance.System + "|" + payorConformance.Value,
		payorEHRAssigned.System + "|" + payorEHRAssigned.Value,
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("the refusal does not name %s: %s", want, res.Message)
		}
	}
}

// TestPayorEdgeOwn_AnotherHoldersIdentityRefused is the rejection row for the
// feed's other side: the same feed carries every participant's identities, and
// another holder's identity is NOT this gateway's to answer for.
func TestPayorEdgeOwn_AnotherHoldersIdentityRefused(t *testing.T) {
	var notes []string
	reg := ownFeed(t, map[string][]shnsdk.PayerIdentifier{
		"payer":       {payorConformance},
		"other-payer": {payorEHRAssigned},
	})
	n, p := pasSubmitResponder(t, publishing(reg, "payer", &notes)...)
	res := handlePAS(t, n, submitNaming(t, payorEHRAssigned))
	assertRefused(t, res, p, http.StatusBadRequest, "does not match this gateway's own payer identity")
	if strings.Contains(res.Message, "does not match any") {
		t.Fatalf("another holder's identity was counted as owned: %s", res.Message)
	}
}

// TestPayorEdgeOwn_NoFeedEntryFallsBackToConfigured: a gateway that cannot see
// its own entry on the feed (unreachable feed, registration not yet propagated)
// decides on the configured identity ALONE — never open — and says so once.
func TestPayorEdgeOwn_NoFeedEntryFallsBackToConfigured(t *testing.T) {
	var notes []string
	reg := ownFeed(t, map[string][]shnsdk.PayerIdentifier{"other-payer": {payorEHRAssigned}})
	opts := publishing(reg, "payer", &notes)

	n, p := pasSubmitResponder(t, opts...)
	body := submitNaming(t, payorConformance)
	if res := handlePAS(t, n, body); res.Status != 0 {
		t.Fatalf("the configured identity is still owned: %d %s", res.Status, res.Message)
	}
	assertSent(t, p, restampedTo(t, body, payorMapped))

	n2, p2 := pasSubmitResponder(t, opts...)
	res := handlePAS(t, n2, submitNaming(t, payorEHRAssigned))
	// The single-identity refusal, word for word as a one-identity payer has
	// always read it.
	assertRefused(t, res, p2, http.StatusBadRequest,
		"inbound Coverage payor "+payorEHRAssigned.System+"|"+payorEHRAssigned.Value+
			" does not match this gateway's own payer identity "+payorConformance.System+"|"+payorConformance.Value+
			"; refusing rather than adjudicating as a different payer")
	if len(notes) != 1 {
		t.Fatalf("the missing-feed-entry note fired %d times, want once: %q", len(notes), notes)
	}
	if !strings.Contains(notes[0], `holder "payer"`) {
		t.Fatalf("the note does not name the holder: %s", notes[0])
	}
}

// TestPayorEdgeOwn_RefusalListIsBounded: a payer publishing many identities
// still gets a refusal it can read — the list is capped and says how many more.
func TestPayorEdgeOwn_RefusalListIsBounded(t *testing.T) {
	published := []shnsdk.PayerIdentifier{payorConformance}
	for _, v := range []string{"201", "202", "203", "204", "205", "206"} {
		published = append(published, shnsdk.PayerIdentifier{System: payorEHRAssigned.System, Value: v})
	}
	var notes []string
	reg := ownFeed(t, map[string][]shnsdk.PayerIdentifier{"payer": published})
	n, p := pasSubmitResponder(t, publishing(reg, "payer", &notes)...)
	res := handlePAS(t, n, submitNaming(t, payorUnpublished))
	assertRefused(t, res, p, http.StatusBadRequest, "(+2 more)")
	if strings.Contains(res.Message, "|206") {
		t.Fatalf("the refusal dumped the whole directory: %s", res.Message)
	}
	if got := strings.Count(res.Message, payorEHRAssigned.System+"|"); got != payorEdgeOwnListMax-1 {
		t.Fatalf("the refusal named %d published identities, want %d: %s", got, payorEdgeOwnListMax-1, res.Message)
	}
}

// TestPayorEdgeOwn_PublishedIdentitiesReadLive: the feed is read per REQUEST,
// so an identity published after boot is owned without a restart — the same
// no-restart property feed-derived routing has. Both requests go through the
// SAME responder, which is what makes this a live-read row: a mapping that
// snapshotted the published set when its option was applied would answer the
// second request off the stale snapshot and stay red here.
func TestPayorEdgeOwn_PublishedIdentitiesReadLive(t *testing.T) {
	var notes []string
	reg := ownFeed(t, map[string][]shnsdk.PayerIdentifier{"payer": {payorConformance}})
	body := submitNaming(t, payorEHRAssigned)
	n, p := pasSubmitResponder(t, publishing(reg, "payer", &notes)...)

	assertRefused(t, handlePAS(t, n, body), p, http.StatusBadRequest, "does not match")

	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer",
		PayerIDs: []shnsdk.PayerIdentifier{payorConformance, payorEHRAssigned}})

	if res := handlePAS(t, n, body); res.Status != 0 {
		t.Fatalf("the newly published identity is not owned by the SAME responder: %d %s", res.Status, res.Message)
	}
	assertSent(t, p, restampedTo(t, body, payorMapped))
}
