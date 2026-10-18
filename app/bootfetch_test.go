package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// bootPlanes serves discovery, the Authorization Framework's public key and
// the Hub's transport key, each answering its first tries by its own plan
// (a status, 0 to hang up without an answer, or -1 to hang up inside the
// answer's headers) and the key or document after.
type bootPlanes struct {
	authzPub, hubPub                ed25519.PublicKey
	disc, authz, hub                *httptest.Server
	discTries, authzTries, hubTries atomic.Int32
}

func newBootPlanes(t *testing.T, disc, authz, hub []int) *bootPlanes {
	t.Helper()
	p := &bootPlanes{}
	p.authzPub, _, _ = ed25519.GenerateKey(rand.Reader)
	p.hubPub, _, _ = ed25519.GenerateKey(rand.Reader)
	serve := func(tries *atomic.Int32, plan []int, answer func() string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n := int(tries.Add(1)); n <= len(plan) {
				if plan[n-1] <= 0 {
					conn, buf, _ := w.(http.Hijacker).Hijack()
					if plan[n-1] < 0 {
						_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n")
						_ = buf.Flush()
					}
					_ = conn.Close()
					return
				}
				w.WriteHeader(plan[n-1])
				return
			}
			_, _ = w.Write([]byte(answer()))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	key := func(k ed25519.PublicKey) func() string {
		return func() string { return fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(k)) }
	}
	p.authz = serve(&p.authzTries, authz, key(p.authzPub))
	p.hub = serve(&p.hubTries, hub, key(p.hubPub))
	p.disc = serve(&p.discTries, disc, func() string {
		return fmt.Sprintf(`{"endpoints":{"hub":"http://hub.invalid"},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, p.authz.URL, p.hub.URL)
	})
	return p
}

var shortWaits = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}

func (p *bootPlanes) resolve(c *http.Client, waits []time.Duration, budget time.Duration) (trustAnchors, resolvedEndpoints, error) {
	return resolveDiscoveryWaiting(context.Background(), bootClient(c, waits, budget), config{DiscoveryURL: p.disc.URL})
}

// A gateway that boots while a plane rolls to a new task waits for it: each
// boot fetch with no answer, or a proxy's 502, 503 or 504, is tried again,
// and the boot takes the key the plane then serves.
func TestBootFetchesWaitForAPlaneWithNoAnswerYet(t *testing.T) {
	for _, c := range []struct {
		name             string
		disc, authz, hub []int
	}{
		{"discovery 503", []int{503}, nil, nil},
		{"Framework key refused by its proxy twice", nil, []int{503, 503}, nil},
		{"Framework key dropped", nil, []int{0}, nil},
		{"Framework key cut off", nil, []int{-1}, nil},
		{"Framework key 502 then 504", nil, []int{502, 504}, nil},
		{"Hub transport key 503", nil, nil, []int{503}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newBootPlanes(t, c.disc, c.authz, c.hub)
			ta, ep, err := p.resolve(&http.Client{Timeout: 5 * time.Second}, shortWaits, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if !ta.AuthzPub.Equal(p.authzPub) || !ta.HubTransportPub.Equal(p.hubPub) || ep.Hub != "http://hub.invalid" {
				t.Fatalf("booted with other anchors: %+v %+v", ta, ep)
			}
			for name, got := range map[string]int32{"discovery": p.discTries.Load(), "Framework key": p.authzTries.Load(), "Hub key": p.hubTries.Load()} {
				want := map[string][]int{"discovery": c.disc, "Framework key": c.authz, "Hub key": c.hub}[name]
				if int(got) != len(want)+1 {
					t.Errorf("%s fetched %d times, want %d", name, got, len(want)+1)
				}
			}
		})
	}
	t.Run("refused dials", func(t *testing.T) {
		for _, errno := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, 10061, 10054} {
			p := newBootPlanes(t, nil, nil, nil)
			var dials atomic.Int32
			transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if dials.Add(1) == 1 {
					return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", errno)}
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}}
			if _, _, err := p.resolve(&http.Client{Transport: transport}, shortWaits, time.Minute); err != nil || p.discTries.Load() != 1 {
				t.Fatalf("errno %d: %v after %d discovery fetches", errno, err, p.discTries.Load())
			}
		}
	})
}

// The boot's own resolveDiscovery waits with the production waits, and a try
// keeps its bound while its answer streams in, not only until its headers.
func TestResolveDiscoveryWaitsForItsFetches(t *testing.T) {
	p := newBootPlanes(t, []int{503}, nil, nil)
	disc := p.disc.Config.Handler
	p.disc.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.discTries.Load() == 0 {
			disc.ServeHTTP(w, r)
			return
		}
		p.discTries.Add(1)
		// The document in two parts, the second after the headers were read.
		_, _ = w.Write([]byte(`{"endpoints":{"hub":"http://hub.invalid"},`))
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, `"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, p.authz.URL, p.hub.URL)
	})
	ta, ep, err := resolveDiscovery(context.Background(), &http.Client{Timeout: 5 * time.Second}, config{DiscoveryURL: p.disc.URL})
	if err != nil || p.discTries.Load() != 2 || ep.Hub != "http://hub.invalid" || !ta.AuthzPub.Equal(p.authzPub) {
		t.Fatalf("%v after %d discovery fetches", err, p.discTries.Load())
	}
}

// Every other answer fails the boot at once, as before: a key is never
// fetched again into acceptance.
func TestBootFetchesFailAtOnceOnAnyOtherAnswer(t *testing.T) {
	for _, c := range []struct {
		name  string
		authz []int
		key   string // served in place of the key, when set
		want  string
	}{
		{"404", []int{404}, "", "fetch authz pubkey"},
		{"401", []int{401}, "", "fetch authz pubkey"},
		{"500", []int{500}, "", "fetch authz pubkey"},
		{"malformed key", nil, `{"pubkey":"not-base64!"}`, "decode base64"},
		{"short key", nil, `{"pubkey":"AAAA"}`, "want 32 bytes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newBootPlanes(t, nil, c.authz, nil)
			if c.key != "" {
				p.authz.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					p.authzTries.Add(1)
					_, _ = w.Write([]byte(c.key))
				})
			}
			_, _, err := p.resolve(&http.Client{Timeout: 5 * time.Second}, shortWaits, time.Minute)
			if err == nil || !strings.Contains(err.Error(), c.want) || p.authzTries.Load() != 1 {
				t.Fatalf("%v after %d fetches, want %q after 1", err, p.authzTries.Load(), c.want)
			}
		})
	}
}

