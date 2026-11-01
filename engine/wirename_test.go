//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// hubDecodeEnvelope decodes an envelope as a test's stand-in Hub receives
// it: like the network's Hub while wire protocols 1.1.0 and 2.0.0 share the
// network, it reads the leg's name in either form as the flat one.
func hubDecodeEnvelope(body []byte) (shnsdk.Envelope, error) {
	env, err := shnsdk.DecodeEnvelope(body)
	if err == nil {
		env.Metadata.TransactionType, _ = canonicalLeg(env.Metadata.TransactionType)
	}
	return env, err
}

// TestWireNames_EveryLegBothForms: every leg of every compiled-in workstream
// is named on the wire by its workstream prefix and its flat name, and either
// form names the leg; no form names two legs, and a name of neither form, a
// wrong prefix among them, names none.
func TestWireNames_EveryLegBothForms(t *testing.T) {
	prefix := map[string]string{} // each published leg's taxonomy workstream
	for _, tax := range []legtaxonomy.Taxonomy{legtaxonomy.PA(), legtaxonomy.FQS()} {
		for _, l := range tax.Legs {
			prefix[l.TransactionType] = tax.Workstream
		}
	}
	seen := map[string]string{}
	for _, w := range workstreams {
		for tx, s := range w.catalog() {
			p, dotted, _ := strings.Cut(s.WireName, ".")
			if want, ok := prefix[tx]; ok && p != want {
				t.Errorf("%s: wire name %q, want the %s workstream's prefix", tx, s.WireName, want)
			}
			if p == "" || dotted != tx || wireName(tx) != s.WireName {
				t.Errorf("%s: wire name %q (wireName %q), want a workstream prefix and %q", tx, s.WireName, wireName(tx), tx)
			}
			for _, form := range []string{tx, s.WireName} {
				if prev, ok := seen[form]; ok && prev != tx {
					t.Errorf("the name %q names both %q and %q", form, prev, tx)
				}
				seen[form] = tx
				if leg, ok := canonicalLeg(form); !ok || leg != tx {
					t.Errorf("canonicalLeg(%q) = %q, %v; want %q", form, leg, ok, tx)
				}
				if spec, ok := legSpecFor(form); !ok || spec.Op != s.Op {
					t.Errorf("legSpecFor(%q) = %+v, %v; want the leg %s", form, spec, ok, tx)
				}
			}
		}
	}
	for _, name := range []string{"", "pa.", "pa.federated-query", "fqs.pas-claim", "PA.pas-claim", "pa.pa.pas-claim", "pa.nowhere"} {
		if leg, ok := canonicalLeg(name); ok {
			t.Errorf("canonicalLeg(%q) = %q, want no leg", name, leg)
		}
		if _, ok := legSpecFor(name); ok {
			t.Errorf("legSpecFor(%q) found a leg", name)
		}
	}
}

// TestCatalogFromTaxonomy_RefusesAWrongWireName: a taxonomy row whose
// prefixed name is not its workstream's prefix and its transaction type stops
// the catalog, so a gateway cannot send a name the network does not route.
func TestCatalogFromTaxonomy_RefusesAWrongWireName(t *testing.T) {
	for _, staged := range []string{"", "pas-claim", "fqs.pas-claim", "pa.pas-claims"} {
		tx := legtaxonomy.PA()
		for i := range tx.Legs {
			if tx.Legs[i].TransactionType == "pas-claim" {
				tx.Legs[i].StagedName = staged
			}
		}
		if _, err := catalogFromTaxonomy(tx, "pa", paLegs); err == nil || !strings.Contains(err.Error(), "prefixed name") {
			t.Errorf("stagedName %q: err = %v, want the catalog refused", staged, err)
		}
	}
	if _, err := catalogFromTaxonomy(legtaxonomy.PA(), "pa", paLegs); err != nil {
		t.Fatalf("fixture: the published taxonomy is refused: %v", err)
	}
}

