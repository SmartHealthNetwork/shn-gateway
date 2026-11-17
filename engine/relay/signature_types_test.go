package relay

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// sigCore is a Signature's required members, as R4 defines them.
const sigCore = `"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-11-15T00:00:00Z","who":{"reference":"Organization/o"}`

// shaped is an object with a Signature's member names and none of its
// member types.
const shaped = `{"type":0,"when":0,"who":0}`

// signaturesOf runs signatureElements over src and returns the path, from
// the root, of each Signature it finds, members by name and array elements
// by index, joined with dots.
func signaturesOf(t *testing.T, src string, find func(*splice.Doc, map[splice.NodeID]*resource) []bool) []string {
	t.Helper()
	d, err := splice.Scan([]byte(src), splice.Limits{})
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	return pathsOf(d, find(d, resourcesOf(d)))
}

// resourcesOf is coverage's map of the document's resources, by node.
func resourcesOf(d *splice.Doc) map[splice.NodeID]*resource {
	byNode := map[splice.NodeID]*resource{}
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		if typ := stringMember(d, n, "resourceType"); d.Kind(n) == splice.KindObject && typ != "" {
			byNode[n] = &resource{node: n, typ: typ}
		}
	}
	return byNode
}

func pathsOf(d *splice.Doc, sig []bool) []string {
	name := make([]string, d.Len())
	parent := parents(d)
	for i := 0; i < d.Len(); i++ {
		n := splice.NodeID(i)
		for _, m := range d.Members(n) {
			name[m.Value] = m.Name
		}
		for k, el := range d.Elems(n) {
			name[el] = strconv.Itoa(k)
		}
	}
	var out []string
	for i, s := range sig {
		if !s {
			continue
		}
		var p []string
		for n := splice.NodeID(i); n > 0; n = parent[n] {
			p = append(p, name[n])
		}
		slices.Reverse(p)
		out = append(out, strings.Join(p, "."))
	}
	return out
}

