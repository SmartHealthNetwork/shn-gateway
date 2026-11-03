package cdextask

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// The fixtures are the CDex 2.1.0 IG's own examples of the
// cdex-task-attachment-request profile, byte for byte
// (https://hl7.org/fhir/us/davinci-cdex/STU2.1/Task-cdex-task-example19.json,
// an attachment request by code, and Task-cdex-task-example22.json, one by
// questionnaire).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func read(t *testing.T, b []byte) Task {
	t.Helper()
	task, err := Read(relay.NewBody(b, relay.OriginIngressRequest))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return task
}

// mutate decodes example 19, applies edit and encodes it again.
func mutate(t *testing.T, edit func(task map[string]any)) []byte {
	t.Helper()
	var task map[string]any
	if err := json.Unmarshal(fixture(t, "cdex-task-example19.json"), &task); err != nil {
		t.Fatal(err)
	}
	edit(task)
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func contained(task map[string]any, id string) map[string]any {
	for _, c := range task["contained"].([]any) {
		if m := c.(map[string]any); m["id"] == id {
			return m
		}
	}
	panic("no contained " + id)
}

func inputs(task map[string]any) []any { return task["input"].([]any) }

func coding(system, code string) map[string]any {
	return map[string]any{"coding": []any{map[string]any{"system": system, "code": code}}}
}

const (
	npi          = "http://hl7.org/fhir/sid/us-npi"
	v20203       = "http://terminology.hl7.org/CodeSystem/v2-0203"
	example19URL = "http://example.org/cdex/payer/$submit-attachment"
	cdexTemp     = "http://hl7.org/fhir/us/davinci-cdex/CodeSystem/cdex-temp"
)

var (
	member     = Identifier{System: "http://example.org/cdex/payer/member-ids", Value: "Member123", Type: []Coding{{System: v20203, Code: "MB"}}}
	orgNPI     = IdentifierField{State: Present, Identifier: Identifier{System: npi, Value: "1234567893"}}
	practNPI   = IdentifierField{State: Present, Identifier: Identifier{System: npi, Value: "9941339100"}}
	payerID    = IdentifierField{State: Present, Identifier: Identifier{System: "http://example.org/cdex/payer/payer-ids", Value: "Payer123"}}
	unreadable = IdentifierField{State: Unreadable}
)

// The IG's attachment request by code reads whole, every field by structure.
func TestRead_TheIGExample(t *testing.T) {
	task := read(t, fixture(t, "cdex-task-example19.json"))
	if !task.Code.Has(PASTempCodes, CodeAttachmentRequest) || task.Code.Has(PASTempCodes, CodeAttachmentRequestQuestionnaire) {
		t.Errorf("code %+v", task.Code)
	}
	p := task.Patient
	if p.State != Present || !p.Contained || p.Reference != "#patient" || p.Identifier.State != Absent || !reflect.DeepEqual(p.Identifiers, []Identifier{member}) {
		t.Errorf("patient %+v", p)
	}
	if !strings.HasPrefix(string(p.Resource), "{") || !strings.Contains(string(p.Resource), `"birthDate"`) || !strings.Contains(string(p.Resource), `"Shaw"`) {
		t.Errorf("patient resource %s", p.Resource)
	}
	if want := (Owner{State: Present, Reference: "#practitionerrole", Organization: orgNPI, Practitioner: practNPI}); !reflect.DeepEqual(task.Owner, want) {
		t.Errorf("owner %+v\nwant %+v", task.Owner, want)
	}
	if want := (Requester{State: Present, Identifier: payerID}); !reflect.DeepEqual(task.Requester, want) {
		t.Errorf("requester %+v", task.Requester)
	}
	if task.PayerURL != (PayerURL{State: Present, Value: example19URL, Index: 2, Form: "valueUrl"}) {
		t.Errorf("payer-url %+v", task.PayerURL)
	}
}

// The IG's attachment request by questionnaire reads its own code and the
// same fields.
func TestRead_TheIGQuestionnaireExample(t *testing.T) {
	task := read(t, fixture(t, "cdex-task-example22.json"))
	if !task.Code.Has(PASTempCodes, CodeAttachmentRequestQuestionnaire) || task.Code.Has(PASTempCodes, CodeAttachmentRequest) {
		t.Errorf("code %+v", task.Code)
	}
	if task.Patient.State != Present || !task.Patient.Contained || len(task.Patient.Identifiers) == 0 {
		t.Errorf("patient %+v", task.Patient)
	}
	if task.Owner.State != Present || task.Owner.Organization.State != Present || task.Owner.Practitioner.State != Present {
		t.Errorf("owner %+v", task.Owner)
	}
	if task.Requester.State != Present || task.Requester.Identifier.State != Present {
		t.Errorf("requester %+v", task.Requester)
	}
	if task.PayerURL != (PayerURL{State: Present, Value: example19URL, Index: 1, Form: "valueUrl"}) {
		t.Errorf("payer-url %+v", task.PayerURL)
	}
}

// A body that is not one JSON Task, or that reads two ways at any depth, is
// not read at all.
func TestRead_RefusesAWholeBody(t *testing.T) {
	ex := string(fixture(t, "cdex-task-example19.json"))
	replace := func(old, new string) string {
		if strings.Count(ex, old) != 1 {
			t.Fatalf("fixture: %s appears %d times, want once", old, strings.Count(ex, old))
		}
		return strings.Replace(ex, old, new, 1)
	}
	for name, row := range map[string]struct {
		body string
		want error
	}{
		"a repeated member name":             {replace(`"resourceType" : "Task"`, `"resourceType" : "Task", "resourceType" : "Task"`), relay.ErrDuplicateKey},
		"a member repeated but for case":     {replace(`"status" : "requested"`, `"status" : "requested", "Status" : "requested"`), relay.ErrDuplicateKey},
		"a repeat deep inside":               {replace(`"value" : "Member123"`, `"value" : "Member123", "value" : "Member999"`), relay.ErrDuplicateKey},
		"resourceType by case only":          {`{"ResourceType":"Task"}`, relay.ErrFieldNameCase},
		"a top-level member by case only":    {replace(`"for" :`, `"For" :`), relay.ErrFieldNameCase},
		"a reference member by case only":    {replace(`"reference" : "#patient"`, `"Reference" : "#patient"`), relay.ErrFieldNameCase},
		"a contained member by case only":    {replace(`"organization" :`, `"Organization" :`), relay.ErrFieldNameCase},
		"a coding member by case only":       {replace(`"code" : "attachment-request-code"`, `"Code" : "attachment-request-code"`), relay.ErrFieldNameCase},
		"the payer-url's value by case only": {replace(`"valueUrl" :`, `"ValueUrl" :`), relay.ErrFieldNameCase},
		"the payer-url's value all lower":    {replace(`"valueUrl" :`, `"valueurl" :`), relay.ErrFieldNameCase},
		"not a Task":                         {replace(`"resourceType" : "Task"`, `"resourceType" : "Patient"`), ErrNotTask},
		"no resourceType":                    {`{"code":{"coding":[{"system":"` + PASTempCodes + `","code":"attachment-request-code"}]}}`, ErrNotTask},
		"resourceType not a string":          {`{"resourceType":1}`, ErrNotTask},
		"resourceType null":                  {`{"resourceType":null}`, ErrNotTask},
		"an array":                           {`[]`, nil},
		"null":                               {`null`, nil},
		"not JSON":                           {`{"resourceType":"Task"`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Read(relay.NewBody([]byte(row.body), relay.OriginIngressRequest))
			if err == nil {
				t.Fatalf("read %+v, want an error", got)
			}
			if row.want != nil && !errors.Is(err, row.want) {
				t.Fatalf("err %v, want %v", err, row.want)
			}
			if !reflect.DeepEqual(got, Task{}) {
				t.Fatalf("a refused body read %+v", got)
			}
		})
	}
}

