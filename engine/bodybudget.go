package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// bodybudget.go bounds the large bodies a gateway holds at once. One
// documentation attachment costs several times its bytes while it is read,
// sealed and carried, and the network's limits bound each request, not how many
// arrive together. A request that does not fit is refused at once, unread past
// the line it is counted at; it never waits, so a caller is never held queueing.

// defaultAttachmentInflightBytes is the budget when nothing states one. The
// gateway's runtime states its own from the task's memory.
const defaultAttachmentInflightBytes = 64 << 20

// bodyBudgetRetryAfter is the Retry-After, in seconds, of a refusal for the
// budget: a first wait, since a held body frees its bytes when its exchange
// ends.
const bodyBudgetRetryAfter = 5

// refusalAtCapacity is a recipient's answer when it has no room for a large
// envelope now.
const refusalAtCapacity = "large envelope refused: this gateway holds as many as it can"

// bodyBudget counts the bytes held against a limit.
type bodyBudget struct {
	mu          sync.Mutex
	limit, held int64
}

// acquire takes n bytes if they fit, and returns what gives them back. n is
// never counted above the limit, so the largest request the network allows
// always fits an idle gateway.
func (b *bodyBudget) acquire(n int64) (release func(), ok bool) {
	n = min(n, b.limit)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.held+n > b.limit {
		return nil, false
	}
	b.held += n
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.held -= n
	}, true
}

// attachmentInflight is the budget Config states, or the default; never below
// the attachment ceiling, so a full submission always fits.
func attachmentInflight(cfg Config) int64 {
	limit, ceiling := cfg.AttachmentInflightBytes, cfg.AttachmentCeiling
	if limit <= 0 {
		limit = defaultAttachmentInflightBytes
	}
	if ceiling <= 0 {
		ceiling = shnsdk.MaxAttachmentBytes
	}
	return max(limit, ceiling)
}

// readInboundEnvelope reads a delivered envelope up to the largest limit of any
// leg, holding the budget for one over the network's limit for other legs. It
// returns the body and the release the caller defers for as long as it holds
// the envelope; held false means the budget had no room and nothing more was
// read. A stated length takes that length before anything is read. A body with
// no stated length, a gzip-coded one among them, is read to the line uncounted;
// past it, it takes the full cap, since nothing says how much more is coming.
// An envelope at or under the line is never counted, so large envelopes cannot
// shed an ordinary leg.
func (g *Gateway) readInboundEnvelope(r *http.Request) (body []byte, release func(), held bool, err error) {
	release = func() {}
	var rd io.Reader = r.Body
	switch {
	case r.ContentLength > shnsdk.MaxRequestBytes:
		var ok bool
		if release, ok = g.bodies.acquire(min(r.ContentLength, int64(maxEnvelopeBytes))); !ok {
			return nil, nil, false, nil
		}
	case r.ContentLength < 0:
		head, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes+1))
		if err != nil {
			return nil, release, true, err
		}
		if len(head) <= shnsdk.MaxRequestBytes {
			return head, release, true, nil
		}
		var ok bool
		if release, ok = g.bodies.acquire(int64(maxEnvelopeBytes)); !ok {
			return nil, nil, false, nil
		}
		rd = io.MultiReader(bytes.NewReader(head), r.Body)
	}
	body, err = readCapped(rd, int64(maxEnvelopeBytes))
	return body, release, true, err
}

// refuseAtCapacity is the recipient's refusal when the budget has no room: 503,
// uncacheable, with Retry-After and the attested NotActedHeader. It is a
// routing refusal, as the size refusals are, and a transient one: this body
// cannot be carried right now, and nothing was done with it.
func refuseAtCapacity(ctx context.Context, w http.ResponseWriter, fence *notActedFence) {
	exchangeOf(ctx).refused(RefusalRouting)
	fence.attested = true
	w.Header().Set("Retry-After", strconv.Itoa(bodyBudgetRetryAfter))
	w.Header().Set(NotActedHeader, NotActedAtCapacity)
	writeUnavailable(w, refusalAtCapacity)
}
