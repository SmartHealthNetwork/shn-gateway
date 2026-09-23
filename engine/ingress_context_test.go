package engine

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/exchangecontext"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

func ingressContextFixture(t *testing.T) (*Gateway, *ecdsa.PrivateKey, exchangecontext.Claims, *http.Request, []byte) {
	t.Helper()
	key, pub := newTestClientKey(t)
	auth := newTestAuthServer(t, "context-source", pub, "ES384")
	reg := auth.clients["context-source"]
	reg.ContextOperations = []string{"pas-submit", "pas-update-submit", "pas-inquire", "crd-order-select", "crd-order-dispatch", "questionnaire-package", "next-question"}
	reg.BoundaryPreparations = []string{"E-01"}
	auth.clients["context-source"] = reg
	registry := shnsdk.NewRegistry()
	registry.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer"})
	g := &Gateway{cfg: Config{HolderID: "provider", Reg: registry, Clock: auth.now}, ingressAuth: auth}
	assertIngressContextNoSideEffects(t, g)
	now := auth.now()
	c := exchangecontext.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "context-source", Subject: "context-source", Audience: jwt.ClaimStrings{testIngressBaseURL + "/Claim/$submit"}, ID: "context-once", IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Holder: "provider", Recipient: "payer", Leg: "pas-claim", Operation: "pas-submit", SubjectPCI: "already-issued-pci", CorrelationID: "corr", ContentType: "application/fhir+json"}
	r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", nil)
	r.Header.Set("Content-Type", c.ContentType)
	return g, key, c, r, []byte("{unreadable")
}
func putContext(t *testing.T, r *http.Request, c exchangecontext.Claims, body []byte, key *ecdsa.PrivateKey) {
	t.Helper()
	token, err := exchangecontext.Sign(c, body, "ES384", key)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(exchangecontext.Header, token)
}
func contextFailure(t *testing.T, err error, status int, code string) {
	t.Helper()
	var e *ingressContextError
	if !errors.As(err, &e) || e.status != status || e.code != code {
		t.Fatalf("error=%v; want %d %s", err, status, code)
	}
}
func TestIngressContextOpaqueExactBytes(t *testing.T) {
	g, key, c, r, body := ingressContextFixture(t)
	putContext(t, r, c, body, key)
	ex, err := g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body)
	if err != nil {
		t.Fatal(err)
	}
	if ex.subjectPCI != c.SubjectPCI || ex.clientID != c.Issuer || ex.operation != "pas-submit" || ex.contentType != c.ContentType || ex.correlationID != c.CorrelationID || ex.policy.level != EnforcementNone {
		t.Fatalf("lost context: %+v", ex)
	}
	_, err = g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body)
	contextFailure(t, err, 403, "context_invalid")
}
func TestIngressContextRejectionsBeforeSideEffects(t *testing.T) {
	rows := []struct {
		name   string
		change func(*Gateway, *exchangecontext.Claims, *http.Request)
		status int
		code   string
	}{
		{"wrong client", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) { c.Issuer = "other"; c.Subject = "other" }, 403, "context_invalid"},
		{"unknown connector", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) { delete(g.ingressAuth.clients, c.Issuer) }, 403, "context_invalid"},
		{"operation not granted", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) {
			reg := g.ingressAuth.clients[c.Issuer]
			reg.ContextOperations = nil
			g.ingressAuth.clients[c.Issuer] = reg
		}, 403, "context_invalid"},
		{"audience", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) {
			c.Audience = jwt.ClaimStrings{"https://attacker/Claim/$submit"}
			r.Host = "attacker"
		}, 403, "context_invalid"},
		{"expired", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) {
			c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Time)
		}, 403, "context_invalid"},
		{"future", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) {
			c.IssuedAt = jwt.NewNumericDate(c.IssuedAt.Add(time.Second))
		}, 403, "context_invalid"},
		{"overlong validity", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) {
			c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Add(5*time.Minute + time.Second))
		}, 403, "context_invalid"},
		{"next question wrong route", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) {
			c.Leg = "dtr-questionnaire-fetch"
			c.Operation = shnsdk.FrameOperationNextQuestion
			r.URL.Path = "/Questionnaire/$questionnaire-package"
			c.Audience = jwt.ClaimStrings{testIngressBaseURL + r.URL.Path}
		}, 403, "context_invalid"},
		{"holder", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) { c.Holder = "other" }, 403, "context_invalid"},
		{"recipient", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) { c.Recipient = "unknown" }, 403, "context_invalid"},
		{"content type", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) { c.ContentType = "application/json" }, 403, "context_invalid"},
		{"operation route", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) {
			c.Leg = "pas-claim-inquire"
			c.Operation = "pas-inquire"
		}, 403, "context_invalid"},
		{"oversized jti", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) {
			c.ID = strings.Repeat("j", MaxReplayKeyBytes+1)
		}, 403, "context_invalid"},
		{"hook on pas", func(g *Gateway, c *exchangecontext.Claims, r *http.Request) { c.CRDHook = "order-sign" }, 403, "context_invalid"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			g, key, c, r, body := ingressContextFixture(t)
			replay := &stubReplayStore{}
			g.ingressAuth.replay = replay
			row.change(g, &c, r)
			putContext(t, r, c, body, key)
			_, err := g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: "context-source"}, body)
			contextFailure(t, err, row.status, row.code)
			if replay.calls != 0 {
				t.Fatalf("invalid context reserved replay state: calls=%d", replay.calls)
			}
		})
	}
}
func TestIngressContextAbsenceAndInvalidAreDistinct(t *testing.T) {
	g, key, c, r, body := ingressContextFixture(t)
	_, err := g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body)
	contextFailure(t, err, 400, "context_missing")
	if !errors.Is(err, errIngressContextAbsent) {
		t.Fatal("absent token must permit legacy adapter selection")
	}
	r.Header[http.CanonicalHeaderKey(exchangecontext.Header)] = []string{""}
	_, err = g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body)
	contextFailure(t, err, 401, "context_invalid")
	if errors.Is(err, errIngressContextAbsent) {
		t.Fatal("present invalid context can fall back")
	}
	putContext(t, r, c, body, key)
	_, err = g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, []byte("changed"))
	contextFailure(t, err, 403, "context_invalid")
	r.Header.Set(exchangecontext.Header, "bad")
	_, err = g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body)
	contextFailure(t, err, 401, "context_invalid")
}
func TestIngressContextCRDHookAndDTRRouting(t *testing.T) {
	for _, row := range []struct {
		path, leg, op, hook string
		status              int
	}{
		{"/cds-services/shn-order-select", "crd-order-select", "crd-order-select", "order-select", 0},
		{"/cds-services/shn-order-sign", "crd-order-select", "crd-order-select", "order-sign", 0},
		{"/cds-services/shn-order-dispatch", "crd-order-dispatch", "crd-order-dispatch", "order-dispatch", 0},
		{"/cds-services/shn-order-select", "crd-order-select", "crd-order-select", "", 400},
		{"/cds-services/shn-order-select", "crd-order-select", "crd-order-select", "order-dispatch", 403},
		{"/cds-services/shn-order-select", "crd-order-select", "crd-order-select", "order-sign", 403},
		{"/Questionnaire/$questionnaire-package", "dtr-questionnaire-fetch", "questionnaire-package", "", 0},
		{"/Claim/$submit", "pas-claim-update", "pas-update-submit", "", 0},
		{"/Claim/$inquire", "pas-claim-inquire", "pas-inquire", "", 0},
	} {
		t.Run(row.path+row.hook, func(t *testing.T) {
			g, key, c, r, body := ingressContextFixture(t)
			r.URL.Path = row.path
			c.Audience = jwt.ClaimStrings{testIngressBaseURL + row.path}
			c.Leg = row.leg
			c.Operation = row.op
			c.CRDHook = row.hook
			if row.hook != "" {
				c.Completed = []exchangecontext.Completion{{ID: "E-01", Version: "1"}}
			}
			putContext(t, r, c, body, key)
			ex, err := g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body)
			if row.status == 0 {
				if err != nil || ex.crdHook != row.hook {
					t.Fatalf("ex=%+v err=%v", ex, err)
				}
			} else {
				code := "context_invalid"
				if row.status == 400 {
					code = "context_missing"
				}
				contextFailure(t, err, row.status, code)
				if errors.Is(err, errIngressContextAbsent) {
					t.Fatal("missing signed hook must not fall back")
				}
			}
		})
	}
}
func TestIngressContextReplayAcrossReplicasAndOutage(t *testing.T) {
	g, key, c, r, body := ingressContextFixture(t)
	putContext(t, r, c, body, key)
	// Each replica has its own auth configuration, parsed signing registration,
	// and token keys. Only the replay backing is shared across the pair.
	reg := g.ingressAuth.clients[c.Issuer]
	reg.PublicKeyPEM = bytes.Clone(reg.PublicKeyPEM)
	reg.Scopes = append([]string(nil), reg.Scopes...)
	reg.ContextOperations = append([]string(nil), reg.ContextOperations...)
	reg.BoundaryPreparations = append([]string(nil), reg.BoundaryPreparations...)
	otherKeys, err := newEphemeralKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	otherAuth, err := newIngressAuthServer(g.ingressAuth.baseURL,
		map[string]IngressClientRegistration{c.Issuer: reg}, g.ingressAuth.now,
		otherKeys, g.ingressAuth.replay)
	if err != nil {
		t.Fatal(err)
	}
	otherRegistry := shnsdk.NewRegistry()
	otherRegistry.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer"})
	other := &Gateway{cfg: Config{HolderID: g.cfg.HolderID, Reg: otherRegistry, Clock: otherAuth.now}, ingressAuth: otherAuth}
	if other.ingressAuth == g.ingressAuth {
		t.Fatal("replicas must have independent auth servers")
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			replica := g
			if i%2 == 0 {
				replica = other
			}
			if _, err := replica.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body); err == nil {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted=%d", accepted.Load())
	}
	outage := &stubReplayStore{err: errors.New("unavailable")}
	g.ingressAuth.replay = outage
	other.ingressAuth.replay = outage
	c.ID = "fresh"
	putContext(t, r, c, body, key)
	for _, replica := range []*Gateway{g, other} {
		_, err := replica.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body)
		contextFailure(t, err, 503, "context_invalid")
	}
}