// The waits end: a plane still not answering after every wait, or past the
// budget, fails the boot and names the fetch; each try keeps the client's
// own bound; and a boot whose context ends stops waiting at once.
func TestBootFetchesWaitWithinBounds(t *testing.T) {
	t.Run("the last wait repeats until the budget", func(t *testing.T) {
		outage := make([]int, 100)
		for i := range outage {
			outage[i] = 503
		}
		p := newBootPlanes(t, nil, outage, nil)
		_, _, err := p.resolve(&http.Client{Timeout: 5 * time.Second}, []time.Duration{10 * time.Millisecond}, 500*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "fetch authz pubkey") || !strings.Contains(err.Error(), "status 503") || p.authzTries.Load() < 3 {
			t.Fatalf("%v after %d fetches, want the 503 after the waits", err, p.authzTries.Load())
		}
	})
	t.Run("budget", func(t *testing.T) {
		p := newBootPlanes(t, nil, []int{503, 503, 503, 503, 503, 503}, nil)
		_, _, err := p.resolve(&http.Client{Timeout: 5 * time.Second}, []time.Duration{time.Second}, 1500*time.Millisecond)
		if err == nil || p.authzTries.Load() != 2 {
			t.Fatalf("%v after %d fetches, want no try started past the budget", err, p.authzTries.Load())
		}
	})
	t.Run("each try bounded", func(t *testing.T) {
		release := make(chan struct{})
		p := newBootPlanes(t, nil, nil, nil)
		p.authz.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			p.authzTries.Add(1)
			<-release
		})
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		began := time.Now()
		_, _, err := resolveDiscoveryWaiting(ctx, bootClient(&http.Client{Timeout: 500 * time.Millisecond}, shortWaits, time.Minute), config{DiscoveryURL: p.disc.URL})
		if err == nil || p.authzTries.Load() != 1 || time.Since(began) > 5*time.Second {
			t.Fatalf("%v after %d fetches, %s", err, p.authzTries.Load(), time.Since(began))
		}
	})
	t.Run("boot cancelled", func(t *testing.T) {
		p := newBootPlanes(t, []int{503, 503, 503, 503, 503}, nil, nil)
		ctx, cancel := context.WithCancel(context.Background())
		hc := bootClient(&http.Client{Timeout: 5 * time.Second}, []time.Duration{time.Hour}, 2*time.Hour)
		go func() {
			for p.discTries.Load() == 0 {
				time.Sleep(time.Millisecond)
			}
			// Well into the hour's wait, not the first try.
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		done := make(chan error, 1)
		go func() { _, _, err := resolveDiscoveryWaiting(ctx, hc, config{DiscoveryURL: p.disc.URL}); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) || p.discTries.Load() != 1 {
				t.Fatalf("%v after %d fetches", err, p.discTries.Load())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the boot kept waiting after its context ended")
		}
	})
	t.Run("tries share the budget, not the client's timeout", func(t *testing.T) {
		// The client's timeout bounds each try; the waits between tries may
		// run past it.
		p := newBootPlanes(t, []int{503}, nil, nil)
		_, _, err := p.resolve(&http.Client{Timeout: time.Second}, []time.Duration{1500 * time.Millisecond}, time.Minute)
		if err != nil || p.discTries.Load() != 2 {
			t.Fatalf("%v after %d fetches", err, p.discTries.Load())
		}
	})
}