func TestRead_Code(t *testing.T) {
	for name, row := range map[string]struct {
		edit  func(map[string]any)
		state State
		has   bool
	}{
		"absent":        {func(m map[string]any) { delete(m, "code") }, Absent, false},
		"null":          {func(m map[string]any) { m["code"] = nil }, Unreadable, false},
		"text only":     {func(m map[string]any) { m["code"] = map[string]any{"text": CodeAttachmentRequest} }, Unreadable, false},
		"not an object": {func(m map[string]any) { m["code"] = CodeAttachmentRequest }, Unreadable, false},
		"coding null":   {func(m map[string]any) { m["code"] = map[string]any{"coding": nil} }, Unreadable, false},
		"coding empty":  {func(m map[string]any) { m["code"] = map[string]any{"coding": []any{}} }, Unreadable, false},
		"coding not a list": {func(m map[string]any) {
			m["code"] = map[string]any{"coding": coding(PASTempCodes, CodeAttachmentRequest)["coding"].([]any)[0]}
		}, Unreadable, false},
		"a coding that is null": {func(m map[string]any) { m["code"] = map[string]any{"coding": []any{nil}} }, Unreadable, false},
		"a coding without a system": {func(m map[string]any) {
			m["code"] = map[string]any{"coding": []any{map[string]any{"code": CodeAttachmentRequest}}}
		}, Unreadable, false},
		"an empty code": {func(m map[string]any) { m["code"] = coding(PASTempCodes, "") }, Unreadable, false},
		"a system that is a number": {func(m map[string]any) {
			m["code"] = map[string]any{"coding": []any{map[string]any{"system": 1, "code": CodeAttachmentRequest}}}
		}, Unreadable, false},
		"another system": {func(m map[string]any) { m["code"] = coding(cdexTemp, CodeAttachmentRequest) }, Present, false},
		"its display, not its code": {func(m map[string]any) {
			m["code"] = map[string]any{"coding": []any{map[string]any{"system": PASTempCodes, "code": "data-request-code", "display": CodeAttachmentRequest}}, "text": CodeAttachmentRequest}
		}, Present, false},
		"beside another coding": {func(m map[string]any) {
			m["code"] = map[string]any{"coding": []any{map[string]any{"system": "urn:x", "code": "y"}, map[string]any{"system": PASTempCodes, "code": CodeAttachmentRequest}}}
		}, Present, true},
		"beside a coding without a system": {func(m map[string]any) {
			m["code"] = map[string]any{"coding": []any{map[string]any{"code": "x"}, map[string]any{"system": PASTempCodes, "code": CodeAttachmentRequest}}}
		}, Present, true},
	} {
		t.Run(name, func(t *testing.T) {
			c := read(t, mutate(t, row.edit)).Code
			if c.State != row.state || c.Has(PASTempCodes, CodeAttachmentRequest) != row.has {
				t.Fatalf("code %+v, want %s, has %v", c, row.state, row.has)
			}
			if c.State != Present && len(c.Codings) != 0 {
				t.Fatalf("a code that is not read has codings %+v", c.Codings)
			}
		})
	}
}