// TestSignatureElementsByElementType: a Signature is found at each place
// R4 types one (signature_gen.go), wherever the place lies (in a contained
// resource, a Bundle entry, a Parameters part, a StructureDefinition's
// elements, an extension at any depth), under a member's name in any case;
// an object merely shaped as a Signature, at a place R4 types as something
// else, is none; and where no R4 type describes the place (a member R4 does
// not define there, a resource of no R4 type, a value of another kind than
// its place's type, outside every resource), a Signature's shape still
// reads as one.
func TestSignatureElementsByElementType(t *testing.T) {
	sig := "{" + sigCore + "}"
	// Rows whose Signatures the shape misses: one lacking a member R4
	// requires, or names in another case than the shape's exact ones.
	shapeMissed := map[string]bool{
		"a partial Signature in an extension in a non-R4 resource":                   true,
		"a partial Signature in an extension of a place for a resource holding none": true,
		"an extension in another case in a non-R4 resource":                          true,
		"a name folding as a lenient reader folds it":                                true,
		"shaped, its members' names in another case":                                 true,
	}
	for name, c := range map[string]struct {
		src  string
		want []string
	}{
		// Each R4 place typed Signature.
		"Contract.signer.signature": {`{"resourceType":"Contract","signer":[{"type":{},"party":{},"signature":[` + sig + `,` + sig + `]}]}`,
			[]string{"signer.0.signature.0", "signer.0.signature.1"}},
		"VerificationResult's three": {`{"resourceType":"VerificationResult","attestation":{"proxySignature":` + sig + `,"sourceSignature":` + sig + `},"validator":[{"organization":{},"attestationSignature":` + sig + `}]}`,
			[]string{"attestation.proxySignature", "attestation.sourceSignature", "validator.0.attestationSignature"}},
		"Task.input and Task.output": {`{"resourceType":"Task","input":[{"type":{},"valueSignature":` + sig + `}],"output":[{"type":{},"valueSignature":` + sig + `}]}`,
			[]string{"input.0.valueSignature", "output.0.valueSignature"}},
		"Parameters.parameter and its parts": {`{"resourceType":"Parameters","parameter":[{"name":"a","valueSignature":` + sig + `,"part":[{"name":"b","part":[{"name":"c","valueSignature":` + sig + `}]}]}]}`,
			[]string{"parameter.0.valueSignature", "parameter.0.part.0.part.0.valueSignature"}},
		"ElementDefinition's four, in a snapshot and a differential": {`{"resourceType":"StructureDefinition","snapshot":{"element":[{"path":"X","defaultValueSignature":` + sig + `,"fixedSignature":` + sig + `}]},"differential":{"element":[{"path":"X","patternSignature":` + sig + `,"example":[{"label":"e","valueSignature":` + sig + `}]}]}}`,
			[]string{"snapshot.element.0.defaultValueSignature", "snapshot.element.0.fixedSignature", "differential.element.0.patternSignature", "differential.element.0.example.0.valueSignature"}},
		"StructureMap's rule source, in a nested rule": {`{"resourceType":"StructureMap","group":[{"rule":[{"rule":[{"source":[{"context":"s","defaultValueSignature":` + sig + `}]}]}]}]}`,
			[]string{"group.0.rule.0.rule.0.source.0.defaultValueSignature"}},
		// An extension holds Extensions wherever it is.
		"an extension on a resource":                        {`{"resourceType":"Patient","extension":[{"url":"u","valueSignature":` + sig + `}]}`, []string{"extension.0.valueSignature"}},
		"a modifier extension on an element":                {`{"resourceType":"Task","input":[{"modifierExtension":[{"url":"u","valueSignature":` + sig + `}]}]}`, []string{"input.0.modifierExtension.0.valueSignature"}},
		"an extension in an extension":                      {`{"resourceType":"QuestionnaireResponse","item":[{"linkId":"1","extension":[{"url":"u","extension":[{"url":"v","valueSignature":` + sig + `}]}]}]}`, []string{"item.0.extension.0.extension.0.valueSignature"}},
		"an extension on a primitive":                       {`{"resourceType":"Patient","birthDate":"2000-01-01","_birthDate":{"extension":[{"url":"u","valueSignature":` + sig + `}]}}`, []string{"_birthDate.extension.0.valueSignature"}},
		"an extension on a Signature's who":                 {`{"resourceType":"Provenance","signature":[{` + sigCore[:strings.Index(sigCore, `"who"`)] + `"who":{"extension":[{"url":"u","valueSignature":` + sig + `}]}}]}`, []string{"signature.0.who.extension.0.valueSignature"}},
		"an extension in another case in a non-R4 resource": {`{"resourceType":"Thing","EXTENSION":[{"url":"u","valueSignature":{"who":{}}}]}`, []string{"EXTENSION.0.valueSignature"}},
		// A resource where R4 types none is untyped, so its own members are
		// too: a partial Signature there is not read by its place.
		"a resource at a data type's place, holding a partial Signature": {`{"resourceType":"Task","input":[{"resourceType":"Contract","signer":[{"signature":[{"who":{}}]}]}]}`, nil},
		"a partial Signature in an extension in a non-R4 resource":       {`{"resourceType":"Thing","extension":[{"url":"u","valueSignature":{"who":{}}}]}`, []string{"extension.0.valueSignature"}},
		"a name folding as a lenient reader folds it":                    {`{"resourceType":"Patient","extenſion":[{"url":"u","valueSignature":{"who":{}}}]}`, []string{"extenſion.0.valueSignature"}},
		"shaped, its members' names in another case":                     {`{"resourceType":"Thing","x":{"Type":0,"WHEN":0,"wHo":0}}`, []string{"x"}},
		"an extension in a non-R4 resource":                              {`{"resourceType":"Thing","x":{"extension":[{"url":"u","valueSignature":` + sig + `}]}}`, []string{"x.extension.0.valueSignature"}},
		"an extension outside every resource":                            {`{"data":{"extension":[{"url":"u","valueSignature":` + sig + `}]}}`, []string{"data.extension.0.valueSignature"}},
		// Resources inside resources start their own paths.
		"a contained Provenance's own signature, and a contained Contract's": {`{"resourceType":"Task","contained":[{"resourceType":"Provenance","signature":[` + sig + `]},{"resourceType":"Contract","signer":[{"signature":[` + sig + `]}]}]}`,
			[]string{"contained.1.signer.0.signature.0"}},
		"a signed Bundle's entry": {`{"resourceType":"Bundle","signature":` + sig + `,"entry":[{"resource":{"resourceType":"Task","input":[{"valueSignature":` + sig + `}]}}]}`,
			[]string{"entry.0.resource.input.0.valueSignature"}},
		"a parameter's resource": {`{"resourceType":"Parameters","parameter":[{"name":"r","resource":{"resourceType":"Contract","signer":[{"signature":[` + sig + `]}]}}]}`,
			[]string{"parameter.0.resource.signer.0.signature.0"}},
		// A Bundle's and a Provenance's own signature is their carrier, not
		// a second Signature.
		"a Bundle's and a Provenance's own": {`{"resourceType":"Bundle","signature":` + sig + `,"entry":[{"resource":{"resourceType":"Provenance","signature":[` + sig + `]}}]}`, nil},
		// A member's name in another case: a lenient reader takes it.
		"names in another case": {`{"resourceType":"Patient","Extension":[{"url":"u","ValueSignature":` + sig + `}],"modifierextension":[{"url":"u","valuesignature":` + sig + `}]}`,
			[]string{"Extension.0.ValueSignature", "modifierextension.0.valuesignature"}},
		// Shaped as a Signature, at a place R4 types as something else.
		"shaped, as an element of R4's own":      {`{"resourceType":"AuditEvent","agent":[{"type":{},"who":{},"requestor":true,"when":"x"}]}`, nil},
		"shaped, at a choice of another type":    {`{"resourceType":"Task","input":[{"type":{},"valueReference":` + shaped + `}],"extension":[{"url":"u","valueCodeableConcept":` + shaped + `}]}`, nil},
		"shaped, at a data type's place":         {`{"resourceType":"Patient","name":[` + shaped + `],"maritalStatus":` + shaped + `,"meta":` + shaped + `}`, nil},
		"a Signature's place, holding no object": {`{"resourceType":"Contract","signer":[{"signature":["x",1,null]}],"extension":[{"url":"u","valueSignature":"x"}]}`, nil},
		// Where no R4 type describes the place, the shape reads as one.
		"shaped, in a member R4 does not define":                                     {`{"resourceType":"Patient","x":` + shaped + `,"name":[{"y":` + shaped + `}]}`, []string{"x", "name.0.y"}},
		"a signature member of a resource R4 gives none":                             {`{"resourceType":"Parameters","signature":` + sig + `}`, []string{"signature"}},
		"under members merely named like an extension":                               {`{"resourceType":"Patient","ext":[{"url":"u","valueSignature":` + sig + `}],"extensions":[{"valueSignature":` + sig + `}]}`, []string{"ext.0.valueSignature", "extensions.0.valueSignature"}},
		"an undefined member in an extension":                                        {`{"resourceType":"Patient","extension":[{"url":"u","x":` + sig + `}]}`, []string{"extension.0.x"}},
		"an object at a primitive's place":                                           {`{"resourceType":"Patient","gender":` + shaped + `}`, []string{"gender"}},
		"shaped, at a Bundle entry's place for a resource":                           {`{"resourceType":"Bundle","entry":[{"resource":{"type":0,"when":0,"who":0}}]}`, []string{"entry.0.resource"}},
		"shaped, at the root, no resource":                                           {`{"type":0,"when":0,"who":0}`, []string{""}},
		"a partial Signature in an extension of a place for a resource holding none": {`{"resourceType":"Task","contained":[{"extension":[{"url":"u","valueSignature":{"who":{}}}]}]}`, []string{"contained.0.extension.0.valueSignature"}},
		"a place for a resource holding none":                                        {`{"resourceType":"Task","contained":[{"signer":[{"signature":[` + sig + `]}]}]}`, []string{"contained.0.signer.0.signature.0"}},
		"a resource at a data type's place":                                          {`{"resourceType":"Contract","signer":[{"resourceType":"Patient","signature":[` + sig + `]}]}`, []string{"signer.0.signature.0"}},
		"an extension's undefined member, outside every resource":                    {`{"data":{"extension":{"s":` + sig + `}}}`, []string{"data.extension.s"}},
		"shaped, in a non-R4 resource":                                               {`{"resourceType":"Thing","x":` + shaped + `}`, []string{"x"}},
		"shaped, with _when, outside every resource":                                 {`{"data":[{"type":0,"_when":{},"who":0}]}`, []string{"data.0"}},
		"shaped, in members R4 does not define, of a non-R4 resource and an R4 one":  {`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Thing","s":` + shaped + `}},{"resource":{"resourceType":"Task","s":` + shaped + `}}]}`, []string{"entry.0.resource.s", "entry.1.resource.s"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := signaturesOf(t, c.src, signatureElements)
			if !slices.Equal(got, c.want) {
				t.Errorf("found %q, want %q\n%s", got, c.want, c.src)
			}
			// The shape found every Signature the types find with a
			// Signature's members, by their exact names: reading types only
			// drops what is merely shaped as one. One lacking a member R4
			// requires, or naming one in another case, the shape missed.
			shape := signaturesOf(t, c.src, shapeSignatures)
			for _, p := range got {
				if !slices.Contains(shape, p) && !shapeMissed[name] {
					t.Errorf("by type %s, which the shape did not find (%q)", p, shape)
				}
			}
		})
	}
}

