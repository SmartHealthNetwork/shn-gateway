package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The core's half of the canonical names' tests (canonicalnames.go): the strict
// and the binding's views, the one renaming, and the binding's readers
// (subject_bind.go) over them. It runs in the core-only build too. The PA zone's
// half, the involved list's request readers on each PA leg, is
// workstream_pa_involved_folded_test.go.

// bindingLevels are the conformance levels; the binding reads alike at each.
var bindingLevels = []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict}

// holdsNoOne is a system of record that holds no member, so every binding
// derives its member's identifier from the request.
type holdsNoOne struct{ SystemOfRecord }

func (holdsNoOne) ResolvePatient(string) (string, Demo, bool) { return "", Demo{}, false }

// noOneHeld is a gateway whose system of record holds no member.
func noOneHeld(level ConformanceEnforcement) *Gateway {
	return &Gateway{cfg: Config{SoR: holdsNoOne{}, ConformanceEnforcement: level}}
}

// The strict view is the one the leg's deciding readers read: a body that is not
// one JSON document, repeats a member name exactly or in another case anywhere, or
// is past the relay's limits is no view, an error and never an empty one.
func TestStrictView_AnUnreadableRequestIsAnError(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 65) + `1` + strings.Repeat(`}`, 65)
	for name, body := range map[string]string{
		"not JSON":                                 `not json`,
		"two documents":                            `{"resourceType":"Bundle"} {}`,
		"a name repeated exactly":                  `{"resourceType":"Patient","id":"a","id":"b"}`,
		"a name repeated in another case":          `{"resourceType":"Patient","id":"a","ID":"b"}`,
		"a vendor's name repeated in another case": `{"hook":"order-select","extension":{"x":1,"X":2}}`,
		"nested past the relay's bound":            deep,
		"a byte that is not well-formed UTF-8":     "{\"resourceType\":\"Patient\",\"id\":\"a\xff\"}",
	} {
		if v, err := strictView([]byte(body)); v.doc != nil || !errors.Is(err, errRequestPatientsUnreadable) {
			t.Errorf("%s: view %v, err %v, want errRequestPatientsUnreadable", name, v, err)
		}
	}
}

// The binding's view decodes as the binding always has (encoding/json, past the
// relay's own limits), with numbers as written, and refuses exactly where it
// refused: not one JSON document, a name repeated exactly anywhere, or in another
// case within a FHIR resource (resourceRepeatsAName). A vendor's name spelled two
// ways outside any resource is read and left as sent.
func TestBindingView_ReadsAndRefusesAsTheBindingDid(t *testing.T) {
	deep := `{"resourceType":"Patient","id":"a","x":` + strings.Repeat(`[`, 70) + `1` + strings.Repeat(`]`, 70) + `}`
	for name, body := range map[string]string{
		"not JSON":                        `not json`,
		"two documents":                   `{"resourceType":"Bundle"} {}`,
		"a name repeated exactly":         `{"hook":"order-select","extension":{"x":1,"x":2}}`,
		"a name repeated in another case": `{"prefetch":{"patient":{"resourceType":"Patient","id":"a","ID":"b"}}}`,
	} {
		if v, err := bindingView([]byte(body)); v.doc != nil || !errors.Is(err, errRequestPatientsUnreadable) {
			t.Errorf("%s: view %v, err %v, want errRequestPatientsUnreadable", name, v, err)
		}
	}
	for name, body := range map[string]string{
		"nested past the relay's bound":        deep,
		"a byte that is not well-formed UTF-8": "{\"resourceType\":\"Patient\",\"id\":\"a\",\"note\":\"\xff\"}",
		"a lone surrogate escape":              `{"resourceType":"Patient","id":"a","note":"\ud800"}`,
	} {
		if _, err := bindingView([]byte(body)); err != nil {
			t.Errorf("%s: %v, want read as the binding read it", name, err)
		}
	}
	v, err := bindingView([]byte(`{"hook":"order-select","Contained":1,"extension":{"Id":"x","id":"y"},"prefetch":{"patient":{"resourceType":"patient","Id":"p","valueDecimal":1e400}}}`))
	if err != nil {
		t.Fatal(err)
	}
	doc := v.doc.(map[string]any)
	if ext := doc["extension"].(map[string]any); ext["Id"] != "x" || ext["id"] != "y" {
		t.Fatalf("a vendor object's members were renamed: %v", ext)
	}
	if _, ok := doc["Contained"]; !ok {
		t.Fatal("a member outside every resource was renamed")
	}
	p := doc["prefetch"].(map[string]any)["patient"].(map[string]any)
	if p["resourceType"] != "Patient" || p["id"] != "p" || p["valueDecimal"] != json.Number("1e400") {
		t.Fatalf("the resource reads %v, want its names and type spelled as FHIR does and the decimal as written", p)
	}
}

