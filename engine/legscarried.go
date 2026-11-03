package engine

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

// legsCarried is the legs one call to an exchange route sent and had answered
// 2xx, in the order they were answered. A refusal a route writes after some of
// them says which: they were carried, and they stand.
type legsCarried struct {
	mu   sync.Mutex
	legs []string
}

type legsCarriedKey struct{}

// tracksLegsCarried gives each call to h its own legsCarried, which roundTrip
// adds to (carriedLeg). routes() puts every exchange route behind it.
func tracksLegsCarried(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), legsCarriedKey{}, &legsCarried{})))
	}
}

// carriedLeg records that the call ctx belongs to sent a leg of txType and
// had it answered 2xx. A context with no tracker records nothing.
func carriedLeg(ctx context.Context, txType string) {
	c, _ := ctx.Value(legsCarriedKey{}).(*legsCarried)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.legs = append(c.legs, txType)
}

// carriedLegsStatement says, for a refusal written after the call may have
// sent legs, which it sent and had answered: they were carried and stand, so
// the caller can tell a refusal at a later leg from one before any leg. A call
// with no tracker (a handler driven directly) cannot say which, and says so
// rather than claim that nothing was sent.
func carriedLegsStatement(ctx context.Context) string {
	c, _ := ctx.Value(legsCarriedKey{}).(*legsCarried)
	if c == nil {
		return "Any leg this request sent before this point was carried and stands."
	}
	c.mu.Lock()
	legs := append([]string(nil), c.legs...)
	c.mu.Unlock()
	switch len(legs) {
	case 0:
		return "No leg of this request was sent."
	case 1:
		return "The " + legs[0] + " leg this request sent was carried and stands."
	default:
		return "The " + strings.Join(legs[:len(legs)-1], ", ") + " and " + legs[len(legs)-1] + " legs this request sent were carried and stand."
	}
}

// withCarriedLegs is msg, a refusal's or a failure's own text, followed by
// carriedLegsStatement as a sentence of its own. A msg that already ends with
// it (a step that said so before its caller answers) is returned as it is: the
// statement is made once.
func withCarriedLegs(ctx context.Context, msg string) string {
	stmt := carriedLegsStatement(ctx)
	if strings.HasSuffix(msg, " "+stmt) || msg == stmt {
		return msg
	}
	msg = strings.TrimRight(msg, " ")
	if msg != "" && !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg + " " + stmt
}
