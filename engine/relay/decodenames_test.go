package relay

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	fhir "github.com/samply/golang-fhir-models/fhir-models/fhir"
)

type nameRef struct {
	Reference string `json:"reference"`
}

type nameEmbedded struct {
	Status string `json:"status"`
}

// NameInner is exported so encoding/json can allocate it through the
// embedded pointer.
type NameInner struct {
	System string `json:"system,omitempty"`
}

type nameClaim struct {
	nameEmbedded
	*NameInner
	ResourceType string               `json:"resourceType"`
	Patient      *nameRef             `json:"patient,omitempty"`
	Items        []struct{ nameItem } `json:"item"`
	Fixed        [1]nameRef           `json:"fixed"`
	ByKey        map[string]nameRef   `json:"byKey"`
	Raw          json.RawMessage      `json:"raw"`
	Custom       caseLooseValue       `json:"custom"`
	Any          any                  `json:"any"`
	Ignored      string               `json:"-"`
	Dash         string               `json:"-,"`
	Untagged     string
	unexported   string // not a member name
}

type nameItem struct {
	Sequence int `json:"sequence"`
}

// caseLooseValue decodes itself and reads its members however it likes;
// its subtree is not inspected (see Decode).
type caseLooseValue struct{ got map[string]any }

func (c *caseLooseValue) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &c.got) }

