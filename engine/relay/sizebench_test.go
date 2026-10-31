//go:build shn_sizebench

package relay

// The cost of carrying one large attachment body, measured behind the
// shn_sizebench tag (never in the default run: it holds hundreds of MiB). Each
// side of the leg is measured as its own process would hold it: the
// provider's gateway (strict scan, seal, envelope), the Hub (decode the
// envelope, encode it again to forward), and the payer's gateway (decode,
// open, strict scan). Run:
//
//	go test -tags shn_sizebench,shn_testdouble -run TestAttachmentSizeCost -v ./engine/relay/

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	mrand "math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// attachmentBody is a $submit-attachment Parameters of about n bytes: one
// attachment whose data is base64 of seeded random bytes.
func attachmentBody(n int) []byte {
	const head = `{"resourceType":"Parameters","parameter":[{"name":"Attachment","part":[{"name":"Content","valueAttachment":{"contentType":"application/pdf","data":"`
	const tail = `"}}]}]}`
	raw := make([]byte, (n-len(head)-len(tail))/4*3)
	r := mrand.New(mrand.NewPCG(1, 2))
	for i := range raw {
		raw[i] = byte(r.Uint32())
	}
	return []byte(head + base64.StdEncoding.EncodeToString(raw) + tail)
}

// peak runs f and reports the most heap it held above what was live before,
// the bytes it allocated, and how long it took.
func peak(f func()) (held, allocated uint64, took time.Duration) {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var top atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > top.Load() {
					top.Store(m.HeapAlloc)
				}
			}
		}
	}()
	start := time.Now()
	f()
	took = time.Since(start)
	close(stop)
	wg.Wait()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > top.Load() {
		top.Store(after.HeapAlloc)
	}
	if t := top.Load(); t > before.HeapAlloc {
		held = t - before.HeapAlloc
	}
	return held, after.TotalAlloc - before.TotalAlloc, took
}

func TestAttachmentSizeCost(t *testing.T) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	meta := shnsdk.Metadata{Sender: "provider", Recipient: "payer", TransactionType: "cdex-attachment-submit",
		AuthorityFrame: "provider-tpo", Timestamp: "2026-10-30T00:00:00Z", CorrelationID: "corr-size"}
	limits := splice.Limits{MaxBytes: 1 << 30}
	mib := func(b uint64) string { return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20)) }
	for _, size := range []int{12 << 20, 16 << 20, 64 << 20, 100 << 20} {
		body := attachmentBody(size)
		var envelope []byte
		var sealedLen int
		held, alloc, took := peak(func() {
			if _, err := splice.Scan(body, limits); err != nil {
				t.Fatal(err)
			}
			env, err := shnsdk.Seal(meta, body, pub)
			if err != nil {
				t.Fatal(err)
			}
			sealedLen = len(env.Ciphertext)
			if envelope, err = shnsdk.EncodeEnvelope(env); err != nil {
				t.Fatal(err)
			}
		})
		t.Logf("%d MiB body: provider gateway held %s, allocated %s, %v (scan, seal, envelope of %s)", size>>20, mib(held), mib(alloc), took.Round(time.Millisecond), mib(uint64(len(envelope))))
		var forwarded []byte
		held, alloc, took = peak(func() {
			env, err := shnsdk.DecodeEnvelope(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if forwarded, err = shnsdk.EncodeEnvelope(env); err != nil {
				t.Fatal(err)
			}
		})
		t.Logf("%d MiB body: Hub held %s, allocated %s, %v (decode, re-encode)", size>>20, mib(held), mib(alloc), took.Round(time.Millisecond))
		held, alloc, took = peak(func() {
			env, err := shnsdk.DecodeEnvelope(forwarded)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := shnsdk.Open(env, pub, priv)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := splice.Scan(plain, limits); err != nil {
				t.Fatal(err)
			}
		})
		t.Logf("%d MiB body: payer gateway held %s, allocated %s, %v (decode, open, scan)", size>>20, mib(held), mib(alloc), took.Round(time.Millisecond))
		t.Logf("%d MiB body: ciphertext %s, envelope %s (%.3fx the body); the body within the relay scan's default bound (%s): %v",
			size>>20, mib(uint64(sealedLen)), mib(uint64(len(envelope))), float64(len(envelope))/float64(len(body)), mib(uint64(splice.DefaultLimits().MaxBytes)), int64(len(body)) <= splice.DefaultLimits().MaxBytes)
		envelope, forwarded, body = nil, nil, nil
	}
}
