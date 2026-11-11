//go:build !shn_coreonly

package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient/redirecttest"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The registrar's client follows no redirect: build puts it under the
// network-service policy (TestMONR25BuildTagsEachConsumerOverItsOwnTransport
// asserts it), and a capability PUT answered with a redirect fails as when
// the registrar cannot be reached: not stored, retried, its log line naming
// the class redirect-refused, never the Location's path or query; the other
// origin and path receive nothing.
func TestDeclareCapabilities_FollowsNoRedirect(t *testing.T) {
	for _, c := range redirecttest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			f := redirecttest.Serve(t, c)
			var out syncBuffer
			prev := log.Writer()
			log.SetOutput(&out)
			defer log.SetOutput(prev)
			// The declaration waits an hour after its first failure: stop it
			// once that failure's line is logged, within a long backstop.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				declareCapabilities(ctx, serviceclient.ServiceClient(f.Client), f.URL, capabilitiesIdentity(t), shnsdk.CapabilityDeclaration{Legs: []string{}}, time.Hour, &declarationCell{})
			}()
			want := "capability declaration not stored (" + serviceclient.RedirectRefused + ")"
			backstop := time.After(10 * time.Second)
			for !strings.Contains(out.String(), want) {
				select {
				case <-backstop:
					cancel()
					<-done
					t.Fatalf("no log line names the class within 10s: %q", out.String())
				case <-done:
					t.Fatalf("the declaration ended before failing: %q", out.String())
				case <-time.After(5 * time.Millisecond):
				}
			}
			cancel()
			<-done
			f.NotFollowed(t)
			redirecttest.NoSecret(t, "the log", out.String())
		})
	}
}

// The registrar's holder feed shares that client: a redirecting registrar
// fails the read (the boot convergence, the poller's tick and the refresh on
// an unknown holder alike), its error naming redirect-refused and the status,
// and the registry stays as it was; the other origin and path receive
// nothing.
func TestRegistryFeed_FollowsNoRedirect(t *testing.T) {
	for _, c := range redirecttest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			f := redirecttest.Serve(t, c)
			reg := shnsdk.NewRegistry()
			reg.Set("known", shnsdk.RegistryEntry{ID: "known", Role: "payer"})
			client := serviceclient.ServiceClient(f.Client)
			n, err := convergeRegistry(context.Background(), client, f.URL, reg)
			refreshErr := registryRefresh(client, f.URL, reg, newFeedFreshness(time.Now))(context.Background())
			f.NotFollowed(t)
			for _, e := range []error{err, refreshErr} {
				if e == nil || !strings.Contains(e.Error(), serviceclient.RedirectRefused) || !strings.Contains(e.Error(), fmt.Sprintf("answered %d", c.Code)) {
					t.Fatalf("feed read = %v, want a failure naming redirect-refused and %d", e, c.Code)
				}
				redirecttest.NoSecret(t, "the error", e.Error())
			}
			if n != 0 {
				t.Fatalf("%d holders converged from a redirect", n)
			}
			if _, ok := reg.Lookup("known"); !ok {
				t.Fatal("the registry lost what it held")
			}
		})
	}
}
