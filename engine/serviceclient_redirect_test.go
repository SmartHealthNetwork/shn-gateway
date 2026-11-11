package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient/redirecttest"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// lockedLog captures the standard logger's output for one test: the rows
// that use it are not parallel, so no other test writes to it meanwhile.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureRedirectLog(t *testing.T) *lockedLog {
	t.Helper()
	l := &lockedLog{}
	prev := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(prev) })
	return l
}

// refusedClient is a client whose every dial is refused: a service that
// cannot be reached, without a port that another process could take.
func refusedClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}}}
}

// consentGateway is a gateway with only what its consent check reads.
func consentGateway(t *testing.T, consentURL string, consentClient, client *http.Client) *Gateway {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Gateway{cfg: Config{
		HolderID: "provider", Identity: shnsdk.Identity{HolderID: "provider", SignPriv: priv},
		Clock: time.Now, ConsentURL: consentURL, ConsentClient: consentClient, Client: client,
	}}
}

// The consent check's client follows no redirect, the plain client an
// embedding program passes as ConsentClient, or as Client with no
// ConsentClient, included: the check fails as when the service cannot be
// reached (502, "consent check failed"), and its log line names the class
// and the status, never the Location's path or query.
func TestConsentCheckFollowsNoRedirect(t *testing.T) {
	_, wantStatus, wantMsg := consentGateway(t, "http://consent.test", refusedClient(), nil).consentBackstop(context.Background(), "pci-1", "payer")
	if wantStatus != http.StatusBadGateway {
		t.Fatalf("control: an unreachable consent service answered %d", wantStatus)
	}
	for _, via := range []string{"ConsentClient", "Client"} {
		for _, c := range redirecttest.Cases() {
			t.Run(via+"/"+c.Name, func(t *testing.T) {
				f := redirecttest.Serve(t, c)
				logs := captureRedirectLog(t)
				g := consentGateway(t, f.URL, f.Client, nil)
				if via == "Client" {
					g = consentGateway(t, f.URL, nil, f.Client)
				}
				ref, status, msg := g.consentBackstop(context.Background(), "pci-1", "payer")
				f.NotFollowed(t)
				if ref != "" || status != wantStatus || msg != wantMsg {
					t.Errorf("consent = %q %d %q, want the unreachable service's %d %q", ref, status, msg, wantStatus, wantMsg)
				}
				line := fmt.Sprintf("gateway: consent check failed: %s (HTTP %d)\n", redirectRefused, c.Code)
				if !strings.Contains(logs.String(), line) {
					t.Errorf("log %q, want the line %q", logs.String(), line)
				}
				redirecttest.NoSecret(t, "the answer", msg)
				redirecttest.NoSecret(t, "the log", logs.String())
			})
		}
	}
}

// A validator sidecar's client follows no redirect, whether or not a
// recorder tags it (TagValidatorClient), and so does a certification client
// (its own transport): the call fails as a validator that does not answer, an
// error the policy names, and the other origin and path receive nothing.
// The routing, observe and CRD inbound paths call these same clients; a
// $validate failure's record carries no class (only "validator unavailable").
func TestValidatorClientsFollowNoRedirect(t *testing.T) {
	for _, kind := range []string{"routing", "certification", "an embedder's lane", "an embedder's certification client"} {
		for _, c := range redirecttest.Cases() {
			t.Run(kind+"/"+c.Name, func(t *testing.T) {
				f := redirecttest.Serve(t, c)
				var v shnsdk.Validator
				switch kind {
				case "routing":
					op := shnsdk.NewOperationValidator(f.URL)
					op.Client = f.Client
					TagValidatorClient(op, nil, endpointcert.Lane(endpointcert.Validator, "2.1"))
					v = op
				case "certification":
					op := NewCertificationOperationValidator(f.URL)
					if c.TLS() {
						inner := op.Client.Transport.(certificationTransport).inner.(*http.Transport)
						inner.TLSClientConfig = f.Client.Transport.(*http.Transport).TLSClientConfig
					}
					v = op
				case "an embedder's lane":
					// A plain-client validator an embedding program hands a
					// discovered lane.
					op := &shnsdk.OperationValidator{BaseURL: f.URL, Client: f.Client}
					NewDiscoveredLane("2.1", f.URL, op)
					v = op
				default:
					op := &shnsdk.OperationValidator{BaseURL: f.URL, Client: f.Client}
					g := NewQualifiedCertificationValidator("2.2", op, nil)
					defer g.Close()
					v = op
				}
				_, err := v.Validate(context.Background(), []byte(`{"resourceType":"Bundle"}`), "")
				f.NotFollowed(t)
				if !serviceclient.IsRedirectRefused(err) {
					t.Fatalf("err = %v, want a refused redirect", err)
				}
				redirecttest.NoSecret(t, "the error", err.Error())
			})
		}
	}
}

// serviceValidator writes only a validator not under the policy yet: one the
// gateway already put under it is left as it is, and anything else than an
// *shnsdk.OperationValidator is untouched.
func TestServiceValidatorWritesOnlyAnUnpolicedValidator(t *testing.T) {
	plain := &shnsdk.OperationValidator{BaseURL: "http://v.test", Client: &http.Client{}}
	serviceValidator(plain)
	if !validatorFollowsNoRedirect(plain) {
		t.Fatal("a plain client was not put under the policy")
	}
	before := plain.Client
	serviceValidator(plain)
	if plain.Client != before {
		t.Fatal("a validator already under the policy was written again")
	}
	cert := NewCertificationOperationValidator("http://c.test")
	before = cert.Client
	serviceValidator(cert)
	if cert.Client != before {
		t.Fatal("a certification client was rewritten")
	}
	serviceValidator(shnsdk.NewFakeValidator())
	serviceValidator(nil)
	var nilOp *shnsdk.OperationValidator
	serviceValidator(nilOp)
}