// TestDecodeRefusesCaseMismatchedFieldNames: encoding/json matches member
// names to struct fields ignoring case, while a recipient reading the same
// bytes matches them exactly. A member that reaches a field only by case
// folding is refused, so what the gateway decides on is what the recipient
// reads.
func TestDecodeRefusesCaseMismatchedFieldNames(t *testing.T) {
	refused := []struct {
		name, src, member, path string
		into                    func() any
	}{
		{"top level", `{"resourceType":"Claim","Patient":{"reference":"Patient/a"}}`, "Patient", "$", func() any { return new(nameClaim) }},
		{"nested object", `{"patient":{"Reference":"Patient/a"}}`, "Reference", "$.patient", func() any { return new(nameClaim) }},
		{"slice element", `{"item":[{"sequence":1},{"Sequence":2}]}`, "Sequence", "$.item[1]", func() any { return new(nameClaim) }},
		{"array element", `{"fixed":[{"REFERENCE":"x"}]}`, "REFERENCE", "$.fixed[0]", func() any { return new(nameClaim) }},
		{"map value", `{"byKey":{"Any-Key":{"reference":"a"},"b":{"refeRence":"b"}}}`, "refeRence", `$.byKey["b"]`, func() any { return new(nameClaim) }},
		{"promoted from an embedded struct", `{"Status":"active"}`, "Status", "$", func() any { return new(nameClaim) }},
		{"promoted from an embedded pointer", `{"SYSTEM":"x"}`, "SYSTEM", "$", func() any { return new(nameClaim) }},
		{"untagged field", `{"untagged":"x"}`, "untagged", "$", func() any { return new(nameClaim) }},
		{"Kelvin sign", `{"` + string(rune(0x212A)) + `ey":1}`, string(rune(0x212A)) + "ey", "$", func() any {
			return new(struct {
				Key int `json:"key"`
			})
		}},
		{"pointer to pointer", `{"Patient":{}}`, "Patient", "$", func() any { p := new(nameClaim); return &p }},
		{"interface holding a pointer", `{"Patient":{}}`, "Patient", "$", func() any { var v any = new(nameClaim); return &v }},
		{"FHIR Claim", `{"resourceType":"Claim","status":"active","Patient":{"reference":"Patient/a"}}`, "Patient", "$", func() any { return new(fhir.Claim) }},
		{"FHIR Claim item", `{"resourceType":"Claim","item":[{"sequence":1,"ProductOrService":{}}]}`, "ProductOrService", "$.item[0]", func() any { return new(fhir.Claim) }},
		{"FHIR Bundle entry", `{"resourceType":"Bundle","entry":[{"fullUrl":"urn:a","Resource":{}}]}`, "Resource", "$.entry[0]", func() any { return new(fhir.Bundle) }},
	}
	for _, r := range refused {
		t.Run(r.name, func(t *testing.T) {
			err := Decode(NewBody([]byte(r.src), OriginPeerFrame), r.into())
			var fe *FieldNameError
			if !errors.As(err, &fe) || !errors.Is(err, ErrFieldNameCase) {
				t.Fatalf("want *FieldNameError, got %v", err)
			}
			if errors.Is(err, ErrDuplicateKey) || errors.Is(err, ErrInvalidJSON) || errors.Is(err, ErrTooLarge) {
				t.Fatalf("a case mismatch is its own error: %v", err)
			}
			if fe.Member != r.member || fe.Path != r.path || !strings.EqualFold(fe.Field, r.member) || fe.Field == r.member {
				t.Fatalf("error %+v", fe)
			}
			if !strings.Contains(err.Error(), "does not match field") {
				t.Fatalf("message %q", err)
			}
		})
	}

	t.Run("nothing is decoded when refused", func(t *testing.T) {
		var c nameClaim
		err := Decode(NewBody([]byte(`{"resourceType":"Claim","Patient":{"reference":"Patient/a"}}`), OriginPeerFrame), &c)
		if !errors.Is(err, ErrFieldNameCase) || c.ResourceType != "" || c.Patient != nil {
			t.Fatalf("%v %+v", err, c)
		}
	})

	accepted := []struct {
		name, src string
		into      any
		check     func(t *testing.T, v any)
	}{
		{"exact names", `{"resourceType":"Claim","status":"active","system":"s","patient":{"reference":"Patient/a"},"item":[{"sequence":2}],"fixed":[{"reference":"f"}],"byKey":{"K":{"reference":"k"}}}`,
			new(nameClaim), func(t *testing.T, v any) {
				c := v.(*nameClaim)
				if c.Patient.Reference != "Patient/a" || c.Items[0].Sequence != 2 || c.Status != "active" ||
					c.System != "s" || c.Fixed[0].Reference != "f" || c.ByKey["K"].Reference != "k" {
					t.Fatalf("%+v", c)
				}
			}},
		{"unknown members are ignored", `{"resourceType":"Claim","extension":[{"URL":"x"}],"Ignored":"y","UNEXPORTED":1}`,
			new(nameClaim), func(t *testing.T, v any) {
				if c := v.(*nameClaim); c.ResourceType != "Claim" || c.Ignored != "" {
					t.Fatalf("%+v", c)
				}
			}},
		{"a field named by tag with a dash", `{"-":"d"}`, new(nameClaim), func(t *testing.T, v any) {
			if v.(*nameClaim).Dash != "d" {
				t.Fatal("dash field not decoded")
			}
		}},
		{"raw values are not inspected", `{"raw":{"Patient":1}}`, new(nameClaim), func(t *testing.T, v any) {
			if string(v.(*nameClaim).Raw) != `{"Patient":1}` {
				t.Fatal("raw not kept")
			}
		}},
		{"generic values are not inspected", `{"any":{"Patient":1},"custom":{"PATIENT":2}}`, new(nameClaim), func(t *testing.T, v any) {
			c := v.(*nameClaim)
			if c.Any.(map[string]any)["Patient"] != json.Number("1") || c.Custom.got["PATIENT"] == nil {
				t.Fatalf("%+v", c)
			}
		}},
		{"map keys are data", `{"byKey":{"Reference":{"reference":"r"}}}`, new(nameClaim), func(t *testing.T, v any) {
			if v.(*nameClaim).ByKey["Reference"].Reference != "r" {
				t.Fatal("map key lost")
			}
		}},
		{"a scalar where a struct is expected is the decoder's error", `{"patient":"x"}`, new(nameClaim), nil},
		{"into a map", `{"Patient":1}`, new(map[string]any), nil},
		{"into an interface", `{"Patient":1}`, new(any), nil},
		{"FHIR Claim", `{"resourceType":"Claim","status":"active","patient":{"reference":"Patient/a"},"item":[{"sequence":1}]}`,
			new(fhir.Claim), func(t *testing.T, v any) {
				c := v.(*fhir.Claim)
				if c.Patient.Reference == nil || *c.Patient.Reference != "Patient/a" || c.Item[0].Sequence != 1 {
					t.Fatalf("%+v", c)
				}
			}},
		{"FHIR code enum decodes itself", `{"resourceType":"Claim","status":"active"}`, new(fhir.Claim), func(t *testing.T, v any) {
			if v.(*fhir.Claim).Status != fhir.FinancialResourceStatusCodesActive {
				t.Fatal("status not decoded")
			}
		}},
	}
	for _, a := range accepted {
		t.Run("accepts "+a.name, func(t *testing.T) {
			err := Decode(NewBody([]byte(a.src), OriginPeerFrame), a.into)
			if errors.Is(err, ErrFieldNameCase) {
				t.Fatalf("refused: %v", err)
			}
			if a.check == nil {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			a.check(t, a.into)
		})
	}

	t.Run("a non-pointer target is the decoder's error", func(t *testing.T) {
		var c nameClaim
		err := Decode(NewBody([]byte(`{"Patient":{}}`), OriginPeerFrame), c)
		var ie *json.InvalidUnmarshalError
		if !errors.As(err, &ie) {
			t.Fatalf("got %v", err)
		}
		if err := Decode(NewBody([]byte(`{}`), OriginPeerFrame), nil); !errors.As(err, &ie) {
			t.Fatalf("nil target: %v", err)
		}
	})
}

// NameLeft and NameRight are exported so reflect.StructOf can embed them.
type NameLeft struct {
	Code string `json:"code"`
}

type NameRight struct {
	Code string `json:"code"`
}

// TestDecodeFieldNamesFollowTheDecoder pins the cases where the check must
// agree with encoding/json about which field, if any, a member reaches.
func TestDecodeFieldNamesFollowTheDecoder(t *testing.T) {
	// Structs whose json names collide are built at run time, so the
	// collisions are deliberate test input rather than declarations.
	left, right := reflect.TypeFor[NameLeft](), reflect.TypeFor[NameRight]()
	ambiguous := reflect.StructOf([]reflect.StructField{
		{Name: "NameLeft", Type: left, Anonymous: true},
		{Name: "NameRight", Type: right, Anonymous: true},
	})
	shallowWins := reflect.StructOf([]reflect.StructField{
		{Name: "NameLeft", Type: left, Anonymous: true},
		{Name: "Plain", Type: reflect.TypeFor[string](), Tag: `json:"code"`},
	})
	t.Run("an ambiguous name reaches no field", func(t *testing.T) {
		src := []byte(`{"CODE":"x"}`)
		viaJSON := reflect.New(ambiguous)
		if err := json.Unmarshal(src, viaJSON.Interface()); err != nil || !viaJSON.Elem().IsZero() {
			t.Fatalf("decoder premise: %v %+v", err, viaJSON.Elem())
		}
		if err := Decode(NewBody(src, OriginPeerFrame), reflect.New(ambiguous).Interface()); err != nil {
			t.Fatalf("an ignored member was refused: %v", err)
		}
	})
	t.Run("a shallower field wins over an embedded one", func(t *testing.T) {
		src := []byte(`{"Code":"x"}`)
		viaJSON := reflect.New(shallowWins)
		if err := json.Unmarshal(src, viaJSON.Interface()); err != nil || viaJSON.Elem().Field(1).String() != "x" {
			t.Fatalf("decoder premise: %v %+v", err, viaJSON.Elem())
		}
		err := Decode(NewBody(src, OriginPeerFrame), reflect.New(shallowWins).Interface())
		var fe *FieldNameError
		if !errors.As(err, &fe) || fe.Field != "code" || fe.Path != "$" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("spare slice capacity holding a pointer is followed", func(t *testing.T) {
		src := []byte(`[{"Patient":{}}]`)
		backing := []any{new(nameClaim)}
		viaJSON := backing[:0]
		if err := json.Unmarshal(src, &viaJSON); err != nil || backing[0].(*nameClaim).Patient == nil {
			t.Fatalf("decoder premise: %v", err)
		}
		backing = []any{new(nameClaim)}
		v := backing[:0]
		err := Decode(NewBody(src, OriginPeerFrame), &v)
		if !errors.Is(err, ErrFieldNameCase) || backing[0].(*nameClaim).Patient != nil {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("elements past a fixed array are skipped", func(t *testing.T) {
		var v struct {
			Fixed [1]nameRef `json:"fixed"`
		}
		if err := Decode(NewBody([]byte(`{"fixed":[{"reference":"a"},{"Reference":"b"}]}`), OriginPeerFrame), &v); err != nil || v.Fixed[0].Reference != "a" {
			t.Fatalf("got %v %+v", err, v)
		}
	})
	t.Run("a non-name tag falls back to the field name", func(t *testing.T) {
		var v struct {
			Odd string `json:"a\\b"`
		}
		err := Decode(NewBody([]byte(`{"odd":"x"}`), OriginPeerFrame), &v)
		var fe *FieldNameError
		if !errors.As(err, &fe) || fe.Field != "Odd" {
			t.Fatalf("got %v", err)
		}
	})
}

// TestDecodeValueRefusesCaseMismatchedFieldNames: the same rule applies to
// a value read from a Document.
func TestDecodeValueRefusesCaseMismatchedFieldNames(t *testing.T) {
	d := mustDoc(t, NewBody([]byte(`{"entry":[{"resource":{"Patient":{}}}]}`), OriginPeerFrame))
	var c nameClaim
	err := d.DecodeValue(at(t, d, "entry", "0", "resource"), &c)
	var fe *FieldNameError
	if !errors.As(err, &fe) || fe.Path != "$" {
		t.Fatalf("got %v", err)
	}
}
