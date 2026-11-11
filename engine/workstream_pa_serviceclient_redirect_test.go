//go:build !shn_coreonly

package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient/redirecttest"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// hubLeg drives one CRD through the provider's ingress with the Hub at
// hubURL over client (an embedder's plain client) and authorize through the
// in-process Framework, and returns the answer and the one record.
func hubLeg(t *testing.T, hubURL string, client *http.Client) (*httptest.ResponseRecorder, ExchangeRecord) {
	t.Helper()
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	env.payerReturns(LegResult{})
	env.originator.cfg.AuthzClient = &http.Client{Transport: env.substrate}
	env.originator.cfg.Client = client
	env.originator.cfg.HubURL = hubURL
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
	return rec, got.only(t)
}

// The Hub's client follows no redirect, the plain client an embedding
// program passes as Config.Client included: the leg fails exactly as when the
// Hub cannot be reached (its answer and outcome), the other origin and path
// receive nothing, the log line names the class and the status, and the
// Location's path and query appear in no answer, record or log line.
func TestHubRouteFollowsNoRedirect(t *testing.T) {
	ctrlRec, ctrl := hubLeg(t, "http://hub.test", refusedClient())
	if ctrl.Outcome != ExchangeUnreachable {
		t.Fatalf("control: an unreachable Hub recorded %q", ctrl.Outcome)
	}
	for _, c := range redirecttest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			f := redirecttest.Serve(t, c)
			logs := captureRedirectLog(t)
			rec, r := hubLeg(t, f.URL, f.Client)
			f.NotFollowed(t)
			if rec.Code != ctrlRec.Code || rec.Body.String() != ctrlRec.Body.String() {
				t.Errorf("answer %d %s, want the unreachable Hub's %d %s", rec.Code, rec.Body.String(), ctrlRec.Code, ctrlRec.Body.String())
			}
			if r.Outcome != ExchangeUnreachable || r.Status != ctrl.Status {
				t.Errorf("record outcome %q status %d, want the unreachable Hub's %q %d", r.Outcome, r.Status, ctrl.Outcome, ctrl.Status)
			}
			line := fmt.Sprintf(`to "payer" failed: %s (HTTP %d) (correlation `, redirectRefused, c.Code)
			if !strings.Contains(logs.String(), "gateway: hub leg ") || !strings.Contains(logs.String(), line) {
				t.Errorf("log %q, want a hub leg line with %q", logs.String(), line)
			}
			redirecttest.NoSecret(t, "the answer", rec.Body.String())
			redirecttest.NoSecret(t, "the record", fmt.Sprintf("%+v", r))
			redirecttest.NoSecret(t, "the log", logs.String())
		})
	}
}

// The authorize call's client follows no redirect, the plain client an
// embedding program passes as AuthzClient, or as Client with no AuthzClient,
// included: the leg is answered as when the Framework cannot be reached
// (503), nothing crosses to the Hub, and the record's authorizeError is
// redirect-refused, never unreachable.
func TestAuthorizeFollowsNoRedirect(t *testing.T) {
	for _, via := range []string{"AuthzClient", "Client"} {
		for _, c := range redirecttest.Cases() {
			t.Run(via+"/"+c.Name, func(t *testing.T) {
				f := redirecttest.Serve(t, c)
				logs := captureRedirectLog(t)
				env := newInProcessExchange(t)
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				env.payerReturns(LegResult{})
				env.originator.cfg.AuthzURL = f.URL
				if via == "AuthzClient" {
					env.originator.cfg.AuthzClient = f.Client
				} else {
					env.originator.cfg.AuthzClient = nil
					env.originator.cfg.Client = f.Client
				}
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteCRD)(rec, env.crdIngressRequest(t))
				r := got.only(t)
				f.NotFollowed(t)
				if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), errAuthzUnreachable.Error()) {
					t.Errorf("answer %d %s, want 503 naming the authorization service unreachable", rec.Code, rec.Body.String())
				}
				if n := env.routeHitCount(); n != 0 {
					t.Errorf("the request crossed to the Hub (%d) without a token", n)
				}
				if r.Outcome != ExchangeRefused || r.Rule != RefusalAuthority || r.AuthorizeError != redirectRefused {
					t.Errorf("record = outcome %q rule %q authorizeError %q, want refused on authority, redirect-refused", r.Outcome, r.Rule, r.AuthorizeError)
				}
				redirecttest.NoSecret(t, "the answer", rec.Body.String())
				redirecttest.NoSecret(t, "the record", fmt.Sprintf("%+v", r))
				redirecttest.NoSecret(t, "the log", logs.String())
			})
		}
	}
}

// A validator an embedding program passes New, as the canonical one or in a
// lane map, follows no redirect: New puts its plain client under the policy.
func TestNewPutsEmbedderValidatorsUnderThePolicy(t *testing.T) {
	for _, field := range []string{"Validator", "ValidatorsByContractLine", "ValidatorsByLine", "CertificationValidatorsByLine"} {
		for _, c := range redirecttest.Cases() {
			t.Run(field+"/"+c.Name, func(t *testing.T) {
				f := redirecttest.Serve(t, c)
				v := &shnsdk.OperationValidator{BaseURL: f.URL, Client: f.Client}
				cfg := newInProcessExchange(t).originator.cfg
				switch field {
				case "Validator":
					cfg.Validator = v
				case "ValidatorsByContractLine":
					cfg.ValidatorsByContractLine = map[string]shnsdk.Validator{"pa.pas@2.1": v}
				case "ValidatorsByLine":
					cfg.ValidatorsByContractLine, cfg.ValidatorsByLine = nil, map[string]shnsdk.Validator{"2.1": v}
				default:
					cfg.CertificationValidatorsByLine = map[string]shnsdk.Validator{"2.1": v}
				}
				g := mustNew(t, cfg)
				_ = g
				_, err := v.Validate(context.Background(), []byte(`{"resourceType":"Bundle"}`), "")
				f.NotFollowed(t)
				if !serviceclient.IsRedirectRefused(err) {
					t.Fatalf("err = %v, want a refused redirect", err)
				}
			})
		}
	}
}
