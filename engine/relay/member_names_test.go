package relay

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// A signed Provenance covers what its targets name. The members that place a
// resource and resolve a target are read by their names in any case, as the
// Provenance's own signature is: a resource's id, a Bundle entry's resource
// and fullUrl, a contained array, the Provenance's target and each target's
// reference. resourceType, the JSON format's fixed property, is read both
// ways: an object whose resourceType is named in another case is no resource
// to R4's types, so its signature is read by its shape and covers the
// resource enclosing it, and it is also a resource a target may name, and,
// as a Provenance with its own signature, covers its targets.

const memberNamesSig = `{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z","who":{"reference":"Practitioner/1"}}`

// memberNamesProvenance is a Provenance signed under signature whose target
// member (named target) holds one Reference (its reference member named
// reference) to ref; resourceType names its resourceType member.
func memberNamesProvenance(resourceType, target, reference, ref string) string {
	return `{"` + resourceType + `":"Provenance","id":"p1","` + target + `":[{"` + reference + `":"` + ref + `"}],` +
		`"recorded":"2026-01-01T00:00:00Z","agent":[{"who":{"reference":"Practitioner/1"}}],"signature":[` + memberNamesSig + `]}`
}

// memberNamesDoc is one document a signed Provenance covers a Task in, through
// the members it names, and the edit made to the Task.
type memberNamesDoc struct {
	name string
	doc  string
	edit []string // the path of the Task value replaced
}

// Each shape names its members; the exact-case forms are those of v1.14.0.
func byTypeAndID(target, reference, id string) memberNamesDoc {
	return memberNamesDoc{
		name: "type and id: " + target + "/" + reference + "/" + id,
		doc: `{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"resource":{"resourceType":"Task","` + id + `":"t1","status":"requested","intent":"order","description":"x"}},` +
			`{"resource":` + memberNamesProvenance("resourceType", target, reference, "Task/t1") + `}]}`,
		edit: []string{"entry", "0", "resource", "description"},
	}
}

func byFullURL(fullURL, res string) memberNamesDoc {
	return memberNamesDoc{
		name: "fullUrl: " + fullURL + "/" + res,
		doc: `{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"` + fullURL + `":"urn:uuid:7c1f5a0e-0b7e-4b1c-9f3e-1d2a3b4c5d6e","` + res + `":{"resourceType":"Task","status":"requested","intent":"order","description":"x"}},` +
			`{"resource":` + memberNamesProvenance("resourceType", "target", "reference", "urn:uuid:7c1f5a0e-0b7e-4b1c-9f3e-1d2a3b4c5d6e") + `}]}`,
		edit: []string{"entry", "0", res, "description"},
	}
}

// byTypeAndFullURL: a Task with no id, named by type and the id its fullUrl
// ends in.
func byTypeAndFullURL(fullURL string) memberNamesDoc {
	return memberNamesDoc{
		name: "type and fullUrl's id: " + fullURL,
		doc: `{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"` + fullURL + `":"https://payer.example/fhir/Task/t1","resource":{"resourceType":"Task","status":"requested","intent":"order","description":"x"}},` +
			`{"resource":` + memberNamesProvenance("resourceType", "target", "reference", "Task/t1") + `}]}`,
		edit: []string{"entry", "0", "resource", "description"},
	}
}

// byContainedID: a Task contains a Basic and a signed Provenance whose target
// "#c1" names the Basic; the Basic is edited.
func byContainedID(contained, id string) memberNamesDoc {
	return memberNamesDoc{
		name: "#id: " + contained + "/" + id,
		doc: `{"resourceType":"Task","id":"t1","status":"requested","intent":"order","` + contained + `":[` +
			`{"resourceType":"Basic","` + id + `":"c1","code":{"text":"x"}},` +
			memberNamesProvenance("resourceType", "target", "reference", "#c1") + `],"description":"x"}`,
		edit: []string{contained, "0", "code", "text"},
	}
}

