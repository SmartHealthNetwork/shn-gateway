//go:build !shn_coreonly

package engine

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// workstream_pa_cdex_size.go — the size of each file a documentation
// attachment submission carries inline. A payer declares the largest single
// inline attachment its system accepts (its registry entry's
// attachmentMaxBytes); the sending gateway measures each file against it
// before sending. Only inline files are measured: an attachment given by url,
// and content with no inline file (a QuestionnaireResponse with no attachment
// answer), carry nothing to measure and are carried. The whole body is
// bounded by the attachment ceiling separately.

// errAttachmentBudget is a requester's answer when its gateway has no room for
// a documentation attachment now.
const errAttachmentBudget = "attachment budget exhausted"

// holdAttachment takes a submission's bytes of the budget for large bodies, or
// refuses it: 503 with Retry-After, recorded as this gateway's routing refusal,
// transient, nothing read and nothing sent.
func (g *Gateway) holdAttachment(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	n := g.attachmentCeiling()
	if r.ContentLength >= 0 {
		n = min(r.ContentLength, n)
	}
	release, ok = g.bodies.acquire(n)
	if !ok {
		exchangeOf(r.Context()).refusing(RefusalRouting)
		w.Header().Set("Retry-After", strconv.Itoa(bodyBudgetRetryAfter))
		writeUnavailable(w, errAttachmentBudget)
	}
	return release, ok
}

// attachmentReadTimeout is how long a submission holding the budget has to
// arrive, so a sender that stalls cannot keep it.
var attachmentReadTimeout = 30 * time.Second

// limitAttachmentRead gives the read of a held submission its deadline, and
// returns what ends it. A connection that cannot take a deadline has none: the
// hold then ends with the read.
func limitAttachmentRead(w http.ResponseWriter) func() {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(attachmentReadTimeout))
	return func() { _ = rc.SetReadDeadline(time.Time{}) }
}

// attachmentCeiling is the largest documentation attachment submission body
// this gateway sends or accepts (Config.AttachmentCeiling), never above the
// network's ceiling.
func (g *Gateway) attachmentCeiling() int64 {
	if c := g.cfg.AttachmentCeiling; c > 0 && c < shnsdk.MaxAttachmentBytes {
		return c
	}
	return shnsdk.MaxAttachmentBytes
}

// errAttachmentTooLarge is the published reason a submission carrying an
// inline file over the largest its payer accepts is refused for.
const errAttachmentTooLarge = "attachment too large"

// attachmentOverDeclared is the refusal of a submission to recipient that
// carries an inline file over the smaller of the largest file the recipient
// declares its system accepts (its registry entry's AttachmentMaxBytes) and
// this gateway's ceiling: 413 and the reason, naming both sizes, or 0. A
// recipient that declares none has only the ceiling, which bounds the whole
// body already; and a submission with no inline file it can measure is
// carried.
func (g *Gateway) attachmentOverDeclared(recipient string, submission []byte) (int, string) {
	entry, ok := g.cfg.Reg.Lookup(recipient)
	if !ok || entry.AttachmentMaxBytes == nil || *entry.AttachmentMaxBytes < 1 {
		// None declared, or a value no declaration can carry: unknown.
		return 0, ""
	}
	limit := min(*entry.AttachmentMaxBytes, g.attachmentCeiling())
	if int64(len(submission)) <= limit {
		// No file can be larger than the body that carries it.
		return 0, ""
	}
	largest, measured, err := largestInlineAttachment(submission)
	if err != nil || !measured || largest <= limit {
		return 0, ""
	}
	return http.StatusRequestEntityTooLarge, errAttachmentTooLarge + ": an inline attachment is " + strconv.FormatInt(largest, 10) +
		" bytes, over the " + strconv.FormatInt(limit, 10) + " the payer's system accepts"
}

// largestInlineAttachment returns the size, in bytes after base64 decoding,
// of the largest inline file in submission, a CDex $submit-attachment
// Parameters, and whether it holds any it could measure. The files are found
// by structure only, at any depth: each object-valued valueAttachment (of a
// parameter or part, or a QuestionnaireResponse's answer), and each
// content[].attachment of a DocumentReference, wherever it is carried. A file
// is measured from its base64 length and padding, never
// decoded; one whose data is not well-formed base64 has no knowable length
// and is not counted (judging a payload's form is not the network's).
func largestInlineAttachment(submission []byte) (int64, bool, error) {
	doc, err := relay.Doc(relay.NewBody(submission, relay.OriginIngressRequest))
	if err != nil {
		return 0, false, err
	}
	var largest int64
	measured := false
	measure := func(attachment relay.NodeID) {
		data, ok := doc.Member(attachment, "data")
		if !ok || doc.Kind(data) != relay.KindString {
			return
		}
		raw := submission
		start, end := doc.Span(data)
		text := raw[start+1 : end-1] // the string's bytes between its quotes
		for _, c := range text {
			if c == '\\' {
				// An escape (a base64 "/" may be written "\/"): the decoded
				// string is measured instead.
				s, err := doc.StringValue(data)
				if err != nil {
					return
				}
				text = []byte(s)
				break
			}
		}
		if n, ok := base64DecodedLen(text); ok {
			largest, measured = max(largest, n), true
		}
	}
	// Every value of the submission is visited, at any depth: a parameter or
	// part, a resource it carries, a contained resource, a Bundle entry, a
	// QuestionnaireResponse's answers. Each object-valued valueAttachment is a
	// file, and so is each attachment of a DocumentReference's content.
	var walk func(n relay.NodeID)
	walk = func(n relay.NodeID) {
		switch doc.Kind(n) {
		case relay.KindArray:
			for _, e := range doc.Elems(n) {
				walk(e)
			}
		case relay.KindObject:
			if v, ok := doc.Member(n, "valueAttachment"); ok && doc.Kind(v) == relay.KindObject {
				measure(v)
			}
			if isDocumentReference(doc, n) {
				if content, ok := doc.Member(n, "content"); ok {
					for _, c := range doc.Elems(content) {
						if a, ok := doc.Member(c, "attachment"); ok && doc.Kind(a) == relay.KindObject {
							measure(a)
						}
					}
				}
			}
			for _, m := range doc.Members(n) {
				walk(m.Value)
			}
		}
	}
	walk(doc.Root())
	return largest, measured, nil
}

// isDocumentReference reports whether the object n is a DocumentReference.
func isDocumentReference(doc *relay.Document, n relay.NodeID) bool {
	rt, ok := doc.Member(n, "resourceType")
	if !ok {
		return false
	}
	name, err := doc.StringValue(rt)
	return err == nil && name == "DocumentReference"
}

// base64Alphabet is the standard base64 alphabet, which base64Binary uses.
const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// base64DecodedLen is the number of bytes the base64 text decodes to, read
// from its length and padding alone, and whether it is well-formed base64
// (the standard alphabet, padded to a multiple of four, with at most two
// "=" and only at the end; whitespace, which FHIR's base64Binary allows, is
// skipped).
func base64DecodedLen(text []byte) (int64, bool) {
	var n, pad int64
	for _, c := range text {
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		case c == '=':
			pad++
		case pad > 0:
			return 0, false // a character after padding
		case strings.IndexByte(base64Alphabet, c) >= 0:
			n++
		default:
			return 0, false
		}
	}
	if pad > 2 || (n+pad)%4 != 0 {
		return 0, false
	}
	return (n+pad)/4*3 - pad, true
}