func TestIngressContextCompletionEvidence(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func(*Gateway, *exchangecontext.Claims)
		valid  bool
	}{
		{"granted version", func(*Gateway, *exchangecontext.Claims) {}, true},
		{"unknown version", func(g *Gateway, c *exchangecontext.Claims) { c.Completed[0].Version = "2" }, false},
		{"unknown preparation", func(g *Gateway, c *exchangecontext.Claims) { c.Completed[0].ID = "unknown" }, false},
		{"duplicate completion", func(g *Gateway, c *exchangecontext.Claims) { c.Completed = append(c.Completed, c.Completed[0]) }, false},
		{"missing grant", func(g *Gateway, c *exchangecontext.Claims) {
			reg := g.ingressAuth.clients[c.Issuer]
			reg.BoundaryPreparations = nil
			g.ingressAuth.clients[c.Issuer] = reg
		}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			g, key, c, r, body := ingressContextFixture(t)
			r.URL.Path = "/cds-services/shn-order-sign"
			c.Audience = jwt.ClaimStrings{testIngressBaseURL + r.URL.Path}
			c.Leg = "crd-order-select"
			c.Operation = "crd-order-select"
			c.CRDHook = "order-sign"
			c.Completed = []exchangecontext.Completion{{ID: "E-01", Version: "1"}}
			row.change(g, &c)
			putContext(t, r, c, body, key)
			replay := &stubReplayStore{}
			g.ingressAuth.replay = replay
			ex, err := g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body)
			if row.valid {
				if err != nil || len(ex.boundary) != 1 || ex.boundary[0] != (BoundaryCompletion{"E-01", "1"}) || replay.calls != 1 {
					t.Fatalf("context=%+v err=%v replay=%d", ex, err, replay.calls)
				}
			} else {
				contextFailure(t, err, 403, "boundary_evidence_invalid")
				if replay.calls != 0 {
					t.Fatal("invalid completion consumed replay key")
				}
			}
		})
	}
}

