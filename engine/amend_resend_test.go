package engine

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// newResendGateway returns a gateway minting corr-1, corr-2, … and recording
// its observer events.
func newResendGateway(events *[]ObserverEvent) *Gateway {
	n := 0
	return &Gateway{cfg: Config{
		CorrelationGen: func() string { n++; return fmt.Sprintf("corr-%d", n) },
		Clock:          time.Now,
		Observer:       func(e ObserverEvent) { *events = append(*events, e) },
	}}
}

// sendAmendment re-sends only on a recipient's 409: every other outcome of the
// first attempt is the caller's to relay, after that one attempt.
func TestSendAmendmentResendsOnlyOn409(t *testing.T) {
	for _, row := range []struct {
		name    string
		handled bool
		err     error
	}{
		{"answered", false, nil},
		{"refused-422", false, &RelayError{Status: http.StatusUnprocessableEntity}},
		{"refused-412", false, &RelayError{Status: http.StatusPreconditionFailed}},
		{"stopped-before-sending", true, nil},
		{"transport-failure", false, errors.New("hub unreachable")},
	} {
		t.Run(row.name, func(t *testing.T) {
			var events []ObserverEvent
			g := newResendGateway(&events)
			var sent []string
			corr, _, _, handled, err := g.sendAmendment("payer", func(corr string) ([]byte, []byte, bool, error) {
				sent = append(sent, corr)
				return nil, nil, row.handled, row.err
			})
			if len(sent) != 1 || corr != "corr-1" || handled != row.handled || err != row.err || len(events) != 0 {
				t.Fatalf("want one attempt, its own outcome, no event: sent=%v corr=%s handled=%v err=%v events=%+v", sent, corr, handled, err, events)
			}
		})
	}
}

// A 409 is re-sent once under a new correlation id, and the answer to the
// re-send, a second 409 included, is what the caller relays.
func TestSendAmendmentResendsA409Once(t *testing.T) {
	for _, row := range []struct {
		name   string
		second error
	}{
		{"re-send-answered", nil},
		{"second-409-relayed", &RelayError{Status: http.StatusConflict}},
	} {
		t.Run(row.name, func(t *testing.T) {
			var events []ObserverEvent
			g := newResendGateway(&events)
			var sent []string
			corr, _, resp, handled, err := g.sendAmendment("payer", func(corr string) ([]byte, []byte, bool, error) {
				sent = append(sent, corr)
				if len(sent) == 1 {
					return nil, nil, false, &RelayError{Status: http.StatusConflict}
				}
				if row.second != nil {
					return nil, nil, false, row.second
				}
				return nil, []byte("answer"), false, nil
			})
			if len(sent) != 2 || sent[0] != "corr-1" || sent[1] != "corr-2" || corr != "corr-2" || handled {
				t.Fatalf("want exactly two attempts, corr-1 then corr-2, returning corr-2: sent=%v corr=%s handled=%v", sent, corr, handled)
			}
			if err != row.second || (row.second == nil && string(resp) != "answer") {
				t.Fatalf("the re-send's own outcome must be returned: resp=%q err=%v", resp, err)
			}
			want := ObserverEvent{Kind: LegResentEvent, LegType: "pas-claim-update", Direction: "originate", CorrelationID: "corr-2", Counterpart: "payer", Status: http.StatusConflict, Detail: `{"refusedCorrelationId":"corr-1","status":409}`}
			if len(events) != 1 {
				t.Fatalf("want one %s event, got %+v", LegResentEvent, events)
			}
			events[0].Time = time.Time{}
			if !reflect.DeepEqual(events[0], want) {
				t.Fatalf("%s event:\n got %+v\nwant %+v", LegResentEvent, events[0], want)
			}
		})
	}
}