// TestInbound_EitherLegNameIsTheSameLeg: a request naming its leg flat or
// prefixed is the same leg at every level: the payer's answer is relayed the
// same, the answer goes out under the prefixed name, and the leg is recorded
// on the observer feed under its flat name.
func TestInbound_EitherLegNameIsTheSameLeg(t *testing.T) {
	t.Parallel()
	approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
	updateReq, _ := updateBundle(t)
	for _, level := range allLevels {
		for _, name := range []string{"pas-claim-update", "pa.pas-claim-update"} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.partner.respByPath[pasSubmitPath] = approved
				got := p.send(t, name, "", updateReq)
				if got.status != http.StatusOK || !bytes.Equal(got.body, approved) {
					t.Fatalf("the payer's answer must be relayed exactly, got %d %s", got.status, got.body)
				}
				if got.name != "pa.pas-claim-update" {
					t.Errorf("the answer names the leg %q, want the prefixed name", got.name)
				}
				p.mu.Lock()
				defer p.mu.Unlock()
				if len(p.unbound) != 1 || p.unbound[0].LegType != "pas-claim-update" {
					t.Fatalf("the feed carries %+v, want the leg under its flat name", p.unbound)
				}
			})
		}
	}
}

// TestInbound_NamesOfNoLegAreRefused: a name of neither form of a leg is
// refused as it always was, before any token work.
func TestInbound_NamesOfNoLegAreRefused(t *testing.T) {
	p := newLevelPayer(t, EnforcementNone)
	for _, name := range []string{"pa.federated-query-x", "pa.nowhere", "fqs.pas-claim"} {
		got := p.sendRaw(t, name, []byte(`{}`))
		if got.status != http.StatusBadRequest || !strings.Contains(string(got.body), "unknown transaction type") {
			t.Errorf("%s: %d %s, want 400 unknown transaction type", name, got.status, got.body)
		}
	}
}

// TestOriginate_SendsThePrefixedName: a leg this gateway originates goes out
// under its prefixed name, and the answer, whatever name the Hub gives it, is
// read on the gateway's own leg.
func TestOriginate_SendsThePrefixedName(t *testing.T) {
	cer := eligibilityRequest(t, "MBR-COVERED")
	answer := payersEligibilityAnswer("Patient/MBR-COVERED")
	env, rec, _ := runEligibilityIngress(t, EnforcementNone, cer, answer)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body)
	}
	env.substrate.mu.Lock()
	defer env.substrate.mu.Unlock()
	if len(env.substrate.sentNames) != 1 || env.substrate.sentNames[0] != "pa.coverage-eligibility" {
		t.Fatalf("the gateway sent %v, want [pa.coverage-eligibility]", env.substrate.sentNames)
	}
}

// sendRaw delivers body under the transaction-type name name, which names no
// leg the catalog can sign for: the token is the pas-claim leg's, and the
// answer is the gateway's bare refusal.
func (p *levelPayer) sendRaw(t *testing.T, name string, body []byte) payerAnswer {
	t.Helper()
	p.seq++
	clock := p.g.cfg.Clock()
	corr := fmt.Sprintf("corr-raw-%d", p.seq)
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: p.requester.ID, Recipient: p.g.cfg.HolderID, TransactionType: name,
		AuthorityFrame: "provider-tpo", Timestamp: clock.Format(time.RFC3339), CorrelationID: corr,
	}, body, p.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tok, err := json.Marshal(signTestToken(shnsdk.Token{
		Operation: "pas-submit", Subject: p.pci, Frame: "provider-tpo", Holder: p.requester.ID,
		CorrelationID: corr, Expiry: clock.Add(time.Hour), PayloadHash: sha256hexT(env.Ciphertext),
	}, p.authzPriv))
	if err != nil {
		t.Fatal(err)
	}
	env.Metadata.AuthzToken = string(tok)
	raw, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	as, err := json.Marshal(shnsdk.IssueAssertion("hub", p.g.cfg.HolderID, p.hubPriv, clock, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", bytes.NewReader(raw))
	r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(as))
	rec := httptest.NewRecorder()
	p.g.inboundRoute()(rec, r)
	return payerAnswer{status: rec.Code, body: rec.Body.Bytes(), corr: corr}
}