func TestRead_Patient(t *testing.T) {
	for name, row := range map[string]struct {
		edit func(map[string]any)
		want Patient
	}{
		"absent":                           {func(m map[string]any) { delete(m, "for") }, Patient{State: Absent}},
		"null":                             {func(m map[string]any) { m["for"] = nil }, Patient{State: Unreadable}},
		"empty":                            {func(m map[string]any) { m["for"] = map[string]any{} }, Patient{State: Unreadable}},
		"a null reference alone":           {func(m map[string]any) { m["for"] = map[string]any{"reference": nil} }, Patient{State: Unreadable}},
		"not an object":                    {func(m map[string]any) { m["for"] = "#patient" }, Patient{State: Unreadable}},
		"a reference that is not a string": {func(m map[string]any) { m["for"] = map[string]any{"reference": 7} }, Patient{State: Unreadable}},
		"an empty reference":               {func(m map[string]any) { m["for"] = map[string]any{"reference": ""} }, Patient{State: Unreadable}},
		"a literal reference":              {func(m map[string]any) { m["for"] = map[string]any{"reference": "Patient/123"} }, Patient{State: Present, Reference: "Patient/123"}},
		"an identifier and no reference": {func(m map[string]any) {
			m["for"] = map[string]any{"identifier": map[string]any{"system": "urn:x", "value": "M1"}}
		}, Patient{State: Present, Identifier: IdentifierField{State: Present, Identifier: Identifier{System: "urn:x", Value: "M1"}}}},
		"an identifier given as a list": {func(m map[string]any) {
			m["for"] = map[string]any{"identifier": []any{map[string]any{"system": "urn:x", "value": "M1"}}}
		}, Patient{State: Present, Identifier: unreadable}},
		"names no contained resource":                {func(m map[string]any) { m["for"] = map[string]any{"reference": "#nobody"} }, Patient{State: Unreadable}},
		"names an id that differs only in case":      {func(m map[string]any) { m["for"] = map[string]any{"reference": "#Patient"} }, Patient{State: Unreadable}},
		"names a contained resource of another type": {func(m map[string]any) { m["for"] = map[string]any{"reference": "#practitionerrole"} }, Patient{State: Unreadable}},
		"two contained resources share its id": {func(m map[string]any) {
			m["contained"] = append(m["contained"].([]any), map[string]any{"resourceType": "Patient", "id": "patient", "identifier": []any{map[string]any{"value": "Other"}}})
		}, Patient{State: Unreadable}},
		"contained is not a list": {func(m map[string]any) { m["contained"] = contained(m, "patient") }, Patient{State: Unreadable}},
		"contained holds a null":  {func(m map[string]any) { m["contained"] = append(m["contained"].([]any), nil) }, Patient{State: Unreadable}},
		"a contained resource has no id": {func(m map[string]any) {
			m["contained"] = append(m["contained"].([]any), map[string]any{"resourceType": "Organization"})
		}, Patient{State: Unreadable}},
		"an identifier without a value": {func(m map[string]any) {
			contained(m, "patient")["identifier"] = []any{map[string]any{"system": "urn:x"}}
		}, Patient{State: Unreadable}},
		"an identifier whose value is not a string": {func(m map[string]any) {
			contained(m, "patient")["identifier"] = []any{map[string]any{"system": "urn:x", "value": 7}}
		}, Patient{State: Unreadable}},
		"identifiers that are not a list": {func(m map[string]any) {
			contained(m, "patient")["identifier"] = map[string]any{"system": "urn:x", "value": "M"}
		}, Patient{State: Unreadable}},
		"identifiers null":  {func(m map[string]any) { contained(m, "patient")["identifier"] = nil }, Patient{State: Unreadable}},
		"identifiers empty": {func(m map[string]any) { contained(m, "patient")["identifier"] = []any{} }, Patient{State: Unreadable}},
		"an identifier's type unreadable": {func(m map[string]any) {
			contained(m, "patient")["identifier"] = []any{map[string]any{"value": "M", "type": "MB"}}
		}, Patient{State: Unreadable}},
	} {
		t.Run(name, func(t *testing.T) {
			got := read(t, mutate(t, row.edit)).Patient
			if !reflect.DeepEqual(got, row.want) {
				t.Fatalf("patient %+v\nwant %+v", got, row.want)
			}
		})
	}
	t.Run("a contained Patient with no identifier", func(t *testing.T) {
		got := read(t, mutate(t, func(m map[string]any) { delete(contained(m, "patient"), "identifier") })).Patient
		if got.State != Present || !got.Contained || len(got.Identifiers) != 0 || len(got.Resource) == 0 {
			t.Fatalf("patient %+v", got)
		}
	})
	t.Run("the identifiers in order", func(t *testing.T) {
		got := read(t, mutate(t, func(m map[string]any) {
			p := contained(m, "patient")
			p["identifier"] = append(p["identifier"].([]any), map[string]any{"value": "MRN-1"})
		})).Patient
		if got.State != Present || !reflect.DeepEqual(got.Identifiers, []Identifier{member, {Value: "MRN-1"}}) {
			t.Fatalf("patient %+v", got)
		}
	})
	// A type coding R4 allows without a system is passed over, never read as
	// the identifier being unreadable: the patient can still be bound.
	t.Run("an identifier type coding without a system", func(t *testing.T) {
		got := read(t, mutate(t, func(m map[string]any) {
			id := contained(m, "patient")["identifier"].([]any)[0].(map[string]any)
			id["type"] = map[string]any{"coding": []any{map[string]any{"code": "MB"}, map[string]any{"system": v20203, "code": "MB"}}}
		})).Patient
		if got.State != Present || !reflect.DeepEqual(got.Identifiers, []Identifier{member}) {
			t.Fatalf("patient %+v", got)
		}
	})
}

