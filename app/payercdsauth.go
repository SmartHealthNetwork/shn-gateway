//go:build !shn_coreonly

package app

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// How the payer's gateway authenticates its calls to the payer's CDS service
// (CRD: the service listing and each hook), PAYER_DAVINCI_CDS_AUTH. CDS Hooks
// 2.0 ("Trusting CDS Clients"), which Da Vinci CRD adopts, defines one way: a
// JWT the client signs with its own key. That is the default whenever the
// payer client has a signing key. The access token the gateway sends the
// payer's other operations (DTR, PAS) is kept for CRD only when the payer
// sets it, or when there is no key to sign with.
const (
	cdsAuthClientJWT   = "client-jwt"
	cdsAuthAccessToken = "access-token"
	// cdsJKUOff is the PAYER_DAVINCI_CDS_JWT_JKU value that leaves jku out.
	cdsJKUOff = "off"
)

// payerCDSAuth is the CDS client authentication loadConfig settled.
type payerCDSAuth struct {
	// mode is cdsAuthClientJWT or cdsAuthAccessToken.
	mode string
	// why is the boot line's reason for mode.
	why string
	// iss is PAYER_DAVINCI_CDS_JWT_ISS ("" unset).
	iss string
	// jku is PAYER_DAVINCI_CDS_JWT_JKU: "" unset (the gateway's own JWK Set
	// URL, when it is https), cdsJKUOff, or an https URL.
	jku string
}

// payerHasSigningKey is whether the payer client has a key the gateway can
// sign with: PAYER_DAVINCI_CLIENT_KEY and PAYER_DAVINCI_CLIENT_ALG both set.
// A key stored as an empty value is no key.
func payerHasSigningKey(cfg config) bool {
	return strings.TrimSpace(cfg.PayerDavinciClientKey) != "" && strings.TrimSpace(cfg.PayerDavinciClientAlg) != ""
}

// checkPayerCDSAuth settles the payer's CDS client authentication from the
// PAYER_DAVINCI_CDS_* settings, refusing a value it cannot honour.
func checkPayerCDSAuth(role string, cfg config, rawMode, rawISS, rawJKU string) (payerCDSAuth, error) {
	set := rawMode != "" || rawISS != "" || rawJKU != ""
	if role != "payer" || cfg.PayerDavinciBaseURL == "" {
		if set {
			return payerCDSAuth{}, fmt.Errorf("gateway: PAYER_DAVINCI_CDS_AUTH, PAYER_DAVINCI_CDS_JWT_ISS and PAYER_DAVINCI_CDS_JWT_JKU need ROLE=payer and PAYER_DAVINCI_BASE_URL")
		}
		return payerCDSAuth{}, nil
	}
	a := payerCDSAuth{iss: rawISS, jku: rawJKU}
	hasKey := payerHasSigningKey(cfg)
	switch rawMode {
	case "":
		if hasKey {
			a.mode, a.why = cdsAuthClientJWT, "the payer client has a signing key (PAYER_DAVINCI_CLIENT_KEY)"
		} else {
			a.mode, a.why = cdsAuthAccessToken, "the payer client has no signing key (PAYER_DAVINCI_CLIENT_KEY and _ALG), so there is no CDS Hooks client JWT to sign; set them for one"
		}
	case cdsAuthClientJWT:
		if !hasKey {
			return payerCDSAuth{}, fmt.Errorf("gateway: PAYER_DAVINCI_CDS_AUTH=client-jwt needs the payer client's signing key: PAYER_DAVINCI_CLIENT_KEY and PAYER_DAVINCI_CLIENT_ALG")
		}
		a.mode, a.why = cdsAuthClientJWT, "PAYER_DAVINCI_CDS_AUTH=client-jwt"
	case cdsAuthAccessToken:
		a.mode, a.why = cdsAuthAccessToken, "PAYER_DAVINCI_CDS_AUTH=access-token"
	default:
		return payerCDSAuth{}, fmt.Errorf("gateway: PAYER_DAVINCI_CDS_AUTH must be client-jwt or access-token, got %q", rawMode)
	}
	if a.mode != cdsAuthClientJWT {
		if rawISS != "" || rawJKU != "" {
			return payerCDSAuth{}, fmt.Errorf("gateway: PAYER_DAVINCI_CDS_JWT_ISS and PAYER_DAVINCI_CDS_JWT_JKU apply to the client JWT; CDS calls here carry the access token (%s)", a.why)
		}
		return a, nil
	}
	// A key and algorithm set are a signing key, with or without a token
	// endpoint: one the gateway cannot sign with refuses the boot here, not
	// at its first CDS call.
	if !slices.Contains(smartAlgs, cfg.PayerDavinciClientAlg) {
		return payerCDSAuth{}, fmt.Errorf("gateway: PAYER_DAVINCI_CLIENT_ALG must be %s to sign the CDS client JWT with the payer client key, got %q (or set PAYER_DAVINCI_CDS_AUTH=access-token)", strings.Join(smartAlgs, "|"), cfg.PayerDavinciClientAlg)
	}
	if rawISS != "" {
		u, err := url.Parse(rawISS)
		if err != nil || !u.IsAbs() {
			return payerCDSAuth{}, fmt.Errorf("gateway: PAYER_DAVINCI_CDS_JWT_ISS must be an absolute URI (CDS Hooks: the URI of the issuer), got %q", rawISS)
		}
	}
	if rawJKU != "" && rawJKU != cdsJKUOff {
		u, err := url.Parse(rawJKU)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Fragment != "" || u.User != nil {
			return payerCDSAuth{}, fmt.Errorf("gateway: PAYER_DAVINCI_CDS_JWT_JKU must be off or an https URL, got %q", rawJKU)
		}
	}
	return a, nil
}