// byContainer: a Task contains a signed Provenance whose target "#" names the
// Task.
func byContainer(contained, target, reference string) memberNamesDoc {
	return memberNamesDoc{
		name: "#: " + contained + "/" + target + "/" + reference,
		doc: `{"resourceType":"Task","id":"t1","status":"requested","intent":"order","` + contained + `":[` +
			memberNamesProvenance("resourceType", target, reference, "#") + `],"description":"x"}`,
		edit: []string{"description"},
	}
}

func TestApplySignedContent_ProvenanceMembersInAnyCase(t *testing.T) {
	type row struct {
		memberNamesDoc
		carrier string // "" = must be edited
	}
	var rows []row
	seen := map[string]bool{}
	covered := func(d memberNamesDoc) {
		// The exact-case form of each shape is given once.
		if !seen[d.name] {
			seen[d.name] = true
			rows = append(rows, row{d, "Provenance.signature"})
		}
	}
	edited := func(d memberNamesDoc) { rows = append(rows, row{d, ""}) }
	for _, n := range []string{"target", "Target", "TARGET", "tARGEt"} {
		covered(byTypeAndID(n, "reference", "id"))
		covered(byContainer("contained", n, "reference"))
	}
	for _, n := range []string{"reference", "Reference", "REFERENCE"} {
		covered(byTypeAndID("target", n, "id"))
		covered(byContainer("contained", "target", n))
	}
	for _, n := range []string{"id", "Id", "ID"} {
		covered(byTypeAndID("target", "reference", n))
		covered(byContainedID("contained", n))
	}
	for _, n := range []string{"fullUrl", "FullUrl", "FULLURL", "fullurl"} {
		covered(byFullURL(n, "resource"))
		covered(byTypeAndFullURL(n))
	}
	for _, n := range []string{"resource", "Resource", "RESOURCE", "reſource"} {
		covered(byFullURL("fullUrl", n))
	}
	for _, n := range []string{"contained", "Contained", "CONTAINED"} {
		covered(byContainedID(n, "id"))
		covered(byContainer(n, "target", "reference"))
	}
	// Another member is no such member: the Task is edited.
	edited(byTypeAndID("targets", "reference", "id"))
	edited(byTypeAndID("target", "references", "id"))
	edited(byTypeAndID("target", "reference", "ids"))
	edited(byFullURL("fullUrls", "resource"))
	edited(byContainedID("containeds", "id"))
	edited(byContainer("containeds", "target", "reference"))
	// resourceType is read both ways. Under another case the object is
	// still a Provenance whose signature covers its targets, wherever it
	// stands, and a resource a target names; its signature is also read by
	// its shape, covering the resource enclosing it.
	for _, n := range []string{"resourceType", "ResourceType", "RESOURCETYPE"} {
		rt := func(src string) string {
			return strings.Replace(src, `{"resourceType":"Provenance"`, `{"`+n+`":"Provenance"`, 1)
		}
		d := byTypeAndID("target", "reference", "id")
		d.name = "resourceType: " + n + ": the target, in the Bundle"
		d.doc = rt(d.doc)
		covered(d)
		// Not a target: an exactly typed Provenance covers its target only;
		// another reads its signature by its shape too, over the Bundle.
		other := memberNamesDoc{
			name: "resourceType: " + n + ": another entry",
			doc: rt(`{"resourceType":"Bundle","type":"collection","entry":[` +
				`{"resource":{"resourceType":"Patient","id":"x","gender":"x"}},` +
				`{"resource":` + memberNamesProvenance("resourceType", "target", "reference", "Task/t1") + `}]}`),
			edit: []string{"entry", "0", "resource", "gender"},
		}
		if n == "resourceType" {
			edited(other)
		} else {
			rows = append(rows, row{other, "Signature in Bundle"})
		}
		// A Provenance a Task contains, targeting another entry: the
		// shape reading covers only the Task.
		covered(memberNamesDoc{
			name: "resourceType: " + n + ": contained, targeting another entry",
			doc: rt(`{"resourceType":"Bundle","type":"collection","entry":[` +
				`{"resource":{"resourceType":"Patient","id":"x","gender":"x"}},` +
				`{"resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order","contained":[` +
				memberNamesProvenance("resourceType", "target", "reference", "Patient/x") + `]}}]}`),
			edit: []string{"entry", "0", "resource", "gender"},
		})
		// A Provenance in an inner Bundle, targeting an outer entry: the
		// shape reading covers only the inner Bundle.
		covered(memberNamesDoc{
			name: "resourceType: " + n + ": in an inner Bundle, targeting an outer entry",
			doc: rt(`{"resourceType":"Bundle","type":"collection","entry":[` +
				`{"resource":{"resourceType":"Patient","id":"x","gender":"x"}},` +
				`{"resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":` +
				memberNamesProvenance("resourceType", "target", "reference", "Patient/x") + `}]}}]}`),
			edit: []string{"entry", "0", "resource", "gender"},
		})
		// Nested contained resources with no resource read exactly above
		// them: the climb from "#" goes on through each contained one.
		nestedNoExact := memberNamesDoc{
			name: "resourceType: " + n + ": #, nested, no resource read exactly above",
			doc: `{"` + n + `":"Task","id":"u","description":"x","contained":[{"` + n + `":"Task","id":"v","contained":[` +
				memberNamesProvenance("resourceType", "target", "reference", "#") + `]}]}`,
			edit: []string{"description"},
		}
		covered(nestedNoExact)
		// Two "#" targets and then another: the containers are covered once,
		// and the target after them still is.
		twoHash := `{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"resource":{"resourceType":"Patient","id":"x","gender":"x"}},` +
			`{"resource":{"` + n + `":"Task","id":"u","description":"x","contained":[{"` + n + `":"Task","id":"v","contained":[` +
			strings.Replace(memberNamesProvenance("resourceType", "target", "reference", "#"), `"target":[{"reference":"#"}]`,
				`"target":[{"reference":"#"},{"reference":"#"},{"reference":"Patient/x"}]`, 1) + `]}]}}]}`
		covered(memberNamesDoc{name: "resourceType: " + n + ": two # targets, then another: the container", doc: twoHash, edit: []string{"entry", "1", "resource", "description"}})
		covered(memberNamesDoc{name: "resourceType: " + n + ": two # targets, then another: the other", doc: twoHash, edit: []string{"entry", "0", "resource", "gender"}})
		// The target's own resourceType named in another case: it is the
		// resource the Provenance's target names.
		covered(memberNamesDoc{
			name: "resourceType: " + n + ": the target's own",
			doc: `{"resourceType":"Bundle","type":"collection","entry":[` +
				`{"resource":{"` + n + `":"Task","id":"t1","status":"requested","intent":"order","description":"x"}},` +
				`{"resource":` + memberNamesProvenance("resourceType", "target", "reference", "Task/t1") + `}]}`,
			edit: []string{"entry", "0", "resource", "description"},
		})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if got := memberNamesCarrier(t, r.memberNamesDoc); got != r.carrier {
				t.Fatalf("carrier %q, want %q", got, r.carrier)
			}
		})
	}
}