func TestRead_Owner(t *testing.T) {
	for name, row := range map[string]struct {
		edit func(map[string]any)
		want Owner
	}{
		"absent":        {func(m map[string]any) { delete(m, "owner") }, Owner{State: Absent}},
		"null":          {func(m map[string]any) { m["owner"] = nil }, Owner{State: Unreadable}},
		"empty":         {func(m map[string]any) { m["owner"] = map[string]any{} }, Owner{State: Unreadable}},
		"not an object": {func(m map[string]any) { m["owner"] = []any{} }, Owner{State: Unreadable}},
		"an identifier on the reference": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": map[string]any{"system": npi, "value": "1234567893"}}
		}, Owner{State: Present, Identifier: orgNPI}},
		"an identifier given as a list": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": []any{map[string]any{"system": npi, "value": "1234567893"}}}
		}, Owner{State: Present, Identifier: unreadable}},
		"an unreadable identifier on the reference": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": map[string]any{"system": npi}}
		}, Owner{State: Present, Identifier: unreadable}},
		"a literal reference": {func(m map[string]any) { m["owner"] = map[string]any{"reference": "Organization/1"} },
			Owner{State: Present, Reference: "Organization/1"}},
		"names no contained resource": {func(m map[string]any) { m["owner"] = map[string]any{"reference": "#nobody"} },
			Owner{State: Present, Reference: "#nobody", Organization: unreadable, Practitioner: unreadable}},
		"names a contained resource of another type": {func(m map[string]any) { m["owner"] = map[string]any{"reference": "#patient"} },
			Owner{State: Present, Reference: "#patient", Organization: unreadable, Practitioner: unreadable}},
		"a role with no organization": {func(m map[string]any) { delete(contained(m, "practitionerrole"), "organization") },
			Owner{State: Present, Reference: "#practitionerrole", Practitioner: practNPI}},
		"an organization reference without an identifier": {func(m map[string]any) {
			contained(m, "practitionerrole")["organization"] = map[string]any{"reference": "Organization/1"}
		}, Owner{State: Present, Reference: "#practitionerrole", Practitioner: practNPI}},
		"an organization that is empty": {func(m map[string]any) {
			contained(m, "practitionerrole")["organization"] = map[string]any{}
		}, Owner{State: Present, Reference: "#practitionerrole", Organization: unreadable, Practitioner: practNPI}},
		"an organization identifier without a value": {func(m map[string]any) {
			contained(m, "practitionerrole")["organization"] = map[string]any{"identifier": map[string]any{"system": npi}}
		}, Owner{State: Present, Reference: "#practitionerrole", Organization: unreadable, Practitioner: practNPI}},
		"an organization that is not an object": {func(m map[string]any) {
			contained(m, "practitionerrole")["organization"] = "1234567893"
		}, Owner{State: Present, Reference: "#practitionerrole", Organization: unreadable, Practitioner: practNPI}},
		"two roles share its id": {func(m map[string]any) {
			m["contained"] = append(m["contained"].([]any), map[string]any{"resourceType": "PractitionerRole", "id": "practitionerrole"})
		}, Owner{State: Present, Reference: "#practitionerrole", Organization: unreadable, Practitioner: unreadable}},
	} {
		t.Run(name, func(t *testing.T) {
			got := read(t, mutate(t, row.edit)).Owner
			if !reflect.DeepEqual(got, row.want) {
				t.Fatalf("owner %+v\nwant %+v", got, row.want)
			}
		})
	}
}

