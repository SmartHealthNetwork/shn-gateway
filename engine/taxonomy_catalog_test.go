package engine

import (
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
)

// TestPACatalog_MatchesTheLegTaxonomy: the engine's catalog carries exactly
// the taxonomy's routable legs, each with the taxonomy's frames, operations
// and scope. The taxonomy is the gateway's embedded copy of the network's
// leg taxonomy; the catalog's contract family and physics detail are the
// engine's own, joined on the transaction type.
// The FQS adapter's catalog is checked against its own file the same way.
func TestPACatalog_MatchesTheLegTaxonomy(t *testing.T) {
	for _, c := range []struct {
		name    string
		tx      legtaxonomy.Taxonomy
		catalog map[string]legSpec
		min     int
	}{
		{"pa", legtaxonomy.PA(), paCatalog, 8},
		{"fqs", legtaxonomy.FQS(), fqsAdapter.catalog(), 1},
	} {
		want := map[string]legSpec{}
		for _, l := range c.tx.Legs {
			if !l.Routable() {
				continue
			}
			want[l.TransactionType] = legSpec{
				ReqFrame: l.Request.Frame, RespFrame: l.Response.Frame,
				Op: l.Request.Operation, RespOp: l.Response.Operation, Scope: l.Scope,
			}
		}
		if len(want) < c.min {
			t.Fatalf("%s: %d routable taxonomy legs, want >= %d", c.name, len(want), c.min)
		}
		for tx, w := range want {
			got, ok := c.catalog[tx]
			if !ok {
				t.Errorf("%s: taxonomy leg %q has no catalog row", c.name, tx)
				continue
			}
			if got.ReqFrame != w.ReqFrame || got.RespFrame != w.RespFrame || got.Op != w.Op || got.RespOp != w.RespOp || got.Scope != w.Scope {
				t.Errorf("%s: catalog %q = %+v, taxonomy frames/operations/scope %+v", c.name, tx, got, w)
			}
			if got.Physics.Kind != KindRequestResponse {
				t.Errorf("%s: catalog %q physics %v, the taxonomy says request-response", c.name, tx, got.Physics.Kind)
			}
		}
		for tx := range c.catalog {
			if _, ok := want[tx]; !ok {
				t.Errorf("%s: catalog row %q is no routable taxonomy leg", c.name, tx)
			}
		}
	}
}

// TestCatalogFromTaxonomy_RefusesDrift: the PA catalog is the derivation of
// the taxonomy and the adapter's rows, and the derivation refuses any row the
// other side does not match.
func TestCatalogFromTaxonomy_RefusesDrift(t *testing.T) {
	got, err := catalogFromTaxonomy(legtaxonomy.PA(), "pa", paLegs)
	if err != nil || !maps.Equal(got, paCatalog) {
		t.Fatalf("the derivation is not the PA catalog: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*legtaxonomy.Taxonomy, map[string]legDetail)
		want   string
	}{
		"a routable taxonomy leg without an adapter row": {
			func(_ *legtaxonomy.Taxonomy, d map[string]legDetail) { delete(d, "pas-claim") },
			`taxonomy leg "pas-claim" has no adapter row`,
		},
		"an adapter row for no taxonomy leg": {
			func(_ *legtaxonomy.Taxonomy, d map[string]legDetail) { d["toy-leg"] = d["pas-claim"] },
			`adapter row "toy-leg" names no routable taxonomy leg`,
		},
		"an adapter row for a leg that is not routable": {
			// No leg of the network's taxonomy is a direct read today, so
			// the row builds one.
			func(tx *legtaxonomy.Taxonomy, d map[string]legDetail) {
				tx.Legs = append(tx.Legs, legtaxonomy.Leg{
					TransactionType: "bearer-read", StagedName: "pa.bearer-read", Physics: "direct-read",
					Request: legtaxonomy.Side{Frame: "patient-access", Roles: []string{"phg"}, Operation: "bearer-read"},
					Scope:   "patient-access-only", PayloadBinding: "forbidden",
				})
				d["bearer-read"] = d["pas-claim"]
			},
			`adapter row "bearer-read" names no routable taxonomy leg`,
		},
		"an adapter row for another workstream's leg": {
			func(_ *legtaxonomy.Taxonomy, d map[string]legDetail) {
				d["federated-query"] = fqsLegs["federated-query"]
			},
			`adapter row "federated-query" names no routable taxonomy leg`,
		},
		"a taxonomy leg that stops riding an envelope": {
			func(tx *legtaxonomy.Taxonomy, _ map[string]legDetail) {
				for i := range tx.Legs {
					if tx.Legs[i].TransactionType == "pas-claim-inquire" {
						tx.Legs[i].PayloadBinding = "forbidden"
					}
				}
			},
			`adapter row "pas-claim-inquire" names no routable taxonomy leg`,
		},
		"another workstream's taxonomy": {
			func(tx *legtaxonomy.Taxonomy, _ map[string]legDetail) { tx.Workstream = "toy" },
			`the taxonomy is workstream "toy"'s, not "pa"'s`,
		},
		"an adapter row with incomplete physics": {
			func(_ *legtaxonomy.Taxonomy, d map[string]legDetail) {
				d["pas-claim"] = legDetail{Physics: LegPhysics{Kind: KindRequestResponse}, Contract: "pa.pas"}
			},
			`leg "pas-claim": the adapter row's physics`,
		},
		"an adapter row whose physics kind is not the taxonomy's": {
			func(_ *legtaxonomy.Taxonomy, d map[string]legDetail) {
				row := d["crd-order-select"]
				row.Physics.Kind = KindNotification
				d["crd-order-select"] = row
			},
			`leg "crd-order-select" is request-response in the taxonomy, its adapter row says notification`,
		},
	} {
		tx, detail := legtaxonomy.PA(), maps.Clone(paLegs)
		tc.mutate(&tx, detail)
		if c, err := catalogFromTaxonomy(tx, "pa", detail); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: catalog %v, error %v; want an error containing %q", name, c, err, tc.want)
		}
	}
}