// memberNamesCarrier applies d's edit and returns the carrier it is refused
// under, or "" when it is made.
func memberNamesCarrier(t *testing.T, d memberNamesDoc) string {
	t.Helper()
	b := NewBody([]byte(d.doc), OriginPeerFrame)
	doc := mustDoc(t, b)
	p, err := Apply(b, fhirJSON, EditDTRCoverageObtain, doc.Replace(at(t, doc, d.edit...), []byte(`"changed"`)))
	if err == nil {
		if p.Ownership() != OwnershipEdited {
			t.Fatalf("want Edited, got %v", p)
		}
		return ""
	}
	var se *SignedContentError
	if !errors.As(err, &se) || !errors.Is(err, ErrSignedContent) {
		t.Fatalf("want *SignedContentError, got %v", err)
	}
	return se.Carrier
}

// TestProvenanceMembersExactCaseAsInV114: every document whose members are
// named exactly is covered as shn-gateway v1.14.0 covered it, edit for edit.
// The v114 column was read from v1.14.0's relay on these documents.
func TestProvenanceMembersExactCaseAsInV114(t *testing.T) {
	for _, c := range []struct {
		memberNamesDoc
		v114 string
	}{
		{byTypeAndID("target", "reference", "id"), "Provenance.signature"},
		{byFullURL("fullUrl", "resource"), "Provenance.signature"},
		{byTypeAndFullURL("fullUrl"), "Provenance.signature"},
		{byContainedID("contained", "id"), "Provenance.signature"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memberNamesCarrier(t, c.memberNamesDoc); got != c.v114 {
				t.Fatalf("carrier %q, v1.14.0 %q", got, c.v114)
			}
		})
	}
}

