//go:build !shn_coreonly

package engine

// workstream_pa_cdex_size_test.go — the size of a documentation attachment
// submission. Its body is bounded by each gateway's attachment ceiling (by
// default the network's, shnsdk.MaxAttachmentBytes, the bound the strict
// JSON scan reads a body to), and its sealed envelope by the leg's own
// envelope limit (shnsdk.MaxRequestBytesFor), above the network's limit for
// every other leg. A submission over either is refused 413 as routing,
// before anything parses it or anything is sent, at every level.

import (
	"bytes"
	"compress/gzip"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdexSubmissionOf is a submission for MBR-COVERED naming the CMS payer, n
// bytes long: the same submission, padded with the whitespace JSON allows
// before its closing brace.
func cdexSubmissionOf(t *testing.T, n int) []byte {
	t.Helper()
	body := cdexSubmissionBody("MBR-COVERED", cdexPayerCMS)
	if len(body) > n {
		t.Fatalf("submission %d bytes, over %d before padding", len(body), n)
	}
	out := make([]byte, 0, n)
	out = append(out, body[:len(body)-1]...)
	out = append(out, bytes.Repeat([]byte(" "), n-len(body))...)
	return append(out, '}')
}

// A submission exactly at the network's ceiling is carried whole, through the
// provider's ingress to the payer's side: its envelope is over the network's
// limit for every other leg, and under the leg's own. At one level here, since
// sealing it is dear: test/adversarial carries it at every level.
func TestCDexIngress_ASubmissionAtTheCeilingIsCarriedWhole(t *testing.T) {
	t.Parallel()
	body := cdexSubmissionOf(t, shnsdk.MaxAttachmentBytes)
	env, rec, x := runCDexIngress(t, EnforcementObserve, body, "", LegResult{Response: testResponse(cdexPayersAnswer)})
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), cdexPayersAnswer) {
		t.Fatalf("answer %d %.300s, want the payer's answer relayed", rec.Code, rec.Body)
	}
	if sent := env.lastRequestPayload(); !bytes.Equal(sent, body) {
		t.Fatalf("the payer's side received %d bytes, want the %d-byte submission as sent", len(sent), len(body))
	}
	if x.Outcome != ExchangeAnswered {
		t.Fatalf("record %+v, want answered", x)
	}
}

// A participant may set a lower ceiling: a submission exactly at it is
// carried, and one byte over it is refused 413 with the published reason,
// recorded as the provider gateway's routing refusal, never authorized or
// sent, at every level.
func TestCDexIngress_AConfiguredCeilingRefusesOneByteOver(t *testing.T) {
	t.Parallel()
	const ceiling = 1 << 20
	for _, level := range allLevels {
		t.Run("over/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.AttachmentCeiling = ceiling
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCDexSubmit)(rec, cdexPost(cdexSubmissionOf(t, ceiling+1), ""))
			refusedBeforeTheNetwork(t, env, rec, http.StatusRequestEntityTooLarge, `"diagnostics":"`+errRequestTooLarge+`"`)
			if env.substrate.authorizeHits != 0 {
				t.Fatalf("the refused submission was authorized (%d)", env.substrate.authorizeHits)
			}
			wantRefusal(t, got.only(t), http.StatusRequestEntityTooLarge, RefusedByProviderGateway, RefusalRouting)
		})
		t.Run("at/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.AttachmentCeiling = ceiling
			env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCDexSubmit)(rec, cdexPost(cdexSubmissionOf(t, ceiling), ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %.300s, want a submission at the ceiling carried", rec.Code, rec.Body)
			}
		})
	}
}

// A ceiling of zero, or one above the network's, is the network's ceiling:
// the gateway never reads more than the strict JSON scan can.
func TestAttachmentCeiling_NeverAboveTheNetworks(t *testing.T) {
	for set, want := range map[int64]int64{
		0:                             shnsdk.MaxAttachmentBytes,
		-1:                            shnsdk.MaxAttachmentBytes,
		1 << 20:                       1 << 20,
		shnsdk.MaxAttachmentBytes:     shnsdk.MaxAttachmentBytes,
		shnsdk.MaxAttachmentBytes + 1: shnsdk.MaxAttachmentBytes,
	} {
		g := &Gateway{cfg: Config{AttachmentCeiling: set}}
		if got := g.attachmentCeiling(); got != want {
			t.Errorf("AttachmentCeiling %d: ceiling %d, want %d", set, got, want)
		}
	}
}