// One function renames, inside FHIR resources only: an object with a resourceType
// in any case and everything within it, the next resource nested in it being one
// of its own. A name it does not list, or a type it does not compare, is left as
// sent.
func TestCanonicalNames_RenamesInsideResourcesOnly(t *testing.T) {
	v, err := strictView([]byte(`{"Entry":[],"context":{"Reference":"x"},"draftOrders":{"RESOURCETYPE":"bundle","ENTRY":[{"FullUrl":"urn:uuid:1","Resource":{"resourceType":"OBSERVATION","Subject":{"Reference":"Patient/p"},"Extension":[{"valueReference":{"Reference":"Patient/q"}}],"BirthDate":"x"}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(v.doc)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Entry":[],"context":{"Reference":"x"},"draftOrders":{"entry":[{"fullUrl":"urn:uuid:1","resource":{"BirthDate":"x","Extension":[{"valueReference":{"reference":"Patient/q"}}],"Subject":{"reference":"Patient/p"},"resourceType":"OBSERVATION"}}],"resourceType":"Bundle"}}`
	if string(got) != want {
		t.Fatalf("view %s\nwant %s", got, want)
	}
}

// largeDecimal is a FHIR decimal past float64's range, valid in FHIR.
const largeDecimal = `"extension":[{"url":"http://example.org/x","valueDecimal":1e400}]`

// exactNames are the member names a reader of a request's patients reads exactly
// by design, so re-casing them may change an answer: the facts a patient
// identifier is derived from (PatientDemographics), the identity rule's and not
// the view's (canonicalnames.go).
var exactNames = map[string]bool{"birthDate": true, "name": true, "family": true}

// recase is body with every member name within a FHIR resource but exactNames,
// and every resourceType value, re-cased by mode: "upper", "lower", or a seeded
// mix of both, rune by rune. A name outside every resource (a vendor's own) is
// left as sent: the view reads it as sent.
func recase(t *testing.T, body []byte, mode string, seed int64) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(seed))
	casing := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case mode == "upper", mode == "mixed" && rng.Intn(2) == 0:
				r = unicode.ToUpper(r)
			default:
				r = unicode.ToLower(r)
			}
			b.WriteRune(r)
		}
		return b.String()
	}
	var walk func(v any, in bool) any
	walk = func(v any, in bool) any {
		switch x := v.(type) {
		case map[string]any:
			res := in || isResource(x)
			out := map[string]any{}
			// The seeded source is drawn from in a fixed order: the members'
			// names sorted.
			for _, k := range slices.Sorted(maps.Keys(x)) {
				c := x[k]
				if s, ok := c.(string); ok && res && strings.EqualFold(k, "resourceType") {
					c = casing(s)
				}
				if res && !exactNames[k] {
					k = casing(k)
				}
				out[k] = walk(c, res)
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, c := range x {
				out[i] = walk(c, in)
			}
			return out
		}
		return v
	}
	out, err := json.Marshal(walk(doc, false))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// A view is new maps, and the binding's readers leave the request's bytes as
// sent: changing one view changes no other read of the same request.
func TestCanonicalNames_TheBindingsRequestIsReadNotChanged(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"patient","id":"MBR-OX","Contained":[{"resourceType":"Patient","id":"x"}]}},{"resource":{"resourceType":"Observation","subject":{"Reference":"Patient/MBR-PAYERB"}}}]}`)
	sent := bytes.Clone(body)
	_, _ = carriedDemographics(body, "MBR-OX", false)
	_ = carriesMembersParty(body, "MBR-OX", true)
	_ = carriesPatient(body, "MBR-OX")
	_ = indexCarriedPatients(body)
	if _, _, err := noOneHeld(EnforcementNone).resolveSubjectPCI(context.Background(), "MBR-OX", body); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, sent) {
		t.Fatal("a reader changed the request's bytes")
	}
	for name, view := range map[string]func([]byte) (canonicalView, error){"strict": strictView, "binding": bindingView} {
		a, _ := view(body)
		b, _ := view(body)
		a.doc.(map[string]any)["entry"] = nil
		if b.doc.(map[string]any)["entry"] == nil {
			t.Fatalf("%s: two views share a map", name)
		}
	}
}

// A body the binding cannot decode at all is refused 422 on authority, never bound
// by the member id alone, at every level, and so is the payer's binding of it. No
// leg's binding meets one: each leg's own readers refuse it first
// (TestBinding_AnUndecodableBodyIsRefusedFirstOnEveryLeg). The system of record's
// own answer is no message: one the binding cannot decode binds by the id alone.
func TestBinding_AnUndecodableBodyIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, level := range bindingLevels {
		g := noOneHeld(level)
		for name, body := range map[string]string{
			"not JSON":                        `{"resourceType":"Bundle"`,
			"a name repeated exactly":         `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"eNEW","id":"eNEW"}}]}`,
			"a name repeated in another case": `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"eNEW","birthDate":"1990-01-01","BirthDate":"1991-01-01"}}]}`,
		} {
			pci, _, found, err := g.resolveBinding(ctx, "eNEW", []byte(body), false)
			if pci != "" || found || !errors.Is(err, errRequestPatientsUnreadable) {
				t.Fatalf("%s/%s: bound %q %v %v, want errRequestPatientsUnreadable", level, name, pci, found, err)
			}
			if status, msg := subjectBindingFailure(ctx, err); status != http.StatusUnprocessableEntity || msg != msgRequestPatientsUnreadable {
				t.Fatalf("%s/%s: the binding answers %d %q, want 422 %q", level, name, status, msg, msgRequestPatientsUnreadable)
			}
			if pci, status, msg, recorded := g.bindInboundSubjectRecorded(ctx, "eNEW", []byte(body), http.StatusBadRequest); pci != "" || status != http.StatusUnprocessableEntity || msg != msgRequestPatientsUnreadable || !recorded {
				t.Fatalf("%s/%s: the payer's binding answers %d %q (recorded %v)", level, name, status, msg, recorded)
			}
			if pci, status, msg, _ := g.bindRecordSubjectRecorded(ctx, "eNEW", []byte(body), http.StatusBadGateway); status != 0 || pci != derivedPCI("eNEW", "", "") {
				t.Fatalf("%s/%s: a record binds %q (%d %q), want the id-alone %q", level, name, pci, status, msg, derivedPCI("eNEW", "", ""))
			}
		}
	}
	if !strings.HasPrefix(msgRequestPatientsUnreadable, "cannot read the request's patients") {
		t.Fatal("the refusal does not open with what it cannot do")
	}
}

// The binding reads a Patient carrying a decimal past float64's range: by its
// demographics, or, as a Coverage's party carrying the member's number with no
// Patient of the member's own, as a member that cannot be identified.
func TestBinding_ALargeDecimalIsRead(t *testing.T) {
	ctx := context.Background()
	g := noOneHeld(EnforcementNone)
	child := `{"resourceType":"Patient","id":"eKID",` + largeDecimal + `,"birthDate":"2015-06-01","name":[{"family":"Childname"}]}`
	if pci, found, err := g.resolveSubjectPCI(ctx, "eKID", []byte(`{"prefetch":{"patient":`+child+`}}`)); err != nil || !found || pci != derivedPCI("eKID", "2015-06-01", "Childname") {
		t.Fatalf("bound %q (found %v, err %v), want the demographics-derived identifier", pci, found, err)
	}
	parent := `{"resourceType":"Patient","id":"parent",` + largeDecimal + `,"identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"eKID"}]}`
	coverage := `{"resourceType":"Coverage","id":"c","contained":[` + parent + `],"subscriber":{"reference":"#parent"},"beneficiary":{"reference":"Patient/eKID"}}`
	if _, _, err := g.resolveSubjectPCI(ctx, "eKID", []byte(`{"prefetch":{"coverage":`+coverage+`}}`)); !errors.Is(err, errPatientUnidentifiable) {
		t.Fatalf("err %v, want the member refused as unidentifiable", err)
	}
	if idx := indexCarriedPatients([]byte(`{"prefetch":{"patient":` + child + `}}`)); len(idx.byName["eKID"]) != 1 {
		t.Fatalf("the carried index reads %v, want the Patient under eKID", idx.byName)
	}
}

