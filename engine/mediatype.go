package engine

import (
	"context"
	"mime"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Media type on the native request. A payer gateway sends its own
// system each leg's own media type — application/json for CDS Hooks,
// application/fhir+json for the FHIR operations — and, on a FHIR leg whose
// sender framed the participant's declared media type, that declared type
// (with its parameters, e.g. fhirVersion), so the payer sees the Content-Type
// and Accept a direct client would have sent.
//
// Only a FHIR JSON media type is carried: application/fhir+json or
// application/json, parsed and re-formatted, so nothing but a well-formed
// media type can reach the payer's request line. A CDS Hooks leg always uses
// application/json: older senders stamp application/fhir+json on every frame
// whatever the client sent, and a CDS service refuses that.

const (
	mediaFHIRJSON = "application/fhir+json"
	mediaJSON     = "application/json"
)

// carriedFHIRMediaType returns ct, exactly as the participant sent it, if it
// is a well-formed FHIR JSON media type this gateway may carry, else "".
func carriedFHIRMediaType(ct string) string {
	ct, mt := writableMediaType(ct)
	if mt != mediaFHIRJSON && mt != mediaJSON {
		return ""
	}
	return ct
}

// writableMediaType returns ct, exactly as the participant stated it (trimmed),
// and its media type, when it is a well-formed media type an HTTP header can
// carry: it parses, it is at most 256 bytes, and it holds no control
// character. Otherwise both are "".
func writableMediaType(ct string) (string, string) {
	ct = strings.TrimSpace(ct)
	if ct == "" || len(ct) > 256 || strings.IndexFunc(ct, func(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }) >= 0 {
		return "", ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", ""
	}
	return ct, mt
}

// writableAnswerMediaType is the Content-Type a peer's relayed success answer
// is written to this gateway's own participant with: the media type the
// answering system stated, exactly as its frame carried it, whatever the type
// (a direct client would see it), when it is one writableMediaType accepts.
// An answer that states none, or one no header can carry, is written
// application/fhir+json: an answer a gateway built itself, or an earlier
// payer gateway's frame, states that type, and the protocol reads a type
// that is not stated as it.
func writableAnswerMediaType(stated string) string {
	if ct, _ := writableMediaType(stated); ct != "" {
		return ct
	}
	return mediaFHIRJSON
}

// acceptFor is the Accept a direct client of the same media type would send:
// the type and its fhirVersion, without a charset.
func acceptFor(ct string) string {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return ct
	}
	if v, ok := params["fhirversion"]; ok {
		// Quoted when the value needs it; the parameter keeps FHIR's casing.
		return strings.Replace(mime.FormatMediaType(mt, map[string]string{"fhirversion": v}), "fhirversion=", "fhirVersion=", 1)
	}
	return mt
}

type requestMediaTypeKey struct{}

// withRequestMediaType tags ctx with the media type the sender framed for this
// request ("" is a no-op).
func withRequestMediaType(ctx context.Context, ct string) context.Context {
	if ct == "" {
		return ctx
	}
	return context.WithValue(ctx, requestMediaTypeKey{}, ct)
}

func requestMediaType(ctx context.Context) string {
	ct, _ := ctx.Value(requestMediaTypeKey{}).(string)
	return ct
}

// inboundFrameMediaType is the FHIR JSON media type a framed request declares,
// or "" (bare, undeclared, or not a carried type).
func inboundFrameMediaType(payload []byte) string {
	if !shnsdk.IsFramed(payload) {
		return ""
	}
	hdr, _, err := shnsdk.DecodeHTTPFrame(payload)
	if err != nil {
		return ""
	}
	return carriedFHIRMediaType(hdr.Headers["Content-Type"])
}

// nativeRequestMediaType is the Content-Type a native request carrying p is
// sent with: p's own media type, or — for a FHIR payload — the sender's
// declared FHIR JSON media type when one was framed.
func nativeRequestMediaType(ctx context.Context, p interface{ ContentType() string }) string {
	ct := p.ContentType()
	if ct == "" {
		return mediaJSON
	}
	if mt, _, err := mime.ParseMediaType(ct); err == nil && mt == mediaFHIRJSON {
		if declared := requestMediaType(ctx); declared != "" {
			return declared
		}
	}
	return ct
}

// answerMediaTypeKey carries a sink for the media type a recipient's framed
// 2xx answer states, for a caller that relays that answer to its own
// participant.
type answerMediaTypeKey struct{}

// withAnswerMediaTypeSink returns ctx carrying sink: the origination that
// decodes a framed 2xx answer stores the answer's stated media type in it
// ("" when the frame states none).
func withAnswerMediaTypeSink(ctx context.Context, sink *string) context.Context {
	return context.WithValue(ctx, answerMediaTypeKey{}, sink)
}

// noteAnswerMediaType stores ct in ctx's answer media type sink, if any.
func noteAnswerMediaType(ctx context.Context, ct string) {
	if sink, ok := ctx.Value(answerMediaTypeKey{}).(*string); ok && sink != nil {
		*sink = ct
	}
}

// statedAnswerKey carries a sink for what a recipient's framed answer states,
// for a caller that relays that answer to its own participant.
type statedAnswerKey struct{}

// withStatedAnswerSink returns ctx carrying sink: the origination that decodes
// a framed 2xx answer stores what the answer states in it. A bare answer
// leaves it unset.
func withStatedAnswerSink(ctx context.Context, sink *statedAnswer) context.Context {
	return context.WithValue(ctx, statedAnswerKey{}, sink)
}

// noteStatedAnswer stores what a framed answer states in ctx's sink, if any.
func noteStatedAnswer(ctx context.Context, hdr shnsdk.HTTPFrameHeader) {
	sink, ok := ctx.Value(statedAnswerKey{}).(*statedAnswer)
	if !ok || sink == nil {
		return
	}
	sink.status, sink.headers, sink.address = hdr.Status, nil, nil
	for _, name := range answerHeaderNames {
		if v := hdr.Headers[name]; v != "" {
			if sink.headers == nil {
				sink.headers = map[string]string{}
			}
			sink.headers[name] = v
		}
	}
	// The address headers a recipient's gateway states for the requester's
	// gateway alone: a payer's on a DTR answer (the re-key, E-12, reads them)
	// and a provider's on its answer to a payer's Task (the Location, E-16,
	// reads it); never written to a participant.
	for _, name := range addressHeaderNames {
		if v := hdr.Headers[name]; v != "" {
			if sink.address == nil {
				sink.address = map[string]string{}
			}
			sink.address[name] = v
		}
	}
}

// relayedAnswerStatus is the status a relayed answer is written to the
// participant's system with: stated, the 2xx a framed answer states (a peer
// gateway before v0.63.0 framed every success as 200), or 200 for a bare
// answer, which states none.
func relayedAnswerStatus(stated int) int {
	if stated == 0 {
		return http.StatusOK
	}
	return stated
}
