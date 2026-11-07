package relay

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Then edits a payload further: the second change is checked against the
// first one's bytes, and the result keeps both edits in order. An empty or
// unchanging second change returns the payload as it was; one that breaks a
// rule is refused; a payload the gateway built takes no further edit.
func TestThen_KeepsEveryEdit(t *testing.T) {
	body := adaptiveBundle(`"valueUrl"  :  `+payerAddress, "")
	d := mustDoc(t, body)
	first, err := Apply(body, fhirJSON, EditDTRAnswerLineNames, d.Replace(at(t, d, "entry", "0", "resource", "status"), []byte(`"retired"`)))
	if err != nil {
		t.Fatal(err)
	}
	both, err := Then(first, OriginPeerFrame, func(d *Document) ([]Change, error) {
		return []Change{{Edit: EditDTRAdaptiveAddressRekey, Ops: []Op{d.Replace(at(t, d, append(slices.Clone(adaptivePath), "valueUrl")...), []byte(networkAddress))}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(BytesForTest(both))
	if !slices.Equal(both.Edits(), []EditID{EditDTRAnswerLineNames, EditDTRAdaptiveAddressRekey}) || !strings.Contains(got, `"retired"`) || !strings.Contains(got, networkAddress) {
		t.Fatalf("edits %v, bytes %s", both.Edits(), got)
	}
	if same, err := Then(first, OriginPeerFrame, func(*Document) ([]Change, error) { return nil, nil }); err != nil || !slices.Equal(same.Edits(), first.Edits()) {
		t.Fatalf("no change: %v %v", same.Edits(), err)
	}
	if _, err := Then(first, OriginPeerFrame, func(d *Document) ([]Change, error) {
		return []Change{{Edit: EditDTRAnswerLineNames, Ops: []Op{d.Replace(at(t, d, append(slices.Clone(adaptivePath), "valueUrl")...), []byte(networkAddress))}}}, nil
	}); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("another edit re-pointing the address: %v, want refused", err)
	}
	built, err := Authored(BuilderGatewayRefusal, []byte(`{}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Then(built, OriginPeerFrame, func(*Document) ([]Change, error) { return nil, nil }); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("a built payload: %v, want refused", err)
	}
}