// A member spelling "contained" in another case outside every FHIR resource (a
// CDS Hooks request's own member, or a vendor's extension) is no contained list:
// the binding reads no Patient there and binds the member by its id alone, as in
// earlier releases.
func TestBinding_AVendorContainedIsNoContainedList(t *testing.T) {
	patient := `[{"resourceType":"Patient","id":"MBR-ZZ","birthDate":"1990-01-01","name":[{"family":"Vendor"}]}]`
	g := noOneHeld(EnforcementNone)
	for name, body := range map[string]string{
		"a top-level Contained":    `{"hook":"order-select","context":{"patientId":"MBR-ZZ"},"Contained":` + patient + `}`,
		"an extension's Contained": `{"hook":"order-select","context":{"patientId":"MBR-ZZ"},"extension":{"Contained":` + patient + `}}`,
	} {
		pci, found, err := g.resolveSubjectPCI(context.Background(), "MBR-ZZ", []byte(body))
		if err != nil || !found || pci != derivedPCI("MBR-ZZ", "", "") {
			t.Errorf("%s: bound %q (found %v, err %v), want the id-alone %q", name, pci, found, err, derivedPCI("MBR-ZZ", "", ""))
		}
	}
}

// bindRequesterMember reads an answer the binding cannot decode as an answer that
// cannot be read, never as one carrying no Patient for the member. A guard: its
// one caller's memberOf (cdexReadMember) reads the answer strictly and reports
// such an answer unreadable first, so this calls it with a memberOf that reads it.
func TestBindRequesterMember_AnAnswerTheBindingCannotDecodeIsUnreadable(t *testing.T) {
	answer := []byte(`{"resourceType":"Task","for":{"reference":"Patient/eNEW"},"contained":[{"resourceType":"Patient","id":"eNEW","id":"eNEW"}]}`)
	if _, err := strictView(answer); err == nil {
		t.Fatal("fixture: the strict reader reads the answer")
	}
	got := noOneHeld(EnforcementNone).bindRequesterMember(context.Background(), answer, func([]byte) (string, bool) { return "eNEW", true })
	if got.unbound != RequesterUnboundUnreadable || got.pci != "" {
		t.Fatalf("bound %+v, want unbound %q", got, RequesterUnboundUnreadable)
	}
}

