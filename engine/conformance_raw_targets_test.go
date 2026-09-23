package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// PCV-04: a successful validator result must describe the exact source
// occurrence, including member order and JSON lexemes, rather than a rebuilt
// semantic twin. Two sibling resources can decode to the same object.
func TestDeepValidationUsesExactSourceTargetBytes(t *testing.T) {
	const first = `{"resourceType":"Patient","id":"same","note":"\u0061","number":9007199254740993.2300}`
	const second = `{"number":9007199254740993.2300,"note":"a","id":"same","resourceType":"Patient"}`
	const inquiryFirst = `{"resourceType":"Bundle","type":"collection","entry":[],"note":"\u0061"}`
	const inquirySecond = `{"note":"a","entry":[],"type":"collection","resourceType":"Bundle"}`
	rows := []struct {
		name string
		in   CheckInput
		want [][]byte
	}{
		{"root", deepInput(" \n" + inquiryFirst + "\t"), [][]byte{[]byte(" \n" + inquiryFirst + "\t")}},
		{"crd siblings", CheckInput{Direction: "request", DeclaredVersion: "pa.crd@2.0", Exchange: ExchangeContext{legType: "crd-order-select"}, Body: []byte(`{"hook":"order-select","context":{},"prefetch":{"b":` + second + `,"a":` + first + `}}`)}, [][]byte{[]byte(first), []byte(second)}},
		{"crd response actions", CheckInput{Direction: "response", DeclaredVersion: "pa.crd@2.0", Exchange: ExchangeContext{legType: "crd-order-select"}, Body: []byte(`{"cards":[{"suggestions":[{"actions":[{"resource":` + second + `}]}]}],"systemActions":[{"resource":` + first + `}]}`)}, [][]byte{[]byte(first), []byte(second)}},
		{"inquiry returns", inquiryDeepInput(`{"resourceType":"Parameters","parameter":[{"name":"return","resource":`+inquiryFirst+`},{"name":"return","resource":`+inquirySecond+`}]}`, "pa.pas@2.2"), [][]byte{[]byte(inquiryFirst), []byte(inquirySecond)}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var seen [][]byte
			v := observationValidator(func(_ context.Context, body []byte, _ string) (shnsdk.ValidationEvidence, error) {
				seen = append(seen, bytes.Clone(body))
				return *syntheticEvidence(), nil
			})
			g := &Gateway{cfg: Config{Validator: v}}
			in := row.in
			if row.name == "root" {
				in.DeclaredVersion = "pa.pas@2.0"
			}
			budget := &observationBudget{}
			in.observation = budget
			if got := deepRule(t, g, "fhir.profile").Check(context.Background(), in); got.State != CheckValid {
				t.Fatalf("profile outcome: %+v", got)
			}
			if len(seen) != len(row.want) {
				t.Fatalf("validator calls=%d want %d", len(seen), len(row.want))
			}
			for i := range seen {
				if !bytes.Equal(seen[i], row.want[i]) {
					t.Fatalf("target %d was reserialized: got %s want %s", i, seen[i], row.want[i])
				}
			}
			if budget.bytes != 0 {
				t.Fatalf("observation reservation leaked %d bytes", budget.bytes)
			}
		})
	}
}

// A prefetch null is not a FHIR target. The scanner's limit must agree with
// crdResources, which counts only resources actually sent to the validator.
func TestDeepValidationPrefetchNullsDoNotConsumeTargetCap(t *testing.T) {
	const bundle = `{"resourceType":"Bundle","type":"collection","entry":[]}`
	for _, count := range []int{16, 17} {
		t.Run(fmt.Sprintf("nulls-%d", count), func(t *testing.T) {
			entries := make([]string, count)
			for i := range entries {
				entries[i] = fmt.Sprintf(`"optional-%02d":null`, i)
			}
			body := []byte(`{"context":{"draftOrders":` + bundle + `},"prefetch":{` + strings.Join(entries, ",") + `}}`)
			assertOneExactCRDTarget(t, body, []byte(bundle))
		})
	}
}

func TestDeepValidationJSONEscapedKeysRetainExactTarget(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"p","note":"\u0061"}`
	for _, key := range []string{`"a/b"`, `"a\/b"`, `"\ud800"`, `"pat\u0069ent"`} {
		t.Run(key, func(t *testing.T) {
			body := []byte(`{"prefetch":{` + key + `:null,"resource":` + patient + `}}`)
			assertOneExactCRDTarget(t, body, []byte(patient))
		})
	}
}

func TestDeepValidationCapsActualCRDTargets(t *testing.T) {
	for _, count := range []int{16, 17} {
		t.Run(fmt.Sprintf("resources-%d", count), func(t *testing.T) {
			entries := make([]string, count)
			for i := range entries {
				entries[i] = fmt.Sprintf(`"resource-%02d":{"resourceType":"Patient","id":"p"}`, i)
			}
			in := CheckInput{Direction: "request", DeclaredVersion: "pa.crd@2.0", Exchange: ExchangeContext{legType: "crd-order-select"}, Body: []byte(`{"prefetch":{` + strings.Join(entries, ",") + `}}`)}
			calls := 0
			v := observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
				calls++
				return *syntheticEvidence(), nil
			})
			g := &Gateway{cfg: Config{Validator: v}}
			got := deepRule(t, g, "fhir.profile").Check(context.Background(), in)
			if count == 16 && (got.State != CheckValid || calls != 16) {
				t.Fatalf("supported target cap: state=%s calls=%d", got.State, calls)
			}
			if count == 17 && (got.State != CheckUnavailable || calls != 0) {
				t.Fatalf("excess targets certified: state=%s calls=%d", got.State, calls)
			}
		})
	}
}

func assertOneExactCRDTarget(t *testing.T, body, want []byte) {
	t.Helper()
	var seen [][]byte
	v := observationValidator(func(_ context.Context, got []byte, _ string) (shnsdk.ValidationEvidence, error) {
		seen = append(seen, bytes.Clone(got))
		return *syntheticEvidence(), nil
	})
	g := &Gateway{cfg: Config{Validator: v}}
	in := CheckInput{Direction: "request", DeclaredVersion: "pa.crd@2.0", Exchange: ExchangeContext{legType: "crd-order-select"}, Body: body, observation: &observationBudget{}}
	if got := deepRule(t, g, "fhir.profile").Check(context.Background(), in); got.State != CheckValid {
		t.Fatalf("profile outcome: %+v", got)
	}
	if len(seen) != 1 || !bytes.Equal(seen[0], want) {
		t.Fatalf("validator target count=%d bytes=%q want=%q", len(seen), seen, want)
	}
	if in.observation.bytes != 0 {
		t.Fatalf("target reservation leaked %d bytes", in.observation.bytes)
	}
}

func TestDeepValidationDoesNotCertifyAmbiguousDuplicateTarget(t *testing.T) {
	calls := 0
	v := observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
		calls++
		return *syntheticEvidence(), nil
	})
	g := &Gateway{cfg: Config{Validator: v}}
	in := deepInput(`{"resourceType":"Bundle","resourceType":"Bundle","entry":[]}`)
	if got := deepRule(t, g, "fhir.profile").Check(context.Background(), in); got.State != CheckUnavailable || calls != 0 {
		t.Fatalf("ambiguous duplicate was certified: %+v calls=%d", got, calls)
	}
}
