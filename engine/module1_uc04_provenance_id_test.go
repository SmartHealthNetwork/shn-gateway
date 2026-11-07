//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// amendCorrelations stands between a fixture's gateway and its stub payer and
// records each pas-claim-update's correlation id, in the order sent.
type amendCorrelations struct {
	next  http.RoundTripper
	corrs []string
}

func (a *amendCorrelations) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/route") || req.Body == nil {
		return a.next.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	env, err := hubDecodeEnvelope(body)
	if err != nil || env.Metadata.TransactionType != "pas-claim-update" {
		return a.next.RoundTrip(req)
	}
	a.corrs = append(a.corrs, env.Metadata.CorrelationID)
	return a.next.RoundTrip(req)
}

// amendmentProvenanceIDIn is the id of the Provenance an amendment carries
// that attributes its DiagnosticReport.
func amendmentProvenanceIDIn(t *testing.T, bundle []byte) string {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.Entry {
		m, err := exactMembers(e.Resource)
		if err != nil || exactString(m, "resourceType") != "Provenance" {
			continue
		}
		if strings.Contains(string(m["target"]), "DiagnosticReport/") {
			return exactString(m, "id")
		}
	}
	t.Fatalf("the amendment carries no Provenance of its report: %s", bundle)
	return ""
}

// UC-04's Provenance of the provider's supplemental report has an id of its
// own for each amendment it is sent in: prov-<the amendment's correlation id>,
// and a 409's re-send, a new amendment under a new correlation id, gets a new
// one. Two amendments attributing one report never share a Provenance id.
func TestHandleUC04_TheProvenanceIDIsTheAmendments(t *testing.T) {
	site := amendmentSites[0]
	if site.name != "uc04" {
		t.Fatalf("amendmentSites[0] is %s, want uc04", site.name)
	}
	for _, conflicts := range []int{0, 1} {
		gw, stub := routeFixtureAtLine(t, site.opts, "2.2")
		// The payer answers the first conflicts amendments 409, a version
		// conflict, in its own framed answer, so the gateway re-sends.
		left := conflicts
		stub.overrideResponse = func(leg string, payload []byte) []byte {
			if leg != "pas-claim-update" || left == 0 {
				return payload
			}
			left--
			framed, err := shnsdk.EncodeHTTPFrame(http.StatusConflict, "application/fhir+json", []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"conflict"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			return framed
		}
		wire := &amendCorrelations{next: gw.cfg.Client.Transport}
		client := *gw.cfg.Client
		client.Transport = wire
		gw.cfg.Client = &client
		rec, _ := serveAmendment(t, gw, site.path, site.serve, func(map[string]json.RawMessage) {})
		sent := stub.requests["pas-claim-update"]
		if rec.Code != http.StatusOK || len(sent) != conflicts+1 || len(wire.corrs) != conflicts+1 {
			t.Fatalf("conflicts %d: %d %s, %d updates sent, corrs %v", conflicts, rec.Code, rec.Body.String(), len(sent), wire.corrs)
		}
		seen := map[string]bool{}
		for i, body := range sent {
			id := amendmentProvenanceIDIn(t, body)
			if want := amendmentProvenanceID(wire.corrs[i]); id != want || !strings.HasPrefix(id, "prov-") {
				t.Fatalf("conflicts %d, update %d (correlation %s): Provenance id %q, want %q", conflicts, i, wire.corrs[i], id, want)
			}
			if idShape.MatchString("prov-"+wire.corrs[i]) && id != "prov-"+wire.corrs[i] {
				t.Fatalf("conflicts %d, update %d: Provenance id %q, want prov-%s", conflicts, i, id, wire.corrs[i])
			}
			if seen[id] {
				t.Fatalf("conflicts %d: the re-sent amendment reused Provenance id %q", conflicts, id)
			}
			seen[id] = true
		}
	}
}