// bindingAnswers are every answer the binding's readers give for body: for every
// name a Patient in it goes by, the demographics, the party answer and whether a
// Patient is carried for it, each read as the member's and as its own record's,
// and the binding the gateway makes (a system holding no one); and, for a Bundle,
// the entries the party rule names as a Coverage's party.
func bindingAnswers(t *testing.T, body []byte) string {
	t.Helper()
	v, err := bindingView(body)
	if err != nil {
		return "binding: " + err.Error()
	}
	var names []string
	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["resourceType"] == "Patient" {
				names = append(names, lenientPatientNames(x)...)
			}
			for _, c := range x {
				collect(c)
			}
		case []any:
			for _, c := range x {
				collect(c)
			}
		}
	}
	collect(v.doc)
	var out []string
	if b, ok := v.doc.(map[string]any); ok {
		out = append(out, "entry parties: "+strings.Join(slices.Sorted(maps.Keys(shnsdk.CoverageEntryParties(b))), ","))
	}
	g := noOneHeld(EnforcementNone)
	sort.Strings(names)
	for _, n := range slices.Compact(names) {
		for _, own := range []bool{false, true} {
			d, ok := carriedDemographics(body, n, own)
			out = append(out, n+": demographics "+d.BirthDate+"/"+d.FamilyName+" "+boolWord(ok)+", party "+boolWord(carriesMembersParty(body, n, own)))
		}
		pci, _, err := g.resolveSubjectPCI(context.Background(), n, body)
		out = append(out, n+": carried "+boolWord(carriesPatient(body, n))+", bound "+pci+" "+errWord(err))
	}
	return strings.Join(out, "\n")
}