type contextReplayCapture struct {
	scope, client, key string
	now, expiry        time.Time
}

func (s *contextReplayCapture) CheckAndRecord(scope, client, key string, now, expiry time.Time) (bool, error) {
	s.scope, s.client, s.key, s.now, s.expiry = scope, client, key, now, expiry
	return false, nil
}
func TestIngressContextReplayUsesVerifiedBinding(t *testing.T) {
	g, key, c, r, body := ingressContextFixture(t)
	putContext(t, r, c, body, key)
	store := &contextReplayCapture{}
	g.ingressAuth.replay = store
	if _, err := g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body); err != nil {
		t.Fatal(err)
	}
	if store.scope != ReplayScopeIngressContext || store.client != c.Issuer || store.key != c.ID || !store.now.Equal(g.cfg.Clock()) || !store.expiry.Equal(c.ExpiresAt.Time) {
		t.Fatalf("wrong replay binding: %+v", store)
	}
}

// Every context fixture shares these assertions, including each malformed-body,
// hook, completion and outage row. Atomic counters also cover concurrent replicas.
func assertIngressContextNoSideEffects(t *testing.T, g *Gateway) {
	t.Helper()
	sor := &contextCountingSoR{}
	var network atomic.Int32
	g.cfg.SoR = sor
	g.cfg.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		network.Add(1)
		return nil, errors.New("unexpected context dispatch")
	})}
	t.Cleanup(func() {
		if reads, dispatches := sor.calls.Load(), network.Load(); reads != 0 || dispatches != 0 {
			t.Errorf("context side effects: SoR reads=%d network dispatches=%d", reads, dispatches)
		}
	})
}