// TestPACatalog_SeamsAndOperationHeaders pins what the core reads from the PA
// adapter's rows instead of naming PA legs: each leg's seam, as the inbound
// dispatcher labelled it, and the one leg whose request frame may name an
// operation (the taxonomy's frame operations).
func TestPACatalog_SeamsAndOperationHeaders(t *testing.T) {
	wantSeam := map[string]string{
		"coverage-eligibility": "payer-native", "dtr-questionnaire-fetch": "payer-native",
		"crd-order-select": "payer-native", "crd-order-dispatch": "payer-native",
		"pas-claim": "payer-native", "pas-claim-update": "payer-native", "pas-claim-inquire": "payer-native",
		"patient-dtr": "phg-inbound",
	}
	if len(paCatalog) != len(wantSeam) {
		t.Fatalf("%d catalog legs, %d pinned", len(paCatalog), len(wantSeam))
	}
	for tx, spec := range paCatalog {
		if spec.Seam != wantSeam[tx] {
			t.Errorf("%s: seam %q, want %q", tx, spec.Seam, wantSeam[tx])
		}
		if spec.OperationHeader != (tx == "dtr-questionnaire-fetch") {
			t.Errorf("%s: OperationHeader %v, want it only on dtr-questionnaire-fetch", tx, spec.OperationHeader)
		}
	}
	// Rejection row: an adapter row that names no seam is refused.
	if _, err := catalogFromTaxonomy(legtaxonomy.PA(), "pa", func() map[string]legDetail {
		d := maps.Clone(paLegs)
		row := d["pas-claim"]
		row.Seam = ""
		d["pas-claim"] = row
		return d
	}()); err == nil || !strings.Contains(err.Error(), "names no seam") {
		t.Errorf("a row with no seam: %v, want it refused", err)
	}
}

// TestPA_OriginatedAnswerCheckIsCRDs: the PA adapter certifies the originated
// answer on the two CRD legs, against the CDS Hooks response rules, and on no
// other leg.
func TestPA_OriginatedAnswerCheckIsCRDs(t *testing.T) {
	for tx := range paCatalog {
		got := originatedAnswerCheckFor(tx)
		crd := tx == "crd-order-select" || tx == "crd-order-dispatch"
		if crd != (got != nil) {
			t.Errorf("%s: originated answer check %v, want one only on the CRD legs", tx, got != nil)
		}
		if crd && reflect.ValueOf(got).Pointer() != reflect.ValueOf(certifyCDSHooksAnswer).Pointer() {
			t.Errorf("%s: the check is not certifyCDSHooksAnswer", tx)
		}
	}
	if originatedAnswerCheckFor("no-such-leg") != nil {
		t.Error("an unknown leg has an originated answer check")
	}
}