// payerCDSClient is the client the payer's gateway calls its payer's CDS
// service with in client-jwt mode, and the key whose public JWK Set it serves;
// both nil in access-token mode, where CDS calls go through the payer's other
// client, as DTR and PAS do. base is the transport beneath it (the payer's
// tagged transport). The issuer and the JWK Set URL are read for each request:
// the setting, else the payer ingress's base, else this gateway's own base as
// the network feed registers it (reg, under holderID).
func payerCDSClient(cfg config, a payerCDSAuth, base http.RoundTripper, reg shnsdk.Registry, holderID string, stdout io.Writer) (*http.Client, *smartauth.CDSClientKey, error) {
	if a.mode != cdsAuthClientJWT {
		switch {
		case !payerNative(cfg):
		case cfg.PayerDavinciTokenURL == "":
			fmt.Fprintf(stdout, "gateway: CDS calls to the payer carry no credential, as its other operations do (PAYER_DAVINCI_TOKEN_URL unset): %s\n", a.why)
		default:
			fmt.Fprintf(stdout, "gateway: CDS calls to the payer carry the payer client's access token: %s\n", a.why)
		}
		return nil, nil, nil
	}
	pk, err := loadSmartKey(cfg.PayerDavinciClientKey, cfg.PayerDavinciClientAlg)
	if err != nil {
		return nil, nil, fmt.Errorf("load payer client key for CDS client JWT: %w", err)
	}
	key, err := smartauth.NewCDSClientKey(cfg.PayerDavinciClientAlg, pk, cfg.PayerDavinciClientKID)
	if err != nil {
		return nil, nil, fmt.Errorf("payer CDS client key: %w", err)
	}
	ownBase := func() (string, string) {
		if cfg.PayerDavinciIngressBaseURL != "" {
			return cfg.PayerDavinciIngressBaseURL, "PAYER_DAVINCI_INGRESS_BASE_URL"
		}
		if holderID != "" {
			if e, ok := reg.Lookup(holderID); ok && e.BaseURL != "" {
				return e.BaseURL, "this gateway's registered base on the network feed"
			}
		}
		return "", ""
	}
	issuer := func() (string, bool) {
		if a.iss != "" {
			return a.iss, true
		}
		b, _ := ownBase()
		return b, b != ""
	}
	jku := func() string {
		switch a.jku {
		case cdsJKUOff:
			return ""
		case "":
			b, _ := ownBase()
			return httpsJWKSURL(b)
		default:
			return a.jku
		}
	}
	hc, err := smartauth.NewClientJWTHTTPClient(key, smartauth.ClientJWTConfig{Issuer: issuer, JKU: jku, Transport: base})
	if err != nil {
		return nil, nil, err
	}
	reportPayerCDSClient(stdout, a, key, ownBase, issuer, jku)
	return hc, key, nil
}