// TestCoverageAgreesOnProvenanceMembersInAnyCase holds the indexed coverage
// to the reference (coverageEach) on every effect in every document above,
// each member under every case given, at the root and in a Bundle entry.
func TestCoverageAgreesOnProvenanceMembersInAnyCase(t *testing.T) {
	var docs []string
	for _, n := range []string{"target", "Target", "targets"} {
		docs = append(docs, byTypeAndID(n, "reference", "id").doc, byContainer("contained", n, "reference").doc)
	}
	for _, n := range []string{"reference", "REFERENCE", "references"} {
		docs = append(docs, byTypeAndID("target", n, "id").doc, byContainer("contained", "target", n).doc)
	}
	for _, n := range []string{"id", "ID", "ids"} {
		docs = append(docs, byTypeAndID("target", "reference", n).doc, byContainedID("contained", n).doc)
	}
	for _, n := range []string{"fullUrl", "FULLURL", "fullUrls"} {
		docs = append(docs, byFullURL(n, "resource").doc, byTypeAndFullURL(n).doc)
	}
	for _, n := range []string{"resource", "reſource", "resources"} {
		docs = append(docs, byFullURL("fullUrl", n).doc)
	}
	for _, n := range []string{"contained", "Contained", "containeds"} {
		docs = append(docs, byContainedID(n, "id").doc, byContainer(n, "target", "reference").doc)
	}
	for _, n := range []string{"resourceType", "ResourceType"} {
		for _, m := range []string{"resourceType", "ResourceType"} {
			docs = append(docs, `{"`+n+`":"Task","id":"u","description":"x","contained":[{"`+m+`":"Task","id":"v","contained":[`+
				memberNamesProvenance("resourceType", "target", "reference", "#")+`]}]}`)
		}
	}
	docs = append(docs, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"x","v":1}},`+
		`{"resource":{"ResourceType":"Task","contained":[{"resourceType":"Task","contained":[`+
		strings.Replace(memberNamesProvenance("resourceType", "target", "reference", "#"), `"target":[{"reference":"#"}]`,
			`"target":[{"reference":"#"},{"reference":"#"},{"reference":"Patient/x"}]`, 1)+`]}],"v":1}}]}`)
	for _, n := range []string{"ResourceType", "RESOURCETYPE"} {
		for _, src := range slices.Clone(docs) {
			docs = append(docs, strings.Replace(src, `"resourceType":"Provenance"`, `"`+n+`":"Provenance"`, 1),
				strings.Replace(src, `"resourceType":"Task"`, `"`+n+`":"Task"`, 1))
		}
	}
	for _, src := range slices.Clone(docs) {
		if strings.HasPrefix(src, `{"resourceType":"Task"`) {
			docs = append(docs, `{"resourceType":"Bundle","entry":[{"fullUrl":"https://payer.example/fhir/Task/t1","resource":`+src+`},{"resource":{"resourceType":"Patient","v":1}}]}`)
		}
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