// The attachment leg's envelope has its own limit: a body at the network's
// ceiling seals under it and is sent, and a body the ingress would never read
// (over the ceiling) seals over it and is refused naming that limit, never
// authorized or sent. Every other leg keeps the network's limit.
func TestOriginateLeg_TheAttachmentLegsEnvelopeHasItsOwnLimit(t *testing.T) {
	t.Parallel()
	// The submission is carried as the ingress carries it; the other leg's
	// request is the test's own.
	originate := func(env *inProcessExchange, leg string, body []byte) error {
		content := Content{WorkstreamType: workstreamPA, Payload: testRequest(body)}
		if leg == legCDexSubmit {
			content = Content{WorkstreamType: workstreamPA, Payload: relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), "application/fhir+json"), Carried: true}
		}
		_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, leg, "pci-1", "corr-size", "", content)
		return err
	}
	t.Run("at the ceiling", func(t *testing.T) {
		env := newInProcessExchange(t)
		env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
		if err := originate(env, legCDexSubmit, cdexSubmissionOf(t, shnsdk.MaxAttachmentBytes)); err != nil {
			t.Fatalf("err %v, want the submission sent", err)
		}
	})
	t.Run("exactly at the leg's envelope limit, and the next size over", func(t *testing.T) {
		// The sealed size moves in steps of 4 bytes, so the correlation
		// identifier's length brings it onto the limit exactly.
		env := newInProcessExchange(t)
		env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
		var sent atomic.Int64
		base := env.originator.cfg.Client.Transport
		env.originator.cfg.Client.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/route") {
				sent.Store(r.ContentLength)
			}
			return base.RoundTrip(r)
		})
		send := func(corr string, n int) error {
			_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, legCDexSubmit, "pci-1", corr, "", Content{WorkstreamType: workstreamPA, Payload: relay.Exact(relay.NewBody(cdexSubmissionOf(t, n), relay.OriginIngressRequest), "application/fhir+json"), Carried: true})
			return err
		}
		// Probed at a small size, since sealing a large one is dear: the size
		// the correlation identifier settles is the same at any length.
		const start = 1 << 20
		corr, n := "", 0
		for pad := range 8 {
			c := "corr-" + strings.Repeat("x", pad+1)
			if err := send(c, start); err != nil {
				t.Fatal(err)
			}
			if gap := int64(shnsdk.MaxAttachmentRequestBytes) - sent.Load(); gap%4 == 0 {
				corr, n = c, start+int(gap/4*3)
				break
			}
		}
		if corr == "" {
			t.Fatal("no correlation identifier brought the envelope onto the limit")
		}
		if err := send(corr, n); err != nil || sent.Load() != int64(shnsdk.MaxAttachmentRequestBytes) {
			t.Fatalf("err %v, %d bytes sent, want an envelope of exactly %d", err, sent.Load(), shnsdk.MaxAttachmentRequestBytes)
		}
		if err := send(corr, n+3); !errors.Is(err, errEnvelopeTooLarge) {
			t.Fatalf("err %v, want the envelope refused 4 bytes over", err)
		}
	})
	t.Run("over the leg's envelope limit", func(t *testing.T) {
		env := newInProcessExchange(t)
		err := originate(env, legCDexSubmit, cdexSubmissionOf(t, shnsdk.MaxAttachmentBytes+(1<<20)))
		if !errors.Is(err, errEnvelopeTooLarge) || !strings.Contains(err.Error(), "22 MiB") {
			t.Fatalf("err %v, want the leg's 22 MiB envelope limit named", err)
		}
		if n := env.routeHitCount(); n != 0 || env.substrate.authorizeHits != 0 {
			t.Fatalf("the refused request crossed the network (%d) or was authorized (%d)", n, env.substrate.authorizeHits)
		}
	})
	t.Run("another leg keeps the network's limit", func(t *testing.T) {
		env := newInProcessExchange(t)
		err := originate(env, "crd-order-select", []byte(`{"a":"`+strings.Repeat("a", 9<<20)+`"}`))
		if !errors.Is(err, errEnvelopeTooLarge) || !strings.Contains(err.Error(), "8 MiB") {
			t.Fatalf("err %v, want the network's 8 MiB envelope limit named", err)
		}
	})
}