func TestRead_Requester(t *testing.T) {
	for name, row := range map[string]struct {
		edit func(map[string]any)
		want Requester
	}{
		"absent":           {func(m map[string]any) { delete(m, "requester") }, Requester{State: Absent}},
		"null":             {func(m map[string]any) { m["requester"] = nil }, Requester{State: Unreadable}},
		"empty":            {func(m map[string]any) { m["requester"] = map[string]any{} }, Requester{State: Unreadable}},
		"a reference only": {func(m map[string]any) { m["requester"] = map[string]any{"reference": "Organization/p"} }, Requester{State: Present, Reference: "Organization/p"}},
		"not an object":    {func(m map[string]any) { m["requester"] = "Payer123" }, Requester{State: Unreadable}},
		"an identifier given as a list": {func(m map[string]any) {
			m["requester"] = map[string]any{"identifier": []any{map[string]any{"value": "P"}}}
		}, Requester{State: Present, Identifier: unreadable}},
		"a value that is not a string": {func(m map[string]any) {
			m["requester"] = map[string]any{"identifier": map[string]any{"system": "urn:x", "value": 123}}
		}, Requester{State: Present, Identifier: unreadable}},
		"an empty system": {func(m map[string]any) {
			m["requester"] = map[string]any{"identifier": map[string]any{"system": "", "value": "P"}}
		}, Requester{State: Present, Identifier: unreadable}},
		"no system": {func(m map[string]any) { m["requester"] = map[string]any{"identifier": map[string]any{"value": "P"}} },
			Requester{State: Present, Identifier: IdentifierField{State: Present, Identifier: Identifier{Value: "P"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			got := read(t, mutate(t, row.edit)).Requester
			if !reflect.DeepEqual(got, row.want) {
				t.Fatalf("requester %+v\nwant %+v", got, row.want)
			}
		})
	}
}

func TestRead_PayerURL(t *testing.T) {
	payerURLInput := func(value map[string]any) map[string]any {
		in := map[string]any{"type": coding(PASTempCodes, InputPayerURL)}
		for k, v := range value {
			in[k] = v
		}
		return in
	}
	setPayerURL := func(m map[string]any, in any) { inputs(m)[2] = in }
	for name, row := range map[string]struct {
		edit func(map[string]any)
		want PayerURL
	}{
		"no input":    {func(m map[string]any) { delete(m, "input") }, PayerURL{State: Absent, Index: -1}},
		"input null":  {func(m map[string]any) { m["input"] = nil }, PayerURL{State: Unreadable, Index: -1}},
		"input empty": {func(m map[string]any) { m["input"] = []any{} }, PayerURL{State: Unreadable, Index: -1}},
		"no payer-url input": {func(m map[string]any) {
			in := inputs(m)
			m["input"] = append(in[:2:2], in[3:]...)
		}, PayerURL{State: Absent, Index: -1}},
		"the code under another system": {func(m map[string]any) {
			setPayerURL(m, map[string]any{"type": coding(cdexTemp, InputPayerURL), "valueUrl": "https://x"})
		}, PayerURL{State: Absent, Index: -1}},
		"a valueString":                   {func(m map[string]any) { setPayerURL(m, payerURLInput(map[string]any{"valueString": "https://x"})) }, PayerURL{State: Unreadable, Index: 2, Form: "valueString"}},
		"an empty valueUrl":               {func(m map[string]any) { setPayerURL(m, payerURLInput(map[string]any{"valueUrl": ""})) }, PayerURL{State: Unreadable, Index: 2, Form: "valueUrl"}},
		"a null valueUrl":                 {func(m map[string]any) { setPayerURL(m, payerURLInput(map[string]any{"valueUrl": nil})) }, PayerURL{State: Unreadable, Index: 2, Form: "valueUrl"}},
		"a valueUrl that is not a string": {func(m map[string]any) { setPayerURL(m, payerURLInput(map[string]any{"valueUrl": 7})) }, PayerURL{State: Unreadable, Index: 2, Form: "valueUrl"}},
		"no value":                        {func(m map[string]any) { setPayerURL(m, payerURLInput(nil)) }, PayerURL{State: Unreadable, Index: 2}},
		"an extension of the value alone": {func(m map[string]any) {
			setPayerURL(m, payerURLInput(map[string]any{"_valueUrl": map[string]any{"id": "e"}}))
		}, PayerURL{State: Unreadable, Index: 2}},
		"a member that only begins with value": {func(m map[string]any) {
			setPayerURL(m, payerURLInput(map[string]any{"valueUrl": "https://x", "values": "y"}))
		}, PayerURL{State: Present, Value: "https://x", Index: 2, Form: "valueUrl"}},
		"two values": {func(m map[string]any) {
			setPayerURL(m, payerURLInput(map[string]any{"valueUrl": "https://x", "valueString": "https://y"}))
		}, PayerURL{State: Unreadable, Index: 2}},
		"two payer-url inputs": {func(m map[string]any) {
			m["input"] = append(inputs(m), payerURLInput(map[string]any{"valueUrl": "https://y"}))
		}, PayerURL{State: Unreadable, Index: -1}},
		"another input whose type is unreadable": {func(m map[string]any) {
			inputs(m)[0].(map[string]any)["type"] = "attachments-needed"
		}, PayerURL{State: Unreadable, Index: -1}},
		"another input whose type is null": {func(m map[string]any) {
			inputs(m)[0].(map[string]any)["type"] = nil
		}, PayerURL{State: Unreadable, Index: -1}},
		"another input with no type": {func(m map[string]any) {
			delete(inputs(m)[0].(map[string]any), "type")
		}, PayerURL{State: Unreadable, Index: -1}},
		"another input whose coding is null": {func(m map[string]any) {
			inputs(m)[0].(map[string]any)["type"] = map[string]any{"coding": nil}
		}, PayerURL{State: Unreadable, Index: -1}},
		"an input that is not an object": {func(m map[string]any) { m["input"] = append(inputs(m), "payer-url") }, PayerURL{State: Unreadable, Index: -1}},
		"an input that is null":          {func(m map[string]any) { m["input"] = append(inputs(m), nil) }, PayerURL{State: Unreadable, Index: -1}},
		"input not a list":               {func(m map[string]any) { m["input"] = inputs(m)[2] }, PayerURL{State: Unreadable, Index: -1}},
		"its display, not its code": {func(m map[string]any) {
			setPayerURL(m, map[string]any{"type": map[string]any{"coding": []any{map[string]any{"system": PASTempCodes, "code": "other", "display": InputPayerURL}}, "text": InputPayerURL}, "valueUrl": "https://x"})
		}, PayerURL{State: Absent, Index: -1}},
		"a primitive extension beside the valueUrl": {func(m map[string]any) {
			setPayerURL(m, payerURLInput(map[string]any{"valueUrl": "https://x", "_valueUrl": map[string]any{"id": "e"}}))
		}, PayerURL{State: Present, Value: "https://x", Index: 2, Form: "valueUrl"}},
		// A type coding R4 allows without a system is passed over: it cannot
		// be the payer-url, and the payer-url beside it is still read.
		"another input's type coding without a system": {func(m map[string]any) {
			inputs(m)[0].(map[string]any)["type"] = map[string]any{"coding": []any{map[string]any{"code": "attachments-needed"}}}
		}, PayerURL{State: Present, Value: example19URL, Index: 2, Form: "valueUrl"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := read(t, mutate(t, row.edit)).PayerURL
			if got != row.want {
				t.Fatalf("payer-url %+v\nwant %+v", got, row.want)
			}
		})
	}
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{Absent: "absent", Unreadable: "unreadable", Present: "present", State(9): "State(9)"} {
		if s.String() != want {
			t.Errorf("%d: %q, want %q", s, s.String(), want)
		}
	}
}

// A member the reader reads, named in another case, fails the whole read
// whatever its siblings hold: an unreadable item before it never stops it
// from being found.
func TestRead_ReadsTwoWaysWhateverItsSiblings(t *testing.T) {
	badType := map[string]any{"coding": []any{map[string]any{"system": "urn:x", "code": 7}}}
	for name, edit := range map[string]func(map[string]any){
		"a bad input type before a payer-url input naming ValueUrl": func(m map[string]any) {
			inputs(m)[0].(map[string]any)["type"] = badType
			in := inputs(m)[2].(map[string]any)
			in["ValueUrl"] = in["valueUrl"]
			delete(in, "valueUrl")
		},
		"two payer-url inputs, the second naming ValueUrl": func(m map[string]any) {
			m["input"] = append(inputs(m), map[string]any{"type": coding(PASTempCodes, InputPayerURL), "ValueUrl": "https://elsewhere"})
		},
		"ValueUrl on an input of another type": func(m map[string]any) {
			inputs(m)[0].(map[string]any)["ValueUrl"] = "https://elsewhere"
		},
		"a bad coding before a coding naming System": func(m map[string]any) {
			m["code"] = map[string]any{"coding": []any{map[string]any{"system": 1, "code": "x"}, map[string]any{"System": PASTempCodes, "code": CodeAttachmentRequest}}}
		},
		"a bad contained resource before one naming ID": func(m map[string]any) {
			m["contained"] = append([]any{nil, map[string]any{"resourceType": 1}}, append(m["contained"].([]any), map[string]any{"resourceType": "Organization", "ID": "o"})...)
		},
		"a bad patient identifier before one naming Value": func(m map[string]any) {
			contained(m, "patient")["identifier"] = []any{map[string]any{"system": "urn:x"}, map[string]any{"system": "urn:x", "Value": "M"}}
		},
		"an unreadable reference before its identifier naming Value": func(m map[string]any) {
			m["requester"] = map[string]any{"reference": 7, "identifier": map[string]any{"Value": "P"}}
		},
		"an identifier with no value before its type naming System": func(m map[string]any) {
			m["requester"] = map[string]any{"identifier": map[string]any{"type": map[string]any{"coding": []any{map[string]any{"System": "urn:x", "code": "y"}}}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Read(relay.NewBody(mutate(t, edit), relay.OriginIngressRequest))
			if !errors.Is(err, relay.ErrFieldNameCase) || !reflect.DeepEqual(got, Task{}) {
				t.Fatalf("read %+v, err %v; want relay.ErrFieldNameCase and nothing read", got, err)
			}
		})
	}
}
