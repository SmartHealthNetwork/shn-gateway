package engine

import (
	"context"
	"mime"
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
	ct = strings.TrimSpace(ct)
	if ct == "" || len(ct) > 256 || strings.IndexFunc(ct, func(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }) >= 0 {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || (mt != mediaFHIRJSON && mt != mediaJSON) {
		return ""
	}
	return ct
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