type contextCountingSoR struct{ calls atomic.Int32 }

func (s *contextCountingSoR) ResolvePatient(string) (string, Demo, bool) {
	s.calls.Add(1)
	return "", Demo{}, false
}
func (s *contextCountingSoR) PatientFHIRRef(string) (string, bool)  { s.calls.Add(1); return "", false }
func (s *contextCountingSoR) CoverageInforce(string) (bool, string) { s.calls.Add(1); return false, "" }
func (s *contextCountingSoR) ClinicalContext(string) (shnsdk.ClinicalContext, bool) {
	s.calls.Add(1)
	return shnsdk.ClinicalContext{}, false
}
func (s *contextCountingSoR) SupplementalReport(string) ([]byte, bool) {
	s.calls.Add(1)
	return nil, false
}
func (s *contextCountingSoR) FacilityRecords(string) (map[string][]byte, bool) {
	s.calls.Add(1)
	return nil, false
}
func (s *contextCountingSoR) OpenOrder(string) ([]byte, bool)    { s.calls.Add(1); return nil, false }
func (s *contextCountingSoR) OpenCoverage(string) ([]byte, bool) { s.calls.Add(1); return nil, false }
func (s *contextCountingSoR) ResolveByReference(string) ([]byte, bool) {
	s.calls.Add(1)
	return nil, false
}

func TestIngressContextBoundaryEvidenceBeforePreparation(t *testing.T) {
	for _, name := range []string{"verified", "operation", "recipient", "body", "completion version"} {
		t.Run(name, func(t *testing.T) {
			g, key, c, r, body := ingressContextFixture(t)
			r.URL.Path = "/cds-services/shn-order-sign"
			c.Audience = jwt.ClaimStrings{testIngressBaseURL + r.URL.Path}
			c.Leg = "crd-order-select"
			c.Operation = "crd-order-select"
			c.CRDHook = "order-sign"
			c.Completed = []exchangecontext.Completion{{ID: "E-01", Version: "1"}}
			switch name {
			case "operation":
				c.Operation = "crd-order-dispatch"
			case "recipient":
				c.Recipient = "unregistered-payer"
			case "completion version":
				c.Completed[0].Version = "2"
			}
			putContext(t, r, c, body, key)
			if name == "body" {
				body = append(body, ' ')
			}
			prepared := 0
			g.cfg.Observer = func(e ObserverEvent) {
				if e.Kind == "boundary.prepared" {
					prepared++
				}
			}
			ex, err := g.resolveIngressContext(context.Background(), r, IngressPrincipal{ClientID: c.Issuer}, body)
			if err == nil {
				_, p, prepErr := g.prepareBoundary(context.Background(), ex, relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), c.ContentType))
				if prepErr != nil || !bytes.Equal(relay.BytesForTest(p), body) {
					t.Fatalf("verified opaque body: %v", prepErr)
				}
			}
			observationFlush(t, g)
			if name == "verified" {
				if err != nil || prepared != 1 {
					t.Fatalf("verified err=%v prepared=%d", err, prepared)
				}
			} else {
				code := "context_invalid"
				if name == "completion version" {
					code = "boundary_evidence_invalid"
				}
				contextFailure(t, err, http.StatusForbidden, code)
				if prepared != 0 {
					t.Fatal("invalid evidence entered preparation")
				}
			}
		})
	}
}