func errWord(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// bindingCorpus is the binding's own test rows: the sdk's Coverage party table
// (as a resource and as a Bundle entry), dependents' Coverages naming a parent,
// a Parameters carrying a beneficiary tie, a versioned reference to a party, the
// folded and large-decimal rows, and the recorded request fixtures.
func bindingCorpus(t *testing.T) map[string][]byte {
	t.Helper()
	corpus := map[string][]byte{}
	raw, err := os.ReadFile(filepath.Join("testdata", "coverageparty", "rows.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Name     string          `json:"name"`
		Coverage json.RawMessage `json:"coverage"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) < 30 {
		t.Fatalf("the sdk's coverage party rows: %d rows, %v", len(rows), err)
	}
	for _, r := range rows {
		corpus["sdk party/"+r.Name+"/resource"] = r.Coverage
		corpus["sdk party/"+r.Name+"/entry"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"urn:uuid:c1","resource":` + string(r.Coverage) + `}]}`)
	}
	parent := `{"resourceType":"Patient","id":"parent","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"eKID"}],"name":[{"family":"Parentname"}],"birthDate":"1970-01-01"}`
	child := `{"resourceType":"Patient","id":"eKID","name":[{"family":"Childname"}],"birthDate":"2015-06-01"}`
	dependent := func(contained, slot string) string {
		return `{"resourceType":"Coverage","id":"c",` + contained + `,"` + slot + `":{"reference":"#parent"},"beneficiary":{"reference":"Patient/eKID"}}`
	}
	for name, cov := range map[string]string{
		"a dependent's Coverage":               dependent(`"contained":[`+parent+`]`, "subscriber"),
		"its parent under Contained":           dependent(`"Contained":[`+parent+`]`, "subscriber"),
		"its parent in a Subscriber slot":      dependent(`"contained":[`+parent+`]`, "Subscriber"),
		"its parent in a policyHolder slot":    dependent(`"contained":[`+parent+`]`, "policyHolder"),
		"a beneficiary spelled with Reference": strings.Replace(dependent(`"contained":[`+parent+`]`, "subscriber"), `"beneficiary":{"reference"`, `"beneficiary":{"Reference"`, 1),
		"a slot beside one in another case":    strings.Replace(dependent(`"contained":[`+parent+`]`, "policyHolder"), `"beneficiary"`, `"Subscriber":{"display":"x"},"beneficiary"`, 1),
		"the parent containing a list":         dependent(`"contained":[`+strings.Replace(parent, `"id":"parent"`, `"id":"parent","Contained":[]`, 1)+`]`, "subscriber"),
	} {
		corpus["dependent/"+name+"/alone"] = []byte(`{"prefetch":{"coverage":` + cov + `}}`)
		corpus["dependent/"+name+"/with the child"] = []byte(`{"prefetch":{"patient":` + child + `,"coverage":` + cov + `}}`)
	}
	// A Parameters carrying a Coverage whose subscriber names another person and
	// whose beneficiary names the member's own Patient beside it: the tie is
	// resolved through "parameter" (tieNames).
	corpus["parameters, a beneficiary tie"] = []byte(`{"resourceType":"Parameters","parameter":[` +
		`{"name":"coverage","resource":{"resourceType":"Coverage","id":"c","subscriber":{"reference":"Patient/someoneelse"},"beneficiary":{"reference":"Patient/MBR-X"}}},` +
		`{"name":"patient","resource":{"resourceType":"Patient","id":"MBR-X","birthDate":"1990-01-01","name":[{"family":"Xname"}]}}]}`)
	// A Coverage entry naming its parent by a versioned reference, which the
	// party rule resolves by the Patient's meta.versionId.
	corpus["versioned parent entry"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"http://ehr.example/fhir/Coverage/c","resource":{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/kid"},"subscriber":{"reference":"Patient/parent/_history/2"}}},` +
		`{"fullUrl":"http://ehr.example/fhir/Patient/kid","resource":{"resourceType":"Patient","id":"kid"}},` +
		`{"fullUrl":"http://ehr.example/fhir/Patient/parent","resource":{"resourceType":"Patient","id":"parent","meta":{"versionId":"2"}}}]}`)
	// A Coverage entry naming its parent, an entry of its own, in its
	// policyHolder slot alone (shnsdk.CoverageEntryParties).
	corpus["policyHolder parent entry"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"http://ehr.example/fhir/Coverage/c","resource":{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/kid"},"policyHolder":{"reference":"Patient/parent"}}},` +
		`{"fullUrl":"http://ehr.example/fhir/Patient/kid","resource":{"resourceType":"Patient","id":"kid"}},` +
		`{"fullUrl":"http://ehr.example/fhir/Patient/parent","resource":{"resourceType":"Patient","id":"parent"}}]}`)
	corpus["a member identifier"] = []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"eMEM"}],"birthDate":"1980-01-01","name":[{"family":"Member"}]}}]}`)
	corpus["large decimal"] = []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"MBR-OX",` + largeDecimal + `}}]}`)
	files, err := filepath.Glob(filepath.Join("testdata", "relayfidelity", "valid", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no recorded fixtures: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		corpus["fixture/"+filepath.Base(f)] = b
	}
	return corpus
}

