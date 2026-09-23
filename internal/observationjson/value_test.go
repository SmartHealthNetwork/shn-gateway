package observationjson

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodedSerializationExactAndBounded(t *testing.T) {
	for _, raw := range []string{`{"nested":[null,true,false,{},[],{"line":"\b\f\n\r\t\u0000<&>\u2028\u2029\\\""}],"numbers":[0,-0,1.2500,1e+99,-1E-2,123456789012345678901234567890]}`, `["�","💉","a"]`} {
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(v)
		n, ok := Size(v, len(want))
		if !ok || n != len(want) {
			t.Fatalf("size %d/%d %v", n, len(want), ok)
		}
		if _, ok := Size(v, n-1); ok {
			t.Fatal("exceeded byte budget")
		}
		got := Append(make([]byte, 0, n), v)
		if !bytes.Equal(got, want) || cap(got) != n {
			t.Fatalf("got=%s want=%s", got, want)
		}
	}
	for _, v := range []any{1.2, struct{}{}, json.Number("01"), json.Number("NaN"), json.Number("1e"), json.Number("")} {
		if _, ok := Size(v, 100); ok {
			t.Fatalf("accepted unsupported %T %v", v, v)
		}
	}
}