// The payer's gateway reads a submission's envelope up to the leg's own
// limit: a submission whose envelope is over the network's limit for every
// other leg reaches the payer's system whole. One over its own ceiling is
// refused 413 as routing before the payer's system is called, and one at it
// is carried, at every level.
func TestCDexPayer_ItsOwnCeilingBoundsASubmission(t *testing.T) {
	t.Parallel()
	const ceiling = 1 << 20
	t.Run("over the network's limit for other legs", func(t *testing.T) {
		p, _ := newRecordingLevelPayer(t, EnforcementObserve)
		p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
		body := cdexSubmissionOf(t, 7<<20) // seals to over 8 MiB
		ans := p.send(t, legCDexSubmit, "", body)
		if ans.status != http.StatusOK || !bytes.Equal(p.partner.lastBody, body) {
			t.Fatalf("answer %d %.300s, want the submission carried whole to the payer's system", ans.status, ans.body)
		}
	})
	for _, level := range allLevels {
		t.Run("over its ceiling/"+level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.g.cfg.AttachmentCeiling = ceiling
			p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
			ans := p.send(t, legCDexSubmit, "", cdexSubmissionOf(t, ceiling+1))
			if ans.status != http.StatusRequestEntityTooLarge || !strings.Contains(string(ans.body), errRequestTooLarge) || p.partner.calls.Load() != 0 {
				t.Fatalf("answer %d %s (payer system calls %d), want 413 %s and no call", ans.status, ans.body, p.partner.calls.Load(), errRequestTooLarge)
			}
			wantRefusal(t, got.only(t), http.StatusRequestEntityTooLarge, RefusedByPayerGateway, RefusalRouting)
		})
		t.Run("at its ceiling/"+level.String(), func(t *testing.T) {
			p, _ := newRecordingLevelPayer(t, level)
			p.g.cfg.AttachmentCeiling = ceiling
			p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
			if ans := p.send(t, legCDexSubmit, "", cdexSubmissionOf(t, ceiling)); ans.status != http.StatusOK {
				t.Fatalf("answer %d %s, want a submission at the ceiling carried", ans.status, ans.body)
			}
		})
	}
}

// storedGzip is b as a gzip stream that does not compress: a coded body as long
// as its decoded one, as an inline file nearly is, with no compressing to pay.
func storedGzip(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := gzip.NewWriterLevel(&out, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// A gzip-coded submission is held to the bounds an uncoded one is, not to the
// network's limit for other legs: the provider's ingress reads it to the
// gateway's ceiling, and the payer's inbound route reads its envelope to the
// largest of any leg. Only the routes that carry a documentation attachment
// read past the network's limit.
func TestCDex_ACodedSubmissionHasTheBoundsOfAnUncodedOne(t *testing.T) {
	t.Parallel()
	limits := func(g *Gateway) map[string]int64 {
		out := map[string]int64{}
		for _, rt := range append(g.paRoutes("provider"), g.coreRoutes("payer")...) {
			if rt.bodyLimit != 0 {
				out[rt.pattern] = rt.bodyLimit
			}
		}
		return out
	}
	// post is the submission through the route as this gateway mounts it.
	post := func(t *testing.T, env *inProcessExchange, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		env.originator.cfg.IngressEnabled = true
		r := httptest.NewRequest(http.MethodPost, "/$submit-attachment", bytes.NewReader(storedGzip(t, body)))
		r.Header.Set("Content-Type", "application/fhir+json")
		r.Header.Set("Content-Encoding", "gzip")
		rec := httptest.NewRecorder()
		env.originator.routes().ServeHTTP(rec, r)
		return rec
	}
	env := newInProcessExchange(t)
	env.originator.cfg.IngressEnabled = true
	want := map[string]int64{
		"POST /$submit-attachment":                    shnsdk.MaxAttachmentBytes,
		"POST /cdex/payer/{payer}":                    shnsdk.MaxAttachmentBytes,
		"POST /cdex/payer/{payer}/$submit-attachment": shnsdk.MaxAttachmentBytes,
		"POST /substrate/inbound":                     int64(maxEnvelopeBytes),
	}
	if got := limits(env.originator); !maps.Equal(got, want) {
		t.Fatalf("routes reading past the network's limit %v, want %v", got, want)
	}
	env.originator.cfg.AttachmentCeiling = 1 << 20
	if got := limits(env.originator)["POST /$submit-attachment"]; got != 1<<20 {
		t.Fatalf("a ceiling of 1 MiB reads to %d, want the ceiling", got)
	}
	// Past the coded stream's bound for the network's limit as well.
	overTheNetworksLimit := cdexSubmissionOf(t, 9<<20)
	t.Run("past the network's limit, carried", func(t *testing.T) {
		env := newInProcessExchange(t)
		env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
		if rec := post(t, env, overTheNetworksLimit); rec.Code != http.StatusOK {
			t.Fatalf("answer %d %.300s, want the submission carried", rec.Code, rec.Body)
		}
		if sent := env.lastRequestPayload(); !bytes.Equal(sent, overTheNetworksLimit) {
			t.Fatalf("the payer's side received %d bytes, want the %d-byte submission", len(sent), len(overTheNetworksLimit))
		}
	})
	for _, level := range allLevels {
		t.Run("one byte over the ceiling/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.AttachmentCeiling = 1 << 20
			env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
			if rec := post(t, env, cdexSubmissionOf(t, 1<<20+1)); rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), errRequestTooLarge) {
				t.Fatalf("answer %d %.300s, want 413 %s", rec.Code, rec.Body, errRequestTooLarge)
			}
		})
	}
	t.Run("any other route as mounted reads a coded body", func(t *testing.T) {
		env := newInProcessExchange(t)
		env.originator.cfg.IngressEnabled = true
		// Past the bound a route with no limit of its own would take, and
		// refused for what it is, never for its size.
		r := env.crdIngressRequest(t)
		coded := httptest.NewRequest(http.MethodPost, r.URL.Path, bytes.NewReader(storedGzip(t, cdexSubmissionOf(t, 64<<10))))
		coded.Header = r.Header.Clone()
		coded.Header.Set("Content-Encoding", "gzip")
		rec := httptest.NewRecorder()
		env.originator.routes().ServeHTTP(rec, coded)
		if rec.Code == http.StatusRequestEntityTooLarge {
			t.Fatalf("answer %d %.300s, want the coded body read", rec.Code, rec.Body)
		}
	})
	t.Run("control: the network's limit refuses it", func(t *testing.T) {
		env := newInProcessExchange(t)
		r := cdexPost(storedGzip(t, overTheNetworksLimit), "")
		r.Header.Set("Content-Encoding", "gzip")
		rec := httptest.NewRecorder()
		codedRoute(shnsdk.MaxRequestBytes, noCredential, env.originator.ingressRoute(cdexRoute(""))).ServeHTTP(rec, r)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("answer %d %.300s, want 413", rec.Code, rec.Body)
		}
	})
	t.Run("the payer's inbound route", func(t *testing.T) {
		p, _ := newRecordingLevelPayer(t, EnforcementObserve)
		p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
		p.coding = "gzip"
		p.envelope = func(raw []byte) []byte {
			return storedGzip(t, append(raw, bytes.Repeat([]byte(" "), 9<<20-len(raw))...))
		}
		body := cdexSubmissionBody("MBR-COVERED", cdexPayerCMS)
		if ans := p.send(t, legCDexSubmit, "", body); ans.status != http.StatusOK || !bytes.Equal(p.partner.lastBody, body) {
			t.Fatalf("answer %d %.300s, want the submission carried whole to the payer's system", ans.status, ans.body)
		}
	})
}

