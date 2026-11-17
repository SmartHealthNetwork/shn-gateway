package relay

import (
	"errors"
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// TestApplySignedContent_ContainedProvenanceTargetsItsContainer: in FHIR R4 a
// contained resource's reference "#" names the resource that contains it, so
// a signed Provenance a Task contains that targets "#" signs the Task. An
// edit anywhere in the Task is refused, the Provenance's own signature named
// in any case. "#" from a Provenance that is not contained names nothing,
// and the container's neighbours are not covered.
func TestApplySignedContent_ContainedProvenanceTargetsItsContainer(t *testing.T) {
	const sig = `{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z","who":{"reference":"Practitioner/1"}}`
	provenance := func(member string) string {
		return `{"resourceType":"Provenance","id":"p1","target":[{"reference":"#"}],"recorded":"2026-01-01T00:00:00Z",` +
			`"agent":[{"who":{"reference":"Practitioner/1"}}],"` + member + `":[` + sig + `]}`
	}
	task := func(member string) string {
		return `{"resourceType":"Task","id":"t1","status":"requested","intent":"order","contained":[` + provenance(member) + `],"description":"x"}`
	}
	str := []byte(`"changed"`)
	type row struct {
		name    string
		doc     string
		ops     func(*Document) []Op
		carrier string // "" = must be edited
	}
	var rows []row
	for _, m := range []string{"signature", "Signature", "SIGNATURE", "ſignature"} {
		rows = append(rows,
			row{"Task." + m + ": a member added to the Task", task(m), func(d *Document) []Op {
				return []Op{d.InsertMember(d.Root(), "output", []byte(`[]`))}
			}, "Provenance.signature"},
			row{"Task." + m + ": a value in the Task", task(m), func(d *Document) []Op {
				return []Op{d.Replace(at(t, d, "description"), str)}
			}, "Provenance.signature"},
			row{"Task." + m + ": the Task in a Bundle", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + task(m) + `},{"resource":{"resourceType":"Patient","id":"pt"}}]}`, func(d *Document) []Op {
				return []Op{d.Replace(at(t, d, "entry", "0", "resource", "description"), str)}
			}, "Provenance.signature"},
			// The container only: a neighbour of the Task is edited.
			row{"Task." + m + ": the Task's neighbour in a Bundle", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + task(m) + `},{"resource":{"resourceType":"Patient","id":"pt","gender":"x"}}]}`, func(d *Document) []Op {
				return []Op{d.Replace(at(t, d, "entry", "1", "resource", "gender"), str)}
			}, ""},
		)
	}
	// Not valid R4, read wide: a Provenance contained in a contained
	// resource, or in a contained array a resource's element holds, covers
	// the Task too.
	nested := `{"resourceType":"Task","id":"t1","status":"requested","intent":"order","contained":[{"resourceType":"Basic","id":"b1","code":{"text":"x"},"contained":[` + provenance("signature") + `]}],"description":"x"}`
	inInput := `{"resourceType":"Task","id":"t1","status":"requested","intent":"order","input":[{"type":{"text":"x"},"valueString":"y","contained":[` + provenance("SIGNATURE") + `]}],"description":"x"}`
	rows = append(rows,
		row{"nested contained: a value in the Task", nested, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "description"), str)}
		}, "Provenance.signature"},
		row{"nested contained, in a Bundle: the Task's neighbour", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + nested + `},{"resource":{"resourceType":"Patient","id":"pt","gender":"x"}}]}`, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "1", "resource", "gender"), str)}
		}, ""},
		row{"contained in a Task input: a value in the Task", inInput, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "description"), str)}
		}, "Provenance.signature"},
	)
	rows = append(rows,
		// "#" from a Provenance that is an entry, not contained, names nothing.
		row{"an entry Provenance's #: another entry", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + provenance("signature") + `},{"resource":{"resourceType":"Patient","id":"pt","gender":"x"}}]}`, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "1", "resource", "gender"), str)}
		}, ""},
		row{"an entry Provenance's #: the Bundle", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + provenance("signature") + `}]}`, func(d *Document) []Op {
			return []Op{d.InsertMember(d.Root(), "id", []byte(`"b"`))}
		}, ""},
		// Not the Provenance's own signature: a Signature shape under
		// another name covers the Provenance only, not its container.
		row{"Task.signatures: a value in the Task", task("signatures"), func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "description"), str)}
		}, ""},
	)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			b := NewBody([]byte(r.doc), OriginPeerFrame)
			p, err := Apply(b, fhirJSON, EditDTRCoverageObtain, r.ops(mustDoc(t, b))...)
			if r.carrier == "" {
				if err != nil {
					t.Fatalf("uncovered edit refused: %v", err)
				}
				if p.Ownership() != OwnershipEdited {
					t.Fatalf("want Edited, got %v", p)
				}
				return
			}
			var se *SignedContentError
			if !errors.As(err, &se) || !errors.Is(err, ErrSignedContent) {
				t.Fatalf("want *SignedContentError, got %v", err)
			}
			if se.Carrier != r.carrier {
				t.Fatalf("carrier %q, want %q", se.Carrier, r.carrier)
			}
		})
	}
}

// TestCoverageAgreesOnContainedHashTargets holds the indexed coverage to the
// reference (coverageEach) on every effect in documents built to reach each
// step of a contained Provenance's "#": a holder that is a resource or not,
// containment nested to three levels, a holder inside an element, a root
// that is a resource, a Bundle entry or neither, and the Provenance's own
// signature named in another case or not its own.
func TestCoverageAgreesOnContainedHashTargets(t *testing.T) {
	prov := func(member string) string {
		return `{"resourceType":"Provenance","` + member + `":[{}],"target":[{"reference":"#"}]}`
	}
	// holders wrap inner, a contained array's content, in a value holding it.
	holders := []func(inner string) string{
		func(inner string) string { return `{"resourceType":"Basic","contained":[` + inner + `],"v":1}` },
		func(inner string) string { return `{"contained":[` + inner + `],"v":1}` },
		func(inner string) string {
			return `{"resourceType":"Task","input":[{"contained":[` + inner + `],"v":1}],"v":1}`
		},
	}
	roots := []func(doc string) string{
		func(doc string) string { return doc },
		func(doc string) string {
			return `{"resourceType":"Bundle","entry":[{"resource":` + doc + `},{"resource":{"resourceType":"Patient","v":1}}]}`
		},
		func(doc string) string { return `{"x":` + doc + `,"v":1}` },
	}
	var docs []string
	for _, member := range []string{"signature", "SIGNATURE", "signatures"} {
		var build func(depth int, inner string)
		build = func(depth int, inner string) {
			for _, h := range holders {
				doc := h(inner)
				for _, root := range roots {
					docs = append(docs, root(doc))
				}
				if depth < 3 {
					build(depth+1, doc)
				}
			}
		}
		build(1, prov(member))
	}
	compared := 0
	for _, src := range docs {
		d, err := splice.Scan([]byte(src), splice.Limits{})
		if err != nil {
			t.Fatalf("%v\n%s", err, src)
		}
		ref, cov := coverageEach(d), coverage(d)
		for i := 0; i < d.Len(); i++ {
			s, e := d.Span(splice.NodeID(i))
			for _, eff := range []effect{{start: s, end: e}, {start: s, end: e, inside: true}} {
				if got, want := cov.hits(eff), hitsEach(ref, eff); !slices.Equal(got, want) {
					t.Fatalf("effect %+v: indexed %q, each %q\n%s", eff, got, want, src)
				}
				compared++
			}
		}
	}
	t.Logf("%d documents, %d effects compared", len(docs), compared)
}