// recasingFence runs answers over every row of corpus as written and re-cased
// (recase: upper, lower, and three fixed seeds of mixed case), and reports each
// difference; at least min rows must be read.
func recasingFence(t *testing.T, corpus map[string][]byte, answers func(*testing.T, []byte) string, min int) {
	t.Helper()
	read := 0
	for _, name := range slices.Sorted(maps.Keys(corpus)) {
		body := corpus[name]
		if _, err := bindingView(body); err != nil {
			continue // a row the readers refuse to read reads nothing to compare
		}
		read++
		want := answers(t, body)
		for _, v := range []struct {
			mode string
			seed int64
		}{{"upper", 0}, {"lower", 0}, {"mixed", 1}, {"mixed", 2}, {"mixed", 3}} {
			if got := answers(t, recase(t, body, v.mode, v.seed)); got != want {
				t.Errorf("%s, %s/%d: the re-cased request reads\n%s\nwant\n%s", name, v.mode, v.seed, got, want)
			}
		}
	}
	if read < min {
		t.Fatalf("only %d rows read: the fence tests too little", read)
	}
}

// The fence on the canonical names over the binding's readers, in the core-only
// build too: re-casing every member name within a FHIR resource (but the identity
// facts, exactNames) and every resourceType value, in every row of the binding's
// own tables, changes no answer the binding or the party rule gives. A name a
// reader reads that viewNames does not list, or a type it compares that viewTypes
// does not, makes some row differ.
func TestCanonicalNames_EveryBindingRowReadsTheSameRecased(t *testing.T) {
	recasingFence(t, bindingCorpus(t), bindingAnswers, 50)
}

// A canonical view is never compared: comparing two with == or != fails to compile,
// since its doc holds a decoded request (a map) and comparing interfaces holding maps
// panics at run time. The compile error itself cannot be a test; its cause is pinned
// here, so a change that made the type comparable again fails.
func TestCanonicalView_IsNotComparable(t *testing.T) {
	if reflect.TypeOf(canonicalView{}).Comparable() {
		t.Fatal("canonicalView is comparable: == on two views would compile, and panic on a map")
	}
}
