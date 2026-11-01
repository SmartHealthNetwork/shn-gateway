package engine

import (
	"errors"
	"maps"
	"net/http"
	"slices"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/contentcoding"
)

// answerHeaderNames are the headers of a participant's 2xx answer the network
// carries, as a direct call would: each describes the answer, and none is an
// address into its system.
var answerHeaderNames = []string{shnsdk.FrameHeaderETag, shnsdk.FrameHeaderLastModified, shnsdk.FrameHeaderRetryAfter}

// Why a header of an answer was not carried: nothing else leaves one out. A
// value is never judged as a validator or a date.
const (
	// AnswerHeaderValueNotCarriable is a value a frame cannot carry exactly: not
	// an HTTP field value, or not UTF-8 (shnsdk.FrameCarriesHeaderValue).
	AnswerHeaderValueNotCarriable = "value-not-carriable"
	// AnswerHeaderOverFrameLimit is a value the message frame's header segment
	// has no room for.
	AnswerHeaderOverFrameLimit = "over-frame-limit"
	// AnswerHeaderOverAnswerLimit is a header the answer was sealed without:
	// with it, the answer left less than answerHeaderMintMargin under the
	// sealed limit for its token and involved list. A header never costs an
	// answer, and an answer is authorized once.
	AnswerHeaderOverAnswerLimit = "over-answer-limit"
	// AnswerHeaderDecodedFromGzip is a strong ETag of an answer the transport
	// decoded from gzip: it named the coded bytes, not the ones carried.
	AnswerHeaderDecodedFromGzip = "decoded-from-gzip"
)

// AnswerHeaderNotCarriedValues is the closed list
// ExchangeRecord.AnswerHeadersNotCarried draws from.
var AnswerHeaderNotCarriedValues = func() []string {
	out := []string{shnsdk.FrameHeaderETag + ":" + AnswerHeaderDecodedFromGzip}
	for _, name := range answerHeaderNames {
		out = append(out, name+":"+AnswerHeaderValueNotCarriable, name+":"+AnswerHeaderOverFrameLimit, name+":"+AnswerHeaderOverAnswerLimit)
	}
	return out
}()

// answerHeadersOf reads the headers the network carries from a 2xx answer, and
// the not-carried record of a strong ETag whose bytes the transport decoded.
func answerHeadersOf(resp *http.Response) (map[string]string, []string) {
	var out map[string]string
	var notCarried []string
	for _, name := range answerHeaderNames {
		v := resp.Header.Get(name)
		if v == "" {
			continue
		}
		if name == shnsdk.FrameHeaderETag && resp.Uncompressed && contentcoding.StrongETag(v) {
			notCarried = append(notCarried, name+":"+AnswerHeaderDecodedFromGzip)
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[name] = v
	}
	return out, notCarried
}

// statedAnswer is what an answer states besides its body: the 2xx status, 0
// when it states none, and its headers.
type statedAnswer struct {
	status  int
	headers map[string]string
}

// builtAnswer is what an answer this gateway built states: 200 and no header.
var builtAnswer = statedAnswer{status: http.StatusOK}

// about returns the answer's headers that still describe p: a registered edit
// of the body drops the ones its row names.
func (a statedAnswer) about(p relay.Payload) map[string]string {
	out := maps.Clone(a.headers)
	for _, id := range p.Edits() {
		for _, name := range relay.EditDropsAnswerHeaders(relayEdits, id) {
			delete(out, name)
		}
	}
	return out
}

// writableAnswerHeaders splits headers into those that can be carried and the
// not-carried record of those that cannot, in name order. It also reads what a
// peer's frame states: a decoder keeps a value as it came.
func writableAnswerHeaders(headers map[string]string) (map[string]string, []string) {
	out := make(map[string]string, len(headers))
	var notCarried []string
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		if !shnsdk.FrameCarriesHeaderValue(headers[name]) {
			notCarried = append(notCarried, name+":"+AnswerHeaderValueNotCarriable)
			continue
		}
		out[name] = headers[name]
	}
	return out, notCarried
}

// encodeAnswerFrame frames an answer with its base headers and as many of
// extra as the frame has room for, leaving out the largest until it fits: a
// header that cannot be carried never costs the answer its frame, and with it
// its status. It reports what it left out; base alone over the limit is
// shnsdk.ErrFrameHeaderTooLarge.
func encodeAnswerFrame(status int, base, extra map[string]string, payload []byte) ([]byte, []string, error) {
	headers := maps.Clone(base)
	maps.Copy(headers, extra)
	remaining := slices.Sorted(maps.Keys(extra))
	var notCarried []string
	for {
		framed, err := shnsdk.EncodeHTTPFrameHeaders(status, headers, payload)
		if !errors.Is(err, shnsdk.ErrFrameHeaderTooLarge) || len(remaining) == 0 {
			return framed, notCarried, err
		}
		largest := slices.MaxFunc(remaining, func(a, b string) int { return len(extra[a]) - len(extra[b]) })
		delete(headers, largest)
		remaining = slices.DeleteFunc(remaining, func(name string) bool { return name == largest })
		notCarried = append(notCarried, largest+":"+AnswerHeaderOverFrameLimit)
	}
}

// writeRelayedAnswer writes a peer's answer to this gateway's own participant
// with what the answer states, for each header that still describes p and can
// be written. An unframed answer states a 200 and no header.
func (g *Gateway) writeRelayedAnswer(w http.ResponseWriter, answer statedAnswer, contentType string, p relay.Payload, k relay.Key) {
	headers, notCarried := writableAnswerHeaders(answer.about(p))
	if !g.writeAnswer(w, relayedAnswerStatus(answer.status), contentType, headers, p, k) {
		return
	}
	for _, value := range notCarried {
		exchangeOfWriter(w).answerHeaderNotCarried(value)
	}
}