// An envelope over the attachment leg's own limit is refused 413 at the
// payer's gateway, recorded as its routing refusal, and the payer's system is
// not called; one exactly at it is read.
func TestCDexPayer_AnEnvelopeOverTheLegsLimitIs413(t *testing.T) {
	t.Parallel()
	padTo := func(n int) func([]byte) []byte {
		return func(raw []byte) []byte { return append(raw, bytes.Repeat([]byte(" "), n-len(raw))...) }
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
			p.envelope = padTo(shnsdk.MaxAttachmentRequestBytes + 1)
			ans := p.send(t, legCDexSubmit, "", cdexSubmissionBody("MBR-COVERED", cdexPayerCMS))
			if ans.status != http.StatusRequestEntityTooLarge || p.partner.calls.Load() != 0 {
				t.Fatalf("answer %d %s (payer system calls %d), want 413 and no call", ans.status, ans.body, p.partner.calls.Load())
			}
			wantRefusal(t, got.only(t), http.StatusRequestEntityTooLarge, RefusedByPayerGateway, RefusalRouting)
		})
	}
	t.Run("at the limit", func(t *testing.T) {
		p, _ := newRecordingLevelPayer(t, EnforcementObserve)
		p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
		p.envelope = padTo(shnsdk.MaxAttachmentRequestBytes)
		if ans := p.send(t, legCDexSubmit, "", cdexSubmissionBody("MBR-COVERED", cdexPayerCMS)); ans.status != http.StatusOK {
			t.Fatalf("answer %d %s, want the envelope read", ans.status, ans.body)
		}
	})
}

// declareAttachmentLimit sets the payer's declared attachmentMaxBytes in the
// provider gateway's registry, as the registry feed would carry it.
func declareAttachmentLimit(t *testing.T, env *inProcessExchange, limit int64) {
	t.Helper()
	entry, ok := env.originator.cfg.Reg.Lookup(env.payerID)
	if !ok {
		t.Fatalf("no registry entry for %s", env.payerID)
	}
	entry.AttachmentMaxBytes = &limit
	env.originator.cfg.Reg.Set(env.payerID, entry)
}