// payerCDSHTTPClient is payerCDSClient over the payer's tagged transport,
// with the diagnostic capture, when it is on, ABOVE the JWT layer: the capture
// records each request as the gateway made it, before its JWT is added, so it
// never holds one. Nil in access-token mode.
func payerCDSHTTPClient(cfg config, reg shnsdk.Registry, holderID string, stdout io.Writer) (*http.Client, *smartauth.CDSClientKey, error) {
	hc, key, err := payerCDSClient(cfg, cfg.PayerCDSAuth, payerTransport(cfg.certs), reg, holderID, stdout)
	if err != nil || hc == nil {
		return nil, nil, err
	}
	return cfg.diagnostic.client(hc), key, nil
}

// httpsJWKSURL is the JWK Set URL under base when base is https; "" otherwise,
// since a jku the service would fetch in cleartext is never sent.
func httpsJWKSURL(base string) string {
	if u, err := url.Parse(base); err != nil || !strings.EqualFold(u.Scheme, "https") {
		return ""
	}
	u, err := smartauth.CDSClientJWKSURL(base)
	if err != nil {
		return ""
	}
	return u
}

// reportPayerCDSClient is the boot line of the client JWT: the key, issuer and
// jku as known now. Never the JWT or the key itself.
func reportPayerCDSClient(stdout io.Writer, a payerCDSAuth, key *smartauth.CDSClientKey, ownBase func() (string, string), issuer func() (string, bool), jku func() string) {
	base, from := ownBase()
	fmt.Fprintf(stdout, "gateway: CDS calls to the payer carry a CDS Hooks client JWT (%s): alg %s, kid %s\n", a.why, key.Alg(), key.KID())
	switch iss, ok := issuer(); {
	case a.iss != "":
		fmt.Fprintf(stdout, "gateway: CDS client JWT iss %s (PAYER_DAVINCI_CDS_JWT_ISS)\n", iss)
	case ok:
		fmt.Fprintf(stdout, "gateway: CDS client JWT iss %s (%s)\n", iss, from)
	default:
		fmt.Fprintf(stdout, "gateway: WARNING CDS client JWT has no issuer yet: set PAYER_DAVINCI_CDS_JWT_ISS or PAYER_DAVINCI_INGRESS_BASE_URL, or it is read from this gateway's registration on the network feed; CDS calls are not sent until then\n")
	}
	switch {
	case a.jku == cdsJKUOff:
		fmt.Fprintf(stdout, "gateway: CDS client JWT jku left out (PAYER_DAVINCI_CDS_JWT_JKU=off)\n")
	case a.jku != "":
		fmt.Fprintf(stdout, "gateway: CDS client JWT jku %s (PAYER_DAVINCI_CDS_JWT_JKU)\n", a.jku)
	case jku() != "":
		fmt.Fprintf(stdout, "gateway: CDS client JWT jku %s, this gateway's own JWK Set\n", jku())
	case base != "":
		fmt.Fprintf(stdout, "gateway: CDS client JWT jku left out: this gateway's base %s is not https, and a jku is only ever https\n", base)
	default:
		fmt.Fprintf(stdout, "gateway: CDS client JWT jku left out until this gateway's base is known\n")
	}
	fmt.Fprintf(stdout, "gateway: CDS client public JWK Set served at %s\n", smartauth.CDSClientJWKSPath)
}
