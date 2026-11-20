package pasgraph

import (
	"errors"
	"strings"
	"testing"
)

// notifiedBundle is a PAS Response Bundle of entries, each a fullUrl and a
// resource, as br-payer notifies its decision: a collection, absolute
// fullUrls on the payer's base.
func notifiedBundle(entries ...string) string {
	return `{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(entries, ",") + `]}`
}

func entryOf(full, resource string) string {
	return `{"fullUrl":"` + full + `","resource":` + resource + `}`
}

const payerBase = "https://payer.example/fhir"

func crNaming(id, patientRef string) string {
	return entryOf(payerBase+"/ClaimResponse/"+id, `{"resourceType":"ClaimResponse","id":"`+id+`","patient":{"reference":"`+patientRef+`"}}`)
}

// unreadableCoverage is a Coverage entry the network cannot read (its
// fullUrl does not end in its id), its parties as members.
func unreadableCoverage(parties string) string {
	return entryOf(payerBase+"/Coverage/c1", `{"resourceType":"Coverage","id":"c2",`+parties+`}`)
}

func patientEntry(id string) string {
	return entryOf(payerBase+"/Patient/"+id, `{"resourceType":"Patient","id":"`+id+`"}`)
}

// A notified decision's patients are read by the identity rules a PAS answer
// is read by, every ClaimResponse's, and only what the binding needs refuses:
// a relative and an absolute reference resolve to the Patient entry; one no
// entry resolves binds by its Patient/<id>; two ClaimResponses bind two
// patients; a defect elsewhere is reported, not refused; one that touches the
// binding, or no patient at all, is unbindable; over the limit is its own
// error.
func TestReadNotified(t *testing.T) {
	for name, c := range map[string]struct {
		raw     string
		members []string
		defects int
		err     error
	}{
		"a relative reference":                         {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1")), []string{"p1"}, 0, nil},
		"an absolute reference on the payer's base":    {notifiedBundle(crNaming("cr1", payerBase+"/Patient/p1"), patientEntry("p1")), []string{"p1"}, 0, nil},
		"a reference no entry resolves":                {notifiedBundle(crNaming("cr1", "Patient/p9")), []string{"p9"}, 0, nil},
		"a bare ClaimResponse":                         {`{"resourceType":"ClaimResponse","id":"cr1","patient":{"reference":"Patient/p1"}}`, []string{"p1"}, 0, nil},
		"two ClaimResponses, two patients":             {notifiedBundle(crNaming("cr1", "Patient/p1"), crNaming("cr2", "Patient/p2"), patientEntry("p1"), patientEntry("p2")), []string{"p1", "p2"}, 0, nil},
		"a Bundle that is not a collection":            {strings.Replace(notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1")), `"collection"`, `"searchset"`, 1), []string{"p1"}, 1, nil},
		"an unrelated entry's fullUrl mismatch":        {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"), entryOf(payerBase+"/Organization/o1", `{"resourceType":"Organization","id":"o2"}`)), []string{"p1"}, 1, nil},
		"a defective Patient no ClaimResponse names":   {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"), entryOf(payerBase+"/Patient/p7", `{"resourceType":"Patient","id":"p8"}`)), []string{"p1"}, 1, nil},
		"the bound Patient's entry defective":          {notifiedBundle(crNaming("cr1", "Patient/p1"), entryOf(payerBase+"/Patient/p1", `{"resourceType":"Patient","id":"p2"}`)), nil, 0, ErrUnbindable},
		"two entries claim the bound identity":         {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"), patientEntry("p1")), nil, 0, ErrUnbindable},
		"the bound Patient without a fullUrl":          {notifiedBundle(crNaming("cr1", "Patient/p1"), `{"resource":{"resourceType":"Patient","id":"p1"}}`), nil, 0, ErrUnbindable},
		"the bound Patient's id under another fullUrl": {notifiedBundle(crNaming("cr1", "Patient/p1"), entryOf(payerBase+"/Patient/p7", `{"resourceType":"Patient","id":"p1"}`)), nil, 0, ErrUnbindable},
		// pasgraph reads no Coverage party: whether an unreadable entry bears
		// on the binding is the engine's to decide, by the binding's own
		// reader (bindPASNotified); here it is a defect like any other.
		"an unreadable Coverage naming the patient":     {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"), unreadableCoverage(`"subscriber":{"reference":"Patient/p1"}`)), []string{"p1"}, 1, nil},
		"an unreadable Coverage naming another patient": {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"), unreadableCoverage(`"subscriber":{"reference":"Patient/p9"},"payor":[{"reference":"Organization/o1"}]`)), []string{"p1"}, 1, nil},
		"a ClaimResponse entry without a fullUrl":       {notifiedBundle(`{"resource":{"resourceType":"ClaimResponse","id":"cr1","patient":{"reference":"Patient/p1"}}}`, patientEntry("p1")), nil, 0, ErrUnbindable},
		// Beside a clean ClaimResponse that binds, an unreadable one naming
		// another patient still names a patient the network cannot bind.
		"an unreadable ClaimResponse beside a clean one": {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"),
			`{"resource":{"resourceType":"ClaimResponse","id":"cr2","patient":{"reference":"Patient/p2"}}}`, patientEntry("p2")), nil, 0, ErrUnbindable},
		// An unreadable ClaimResponse beside a clean one, its type or its
		// entry's resource member in another case, as a lenient reader
		// takes them, still names a patient the network cannot bind.
		"an unreadable claimResponse beside a clean one": {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"),
			entryOf(payerBase+"/ClaimResponse/cr2", `{"resourceType":"claimResponse","id":"cr2","patient":{"reference":"Patient/p2"}}`)), nil, 0, ErrUnbindable},
		"an unreadable ClaimResponse by ResourceType beside a clean one": {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"),
			entryOf(payerBase+"/ClaimResponse/cr2", `{"ResourceType":"ClaimResponse","id":"cr2","patient":{"reference":"Patient/p2"}}`)), nil, 0, ErrUnbindable},
		"an unreadable ClaimResponse under Resource beside a clean one": {notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"),
			`{"fullUrl":"`+payerBase+`/ClaimResponse/cr2","Resource":{"resourceType":"ClaimResponse","id":"cr2","patient":{"reference":"Patient/p2"}}}`), nil, 0, ErrUnbindable},
		// urn:uuid fullUrls and references resolve by FHIR R4's Bundle rule:
		// the patient is the Patient entry whose fullUrl the reference is.
		"a urn:uuid patient":                   {notifiedBundle(entryOf("urn:uuid:c1", `{"resourceType":"ClaimResponse","id":"cr1","patient":{"reference":"urn:uuid:p"}}`), entryOf("urn:uuid:p", `{"resourceType":"Patient","id":"p1"}`)), []string{"p1"}, 0, nil},
		"a urn:uuid patient no entry resolves": {notifiedBundle(entryOf("urn:uuid:c1", `{"resourceType":"ClaimResponse","id":"cr1","patient":{"reference":"urn:uuid:q"}}`), entryOf("urn:uuid:p", `{"resourceType":"Patient","id":"p1"}`)), nil, 0, ErrUnbindable},
		"a urn:uuid Patient with no id":        {notifiedBundle(entryOf("urn:uuid:c1", `{"resourceType":"ClaimResponse","id":"cr1","patient":{"reference":"urn:uuid:p"}}`), entryOf("urn:uuid:p", `{"resourceType":"Patient"}`)), nil, 0, ErrUnbindable},
		// A tail no entry resolves, which a defective entry claims, binds
		// to nothing.
		"a tail a defective entry claims":   {notifiedBundle(crNaming("cr1", "Patient/p9"), entryOf(payerBase+"/Basic/b1", `{"resourceType":"Patient","id":"p9","x":1}`)), nil, 0, ErrUnbindable},
		"a ClaimResponse naming no patient": {notifiedBundle(entryOf(payerBase+"/ClaimResponse/cr1", `{"resourceType":"ClaimResponse","id":"cr1"}`)), nil, 0, ErrUnbindable},
		"a patient that is no Patient":      {notifiedBundle(crNaming("cr1", payerBase+"/Group/g1")), nil, 0, ErrUnbindable},
		"no ClaimResponse":                  {notifiedBundle(patientEntry("p1")), nil, 0, ErrUnbindable},
		"no entries":                        {`{"resourceType":"Bundle","type":"collection","entry":[]}`, nil, 0, ErrUnbindable},
		"not JSON":                          {`{`, nil, 0, ErrUnbindable},
		"over the bytes limit":              {notifiedBundle(crNaming("cr1", "Patient/p1"), entryOf(payerBase+"/Basic/b1", `{"resourceType":"Basic","id":"b1","x":"`+strings.Repeat("a", MaxBytes)+`"}`)), nil, 0, ErrOverLimit},
	} {
		t.Run(name, func(t *testing.T) {
			n, err := ReadNotified([]byte(c.raw))
			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Fatalf("err %v, want %v", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err %v, want bound", err)
			}
			var members []string
			for _, p := range n.Patients {
				members = append(members, p.Member())
			}
			if strings.Join(members, ",") != strings.Join(c.members, ",") || len(n.Defects) != c.defects {
				t.Fatalf("members %v and %d defects %v, want %v and %d", members, len(n.Defects), n.Defects, c.members, c.defects)
			}
		})
	}
	over := make([]string, MaxResources+1)
	for i := range over {
		over[i] = patientEntry("p" + strings.Repeat("x", i%7) + string(rune('a'+i%26)))
	}
	if _, err := ReadNotified([]byte(notifiedBundle(over...))); !errors.Is(err, ErrOverLimit) {
		t.Fatalf("over the entry limit: %v, want ErrOverLimit", err)
	}
}

// A notified decision is held to the response graph rule at each line it is
// read at: the payer's declared lines, or every line it could be on when it
// declares none (NotifiedLines); it meets the rule when any accepts it, and
// the refusal names each line tried and its failure. A dangling reference
// with the request Claim named meets 2.1 alone.
func TestValidateNotified(t *testing.T) {
	ok := notifiedBundle(crNaming("cr1", "Patient/p1"), patientEntry("p1"))
	dangling := notifiedBundle(entryOf(payerBase+"/ClaimResponse/cr1", `{"resourceType":"ClaimResponse","id":"cr1","patient":{"reference":"Patient/p1"},"insurer":{"reference":"Organization/o9"},"request":{"identifier":{"value":"c1"}}}`), patientEntry("p1"))
	if line, err := ValidateNotified([]byte(ok), nil); err != nil || line != "2.0.1" {
		t.Fatalf("a clean decision: %q, %v; want 2.0.1", line, err)
	}
	if line, err := ValidateNotified([]byte(dangling), nil); err != nil || line != "2.1" {
		t.Fatalf("a dangling reference, no line declared: %q, %v; want 2.1", line, err)
	}
	_, err := ValidateNotified([]byte(dangling), []string{"2.0.1", "2.2"})
	if err == nil || !strings.Contains(err.Error(), "the IG lines the payer declares (2.0.1: ") || !strings.Contains(err.Error(), "; 2.2: ") || strings.Contains(err.Error(), "2.1:") {
		t.Fatalf("declared 2.0.1 and 2.2: %v; want refused, naming each and its failure", err)
	}
	two := notifiedBundle(crNaming("cr1", "Patient/p1"), crNaming("cr2", "Patient/p1"), patientEntry("p1"))
	_, err = ValidateNotified([]byte(two), nil)
	if err == nil || !strings.Contains(err.Error(), "the IG lines it could be on (2.0.1: ") || !strings.Contains(err.Error(), "; 2.2: ") || !strings.Contains(err.Error(), "; 2.1: ") {
		t.Fatalf("two ClaimResponses: %v; want refused at every line, each named", err)
	}
}

// The PAS answer's own reader (ValidateBundleAt, the pas-claim answer's
// graph rule) resolves a urn:uuid reference to the entry whose fullUrl it is
// at every line, so a decision written with urn:uuid is read the same way on
// both paths.
func TestPASAnswerGraphResolvesURN(t *testing.T) {
	urn := notifiedBundle(entryOf("urn:uuid:c1", `{"resourceType":"ClaimResponse","id":"cr1","patient":{"reference":"urn:uuid:p"},"request":{"identifier":{"value":"c1"}}}`), entryOf("urn:uuid:p", `{"resourceType":"Patient","id":"p1"}`))
	for _, l := range NotifiedLines {
		if err := ValidateBundleAt([]byte(urn), l); err != nil {
			t.Errorf("%s: %v", l, err)
		}
	}
	if err := ValidateBundle([]byte(strings.Replace(urn, `"urn:uuid:p"}`, `"urn:uuid:q"}`, 1))); err == nil {
		t.Error("a urn:uuid reference no entry resolves: read, want refused")
	}
}

// A bare ClaimResponse is the resource of the entry carrying it: a relative
// patient reference resolves against that entry's RESTful fullUrl, and,
// under a urn or with none, binds by the Patient/<id> it names.
func TestReadNotifiedAtItsEntry(t *testing.T) {
	cr := `{"resourceType":"ClaimResponse","id":"cr1","patient":{"reference":"Patient/p1"}}`
	for full, want := range map[string]string{
		"https://other.example/fhir/ClaimResponse/cr1":  "https://other.example/fhir/Patient/p1",
		"urn:uuid:a0000000-0000-4000-8000-0000000000c1": "Patient/p1",
		"": "Patient/p1",
		// A fullUrl that is not the ClaimResponse's own identity (another
		// id, a history version, a query) is no base it resolves against.
		"https://other.example/fhir/ClaimResponse/cr9":            "Patient/p1",
		"https://other.example/fhir/ClaimResponse/cr1/_history/2": "Patient/p1",
		"https://other.example/fhir/ClaimResponse/cr1?x=1":        "Patient/p1",
		"https://other.example/fhir/Claim/cr1":                    "Patient/p1",
	} {
		n, err := ReadNotifiedAt([]byte(cr), full)
		if err != nil || len(n.Patients) != 1 || n.Patients[0].Identity != want {
			t.Fatalf("at %q: %+v, %v; want %s", full, n, err, want)
		}
	}
	// An id no FHIR id could be (one holding a "/") is no identity a fullUrl
	// ends in, however its path is spelled.
	unsafe := `{"resourceType":"ClaimResponse","id":"x/../cr1","patient":{"reference":"Patient/p1"}}`
	if n, err := ReadNotifiedAt([]byte(unsafe), "https://other.example/fhir/ClaimResponse/x/../cr1"); err != nil || len(n.Patients) != 1 || n.Patients[0].Identity != "Patient/p1" {
		t.Fatalf("an unsafe id: %+v, %v; want Patient/p1", n, err)
	}
}
