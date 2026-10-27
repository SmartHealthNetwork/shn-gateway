//go:build !shn_coreonly

package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Every outbound client build() makes carries the endpoint kind of the
// setting its URL comes from (MON-R25): its transport is the one it used
// before, Wrapped by endpointcert to record each TLS response's certificate,
// so the certificate-expiry gauge can say which certificate to renew. No
// connection, pool or transport setting changes. A setting left to fall
// back on another's URL keeps its own kind.
//
// One thing does change with the wrapper: net/http enforces a client's
// Timeout on a transport that is not its own *http.Transport by cancelling
// the request (its older path), racing the request's context deadline.
// The deadline and its error classes are the same; only a timeout error's
// text can differ ("net/http: request canceled" where it read "context
// deadline exceeded"). Clients behind the bearer layer already took that
// path.

// settingTags is the endpoint kind of each row of optionalURLs, by setting.
// Every row has one (TestMONR25EveryOptionalURLHasAnEndpointKind).
var settingTags = map[string]endpointcert.Tag{
	"AUTHZ_URL":                         endpointcert.Of(endpointcert.Authorization),
	"HUB_URL":                           endpointcert.Of(endpointcert.Hub),
	"CONSENT_URL":                       endpointcert.Of(endpointcert.Consent),
	"FHIR_VALIDATE_URL":                 validatorLane(canonicalLine()),
	"FHIR_VALIDATE_URL_2_1":             validatorLane("2.1"),
	"FHIR_VALIDATE_URL_2_2":             validatorLane("2.2"),
	"FHIR_CERTIFY_URL_2_1":              certifyLane("2.1"),
	"FHIR_CERTIFY_URL_2_2":              certifyLane("2.2"),
	"FHIR_DATA_URL":                     endpointcert.Of(endpointcert.FHIR),
	"REGISTRAR_URL":                     endpointcert.Of(endpointcert.Registrar),
	"FHIR_TOKEN_URL":                    endpointcert.Of(endpointcert.FHIRToken),
	"PAYER_DAVINCI_BASE_URL":            endpointcert.Of(endpointcert.PayerDavinci),
	"PAYER_DAVINCI_DTR_BASE_URL":        endpointcert.Of(endpointcert.PayerDTR),
	"PAYER_DAVINCI_PAS_BASE_URL":        endpointcert.Of(endpointcert.PayerPAS),
	"PAYER_DAVINCI_TOKEN_URL":           endpointcert.Of(endpointcert.PayerToken),
	"PROVIDER_DTR_POPULATE_URL":         endpointcert.Of(endpointcert.Populate),
	"PROVIDER_DTR_POPULATE_TOKEN_URL":   endpointcert.Of(endpointcert.PopulateToken),
	"PROVIDER_DAVINCI_INGRESS_BASE_URL": endpointcert.Of(endpointcert.Ingress),
	"SHN_DISCOVERY_URL":                 endpointcert.Of(endpointcert.Discovery),
	"AUTHZ_PUBKEY_URL":                  endpointcert.Of(endpointcert.AuthorizationKey),
	"HUB_TRANSPORT_KEY_URL":             endpointcert.Of(endpointcert.HubKey),
}

// canonicalLine is the IG line FHIR_VALIDATE_URL validates.
func canonicalLine() string { return shnsdk.LineOf(engine.CanonicalContractVersion()) }

// validatorLane and certifyLane are the tags of a line's validator and
// certification endpoints.
func validatorLane(line string) endpointcert.Tag {
	return endpointcert.Lane(endpointcert.Validator, line)
}
func certifyLane(line string) endpointcert.Tag { return endpointcert.Lane(endpointcert.Certify, line) }

// checkTag is the endpoint kind a /internal/checks target's probes reach:
// its setting's, or for a target that is not a setting, the endpoint it
// stands for. ok is false for an ID this table does not know.
func checkTag(id string) (endpointcert.Tag, bool) {
	if t, ok := settingTags[id]; ok {
		return t, true
	}
	switch {
	case id == "PAYER_DAVINCI_WELL_KNOWN":
		return endpointcert.Of(endpointcert.PayerDavinci), true
	case id == "AUDIT_ENDPOINT":
		return endpointcert.Of(endpointcert.Audit), true
	case strings.HasPrefix(id, "validator-lane-"):
		return validatorLane(strings.TrimPrefix(id, "validator-lane-")), true
	}
	return endpointcert.Tag{}, false
}