// readCDexFixture is one of the synthetic submissions under
// testdata/cdex-attachments.
func readCDexFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cdex-attachments", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The provider's gateway refuses, before anything is sent, a submission that
// carries an inline file over the largest its payer declares its system
// accepts: 413 naming both sizes, recorded as its routing refusal, never
// authorized or sent, at every level. A file exactly at the declared limit,
// several files each under it whatever their sum, a file by url, and data
// that is not well-formed base64 are carried, and so is every submission to a
// payer that declares no limit.
func TestCDexIngress_AFileOverThePayersDeclaredLimitIsRefused(t *testing.T) {
	t.Parallel()
	m := readCDexAttachmentFixtures(t)
	for _, f := range m.Fixtures {
		over := f.Measured && f.LargestInline > m.DeclaredLimit
		for _, level := range allLevels {
			t.Run(f.File+"/"+level.String(), func(t *testing.T) {
				body := readCDexFixture(t, f.File)
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				declareAttachmentLimit(t, env, m.DeclaredLimit)
				env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteCDexSubmit)(rec, cdexPost(body, ""))
				if over {
					want := errAttachmentTooLarge + ": an inline attachment is 65537 bytes, over the 65536 the payer's system accepts"
					refusedBeforeTheNetwork(t, env, rec, http.StatusRequestEntityTooLarge, `"diagnostics":"`+want+`"`)
					if env.substrate.authorizeHits != 0 {
						t.Fatalf("the refused submission was authorized (%d)", env.substrate.authorizeHits)
					}
					wantRefusal(t, got.only(t), http.StatusRequestEntityTooLarge, RefusedByProviderGateway, RefusalRouting)
					return
				}
				if rec.Code != http.StatusOK || !bytes.Equal(env.lastRequestPayload(), body) {
					t.Fatalf("answer %d %.300s, want %s (%s) carried as sent", rec.Code, rec.Body, f.File, f.What)
				}
			})
		}
	}
	t.Run("a payer that declares none", func(t *testing.T) {
		body := readCDexFixture(t, "over-limit.json")
		env := newInProcessExchange(t)
		env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
		rec := httptest.NewRecorder()
		env.originator.ingressRoute(RouteCDexSubmit)(rec, cdexPost(body, ""))
		if rec.Code != http.StatusOK || !bytes.Equal(env.lastRequestPayload(), body) {
			t.Fatalf("answer %d %.300s, want the submission carried: no limit declared is unknown, never a refusal", rec.Code, rec.Body)
		}
	})
}

// The declared limit is the payer's statement, applied together with this
// gateway's ceiling: the smaller of the two bounds a file. A declaration
// above the ceiling refuses nothing the ceiling carries.
func TestAttachmentOverDeclared_TheSmallerOfTheDeclarationAndTheCeiling(t *testing.T) {
	file := readCDexFixture(t, "over-limit.json") // one file of 65537 bytes
	reg := shnsdk.NewRegistry()
	g := &Gateway{cfg: Config{Reg: reg}}
	for name, row := range map[string]struct {
		declared *int64
		ceiling  int64
		refused  bool
	}{
		"declared under the file":                     {ptrTo(int64(65536)), 0, true},
		"declared at the file":                        {ptrTo(int64(65537)), 0, false},
		"declared far above the ceiling":              {ptrTo(int64(1 << 40)), 0, false},
		"declared none":                               {nil, 0, false},
		"declared zero, which no declaration carries": {ptrTo(int64(0)), 0, false},
		"declared negative":                           {ptrTo(int64(-1)), 0, false},
		"a ceiling under the declared limit":          {ptrTo(int64(1 << 20)), 65536, true},
	} {
		t.Run(name, func(t *testing.T) {
			reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", AttachmentMaxBytes: row.declared})
			g.cfg.AttachmentCeiling = row.ceiling
			status, msg := g.attachmentOverDeclared("payer", file)
			if (status == http.StatusRequestEntityTooLarge) != row.refused || (status != 0 && status != http.StatusRequestEntityTooLarge) {
				t.Fatalf("status %d %q, refused want %v", status, msg, row.refused)
			}
		})
	}
	if status, _ := g.attachmentOverDeclared("no-such-holder", file); status != 0 {
		t.Fatal("a recipient the registry does not hold refused here")
	}
}

func ptrTo[T any](v T) *T { return &v }