// TestCoverageCarriersAreBounded: a carrier names its resource's type only
// when that is an R4 resource type, so however many types a peer's message
// makes up, its Signatures share one carrier ("Signature"), and the
// carriers an exempt edit discloses are at most one per R4 resource type
// and three more.
func TestCoverageCarriersAreBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"resourceType":"Bundle","signature":{` + sigCore + `},"entry":[`)
	types := []string{"Provenance"}
	for typ := range r4ResourceTypes {
		types = append(types, typ)
	}
	for i := range 2000 {
		types = append(types, typeName(i), "X"+typeName(i))
	}
	slices.Sort(types)
	for i, typ := range types {
		if i > 0 {
			b.WriteByte(',')
		}
		sig := `"signature":[{` + sigCore + `}],`
		if typ != "Provenance" {
			sig = `"extension":[{"url":"u","valueSignature":{` + sigCore + `}}],`
		}
		b.WriteString(`{"resource":{"resourceType":"` + typ + `",` + sig + `"id":"1"}}`)
	}
	b.WriteString(`]}`)
	d, err := splice.Scan([]byte(b.String()), splice.Limits{MaxBytes: 1 << 24, MaxTokens: 1 << 20, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	s, e := d.Span(d.Root())
	got := coverage(d).hits(effect{start: s, end: e})
	if bound := len(r4ResourceTypes) + 3; len(got) > bound {
		t.Fatalf("%d carriers, more than %d", len(got), bound)
	}
	for _, c := range got {
		typ, named := strings.CutPrefix(c, "Signature in ")
		if named && !r4ResourceTypes[typ] {
			t.Errorf("carrier %q names a type that is not R4's", c)
		}
	}
	for _, want := range []string{"Bundle.signature", "Signature", "Signature in Patient", "Signature in Task"} {
		if !slices.Contains(got, want) {
			t.Errorf("carriers %v lack %q", got, want)
		}
	}
	// Provenance's own signature covers its targets (none here) and itself,
	// under its own carrier, never "Signature in Provenance".
	if slices.Contains(got, "Signature in Provenance") || !slices.Contains(got, "Provenance.signature") {
		t.Errorf("carriers %v: a Provenance's own signature is Provenance.signature", got)
	}
}

// signedCorpora are this module's own real messages that a test reads:
// the relay-fidelity corpus and the engine's fixtures.
var signedCorpora = []string{
	filepath.Join("internal", "splice", "testdata", "relayfidelity"),
	filepath.Join("..", "testdata"),
}

// TestSignatureElementsAgreeWithTheShapeOnRealMessages: on every JSON
// message in this module's corpora, every Signature the shape found is
// found by its element's type, so reading types loses no real Signature;
// one the types alone find sits at a place R4 types Signature and lacks a
// member R4 requires of one (an empty valueSignature), so the shape missed
// it. On the rows of TestSignatureElementsByElementType, the shape finds
// every Signature the types find, and more only at places no Signature can
// be.
func TestSignatureElementsAgreeWithTheShapeOnRealMessages(t *testing.T) {
	files, signed, partial := 0, 0, 0
	for _, root := range signedCorpora {
		err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || filepath.Ext(path) != ".json" {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			d, err := splice.Scan(b, splice.Limits{MaxBytes: 1 << 24, MaxTokens: 1 << 20, MaxDepth: 64})
			if err != nil {
				return nil // a corpus may hold a message that is not JSON, or exceeds a limit, on purpose
			}
			files++
			byNode := resourcesOf(d)
			bySig, byShape := signatureElements(d, byNode), shapeSignatures(d, byNode)
			for i := range bySig {
				n := splice.NodeID(i)
				switch {
				case byShape[i] && !bySig[i]:
					t.Errorf("%s: %q is a Signature by its shape, not by its type", path, pathsOf(d, onlyNode(d, n)))
				case bySig[i] && !byShape[i] && hasMember(d, n, "type") && (hasMember(d, n, "when") || hasMember(d, n, "_when")) && hasMember(d, n, "who"):
					t.Errorf("%s: %q has a Signature's members, yet the shape missed it", path, pathsOf(d, onlyNode(d, n)))
				case bySig[i] && !byShape[i]:
					// At a place R4 types Signature, lacking a member
					// R4 requires of one: a Signature still, which the
					// shape missed.
					partial++
				}
			}
			if slices.Contains(bySig, true) {
				signed++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 150 || signed < 5 {
		t.Fatalf("read %d messages, %d of them holding a Signature: the corpora moved", files, signed)
	}
	t.Logf("%d messages, %d holding a Signature: the types find every Signature the shape finds, and %d more lacking a member R4 requires", files, signed, partial)
}

// onlyNode is a Signature set holding n alone.
func onlyNode(d *splice.Doc, n splice.NodeID) []bool {
	s := make([]bool, d.Len())
	s[n] = true
	return s
}

// TestFoldNameIsEqualFold: the walk folds member names exactly as the
// splice scanner and strings.EqualFold do, so a name a lenient reader takes
// for a member R4 defines is read as that member.
func TestFoldNameIsEqualFold(t *testing.T) {
	names := []string{"extension", "Extension", "EXTENSION", "extenſion", "EXTENſION", "extensiön", "valueSignature", "VALUESIGNATURE", "valueſignature",
		"kind", "Kind", "KIND", "who", "WHO", "wHo", "when", "_when", "_WHEN", "type", "TYPE", "ß", "ẞ", "ss", "σ", "ς", "Σ", ""}
	for _, a := range names {
		for _, b := range names {
			if got, want := foldName(a) == foldName(b), strings.EqualFold(a, b); got != want {
				t.Errorf("%q and %q: fold alike %v, EqualFold %v", a, b, got, want)
			}
		}
	}
}

// TestOwnSignatureInAnyCaseIsItsCarrier: a Bundle's or a Provenance's own
// signature, its member named in any case, is that resource's carrier
// (Bundle.signature, Provenance.signature), never dropped. Against
// v1.14.0, which read a Signature by its members' names alone: every edit
// v1.14.0 found signed is found signed here, under the carrier the resource
// names for its own signature; a case-variant Provenance signature also
// covers its targets, which v1.14.0 left unsigned.
func TestOwnSignatureInAnyCaseIsItsCarrier(t *testing.T) {
	sig := `{` + sigCore + `}`
	task := `{"resource":{"resourceType":"Task","id":"t","status":"draft"}}`
	for _, c := range []struct {
		name, doc string
		at        []string // the edit, besides one over the whole document
		v114, now []string // carriers over the whole document, then at
	}{
		{"Bundle with signature", `{"resourceType":"Bundle","signature":` + sig + `,"type":"collection","entry":[` + task + `]}`,
			[]string{"entry", "0", "resource", "status"}, []string{"Bundle.signature", "Bundle.signature"}, []string{"Bundle.signature", "Bundle.signature"}},
		{"Bundle with SIGNATURE", `{"resourceType":"Bundle","SIGNATURE":` + sig + `,"type":"collection","entry":[` + task + `]}`,
			[]string{"entry", "0", "resource", "status"}, []string{"Signature in Bundle", "Signature in Bundle"}, []string{"Bundle.signature", "Bundle.signature"}},
		{"Provenance with Signature", `{"resourceType":"Provenance","Signature":[` + sig + `],"target":[{"reference":"Task/t"}]}`,
			[]string{"target", "0", "reference"}, []string{"Signature in Provenance", "Signature in Provenance"}, []string{"Provenance.signature", "Provenance.signature"}},
		{"Contract with signer Signature", `{"resourceType":"Contract","signer":[{"Signature":[` + sig + `]}],"status":"x"}`,
			[]string{"status"}, []string{"Signature in Contract", "Signature in Contract"}, []string{"Signature in Contract", "Signature in Contract"}},
		{"Provenance with Signature, its target beside it", `{"resourceType":"Bundle","type":"collection","entry":[` + task + `,{"resource":{"resourceType":"Provenance","Signature":[` + sig + `],"target":[{"reference":"Task/t"}]}}]}`,
			[]string{"entry", "0", "resource", "status"}, []string{"Signature in Provenance", ""}, []string{"Provenance.signature", "Provenance.signature"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, err := splice.Scan([]byte(c.doc), splice.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			n := d.Root()
			for _, p := range c.at {
				v, ok := d.Member(n, p)
				if !ok {
					k, _ := strconv.Atoi(p)
					v = d.Elems(n)[k]
				}
				n = v
			}
			s, e := d.Span(d.Root())
			as, ae := d.Span(n)
			cov := coverage(d)
			got := []string{strings.Join(cov.hits(effect{start: s, end: e}), ","), strings.Join(cov.hits(effect{start: as, end: ae}), ",")}
			if !slices.Equal(got, c.now) {
				t.Fatalf("carriers %q, want %q", got, c.now)
			}
			for i := range got {
				if c.v114[i] != "" && got[i] == "" {
					t.Fatalf("v1.14.0 found %q signed, now unsigned", c.v114[i])
				}
			}
			if ref := hitsEach(coverageEach(d), effect{start: as, end: ae}); strings.Join(ref, ",") != got[1] {
				t.Fatalf("reference coverage %q, indexed %q", ref, got[1])
			}
		})
	}
	// The member's name in other cases (a long s, U+017F, folds to s), at
	// the root and where a Parameters part holds the resource: the
	// resource's own signature, under its carrier, never also a Signature in
	// it, as the reference coverage finds too. A member R4 does not define
	// there, shaped as a Signature, is one in its resource, as it was.
	for _, typ := range []string{"Bundle", "Provenance"} {
		for _, name := range []string{"signature", "Signature", "SIGNATURE", "sIGNATURe", "\u017fignature", "signatures"} {
			res := `{"resourceType":"` + typ + `","` + name + `":` + sig + `,"id":"x"}`
			want := typ + ".signature"
			if name == "signatures" {
				want = "Signature in " + typ
			}
			for _, doc := range []string{res, `{"resourceType":"Parameters","parameter":[{"name":"p","resource":` + res + `}]}`} {
				d, err := splice.Scan([]byte(doc), splice.Limits{})
				if err != nil {
					t.Fatal(err)
				}
				s, e := d.Span(d.Root())
				got := coverage(d).hits(effect{start: s, end: e})
				if !slices.Equal(got, []string{want}) {
					t.Errorf("%s: carriers %q, want [%s]", doc, got, want)
				}
				if ref := hitsEach(coverageEach(d), effect{start: s, end: e}); !slices.Equal(ref, got) {
					t.Errorf("%s: reference coverage %q, indexed %q", doc, ref, got)
				}
			}
		}
	}
}