// tagCheckTargets gives each target a copy of the runner's client with the
// target's endpoint kind on its transport.
func tagCheckTargets(rec *endpointcert.Recorder, targets []checks.Target, client *http.Client) []checks.Target {
	for i := range targets {
		if tag, ok := checkTag(targets[i].ID); ok {
			targets[i].Client = rec.Client(tag, client)
		}
	}
	return targets
}

// tagSmart puts ops on sc's operations (beneath the bearer) and the tagged
// token client on its token acquisition. A nil rec leaves sc as it was.
func tagSmart(rec *endpointcert.Recorder, sc *smartauth.Config, ops http.RoundTripper, token endpointcert.Kind) {
	if rec == nil {
		return
	}
	sc.Transport = ops
	// The bearer layer fetches its token under the operation's context, which
	// carries the operation's mark (payer-pas, cds, ...): the token client
	// records under its own kind whatever the request arrived marked with.
	sc.HTTPClient = &http.Client{Timeout: 10 * time.Second, Transport: rec.WrapOwn(endpointcert.Of(token), nil)}
}

// payerTransport is the payer's operations transport: http.DefaultTransport,
// as before, recording under payer-davinci by default and under each
// per-operation setting's kind for the requests the engine marks with it
// (endpointcert.WithTag at the CDS, DTR, PAS and eligibility call sites).
// One transport serves one bearer layer and one connection pool, as before.
// Every exchange the payer forwards is marked, so payer-davinci itself is
// read from the requests made to PAYER_DAVINCI_BASE_URL as such: the
// /internal/checks metadata and well-known probes.
func payerTransport(rec *endpointcert.Recorder) http.RoundTripper {
	return rec.WrapRouted(endpointcert.Of(endpointcert.PayerDavinci), nil,
		endpointcert.Of(endpointcert.PayerDTR), endpointcert.Of(endpointcert.PayerPAS),
		endpointcert.Of(endpointcert.PayerEligibility), endpointcert.Of(endpointcert.CDS))
}

// taggedValidator is shnsdk.NewOperationValidator(base) as line's validator
// lane.
func taggedValidator(rec *endpointcert.Recorder, line, base string) *shnsdk.OperationValidator {
	v := shnsdk.NewOperationValidator(base)
	engine.TagValidatorClient(v, rec, validatorLane(line))
	return v
}

// certifyClient is certificationClient(endpoint) as line's certify lane.
func certifyClient(rec *endpointcert.Recorder, line, endpoint string) *shnsdk.OperationValidator {
	v := certificationClient(endpoint)
	engine.TagValidatorClient(v, rec, certifyLane(line))
	return v
}

// laneQualifier is qualifyDefaultLane through a client tagged for the lane
// the attempt is for: the engine marks a routing attempt's context with the
// validator kind for its line and a certification attempt's with certify.
// An attempt that names neither is recorded under none.
// build() makes one and hands the same value to routing and certification,
// whose hand-off (a routing pass counting for certification) compares the
// qualifiers' identity.
func laneQualifier(rec *endpointcert.Recorder) engine.LaneQualifier {
	if rec == nil {
		return qualifyDefaultLane
	}
	return func(ctx context.Context, base, line string) error {
		tag, ok := endpointcert.TagOf(ctx)
		if !ok || !tag.Kind().HasLanes() || tag.IGLine() != line {
			tag = endpointcert.Tag{} // records nothing: never folded into a lane
		}
		client := rec.Client(tag, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
		return engine.QualifyValidatorLaneWith(ctx, client, base, line)
	}
}

// tagStoreTLS puts the store kind's hook on the store database's TLS
// configs, the DSN's own and each fallback's (sslmode=prefer and allow carry
// one with and one without TLS): pgx verifies as the DSN asks, and the hook
// then records the server certificate the database presented. A plaintext
// DSN has no TLS config and records nothing.
func tagStoreTLS(rec *endpointcert.Recorder, cc *pgconn.Config) {
	if rec == nil || cc == nil {
		return
	}
	tag := endpointcert.Of(endpointcert.Store)
	if cc.TLSConfig != nil {
		cc.TLSConfig.VerifyConnection = rec.VerifyConnection(tag, cc.TLSConfig.VerifyConnection)
	}
	for _, fb := range cc.Fallbacks {
		if fb.TLSConfig != nil && fb.TLSConfig != cc.TLSConfig {
			fb.TLSConfig.VerifyConnection = rec.VerifyConnection(tag, fb.TLSConfig.VerifyConnection)
		}
	}
}
