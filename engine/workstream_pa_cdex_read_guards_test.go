//go:build !shn_coreonly

package engine

// workstream_pa_cdex_read_guards_test.go — two guards on the payer's read of
// its CDex Task, each with its rejection row and its control: a read's path
// rides only on the leg whose row defines one (errReadPathUndefined), and a
// provider's answer that repeats a member name is not delivered
// (repeatsAMember).

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A round trip that frames a read's path on a leg whose row defines none (the
// attachment request) is refused errReadPathUndefined before anything leaves
// the gateway: no token is asked for and nothing is posted to the Hub. The
// control: the same read on the read leg, whose row defines one, is
// authorized and posted once, its path in the frame.
func TestRoundTrip_AReadPathRidesOnlyOnItsLeg(t *testing.T) {
	read := func() Content {
		return Content{WorkstreamType: workstreamPA, Payload: relay.Exact(relay.NewBody(nil, relay.OriginIngressRequest), ""), Carried: true, ReadPath: "/Task/t-1"}
	}
	t.Run("on a leg that defines none", func(t *testing.T) {
		p := newReadPayer(t, EnforcementNone)
		req := httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/cdex/provider/"+readProviderID+"/Task/t-1", nil)
		if spec, _ := legSpecFor(legCDexRequest); spec.ReadPath {
			t.Fatalf("%s defines a read path: the row would not reach the guard", legCDexRequest)
		}
		_, err := p.g.OriginateLeg(t.Context(), req, readProviderID, legCDexRequest, "", "corr-readpath-1", "", read())
		if !errors.Is(err, errReadPathUndefined) {
			t.Fatalf("err %v, want errReadPathUndefined", err)
		}
		routed, authorized, _ := p.hub.snapshot()
		if len(routed) != 0 || len(authorized) != 0 {
			t.Fatalf("%d routed, %d authorized: want nothing sent", len(routed), len(authorized))
		}
	})
	t.Run("control: the read leg", func(t *testing.T) {
		p := newReadPayer(t, EnforcementNone)
		p.hub.answerBody = readTaskAnswer(t, nil)
		req := httptest.NewRequest(http.MethodGet, testIngressBaseURL+"/cdex/provider/"+readProviderID+"/Task/t-1", nil)
		if _, err := p.g.OriginateLeg(t.Context(), req, readProviderID, legCDexTaskStatus, "", "corr-readpath-2", "", read()); err != nil {
			t.Fatalf("the read leg: %v", err)
		}
		routed, authorized, _ := p.hub.snapshot()
		if len(routed) != 1 || len(authorized) != 1 {
			t.Fatalf("%d routed, %d authorized: want the read sent once", len(routed), len(authorized))
		}
		if got := routed[0].headers[shnsdk.FrameHeaderReadPath]; got != "/Task/t-1" {
			t.Fatalf("frame headers %v, want readPath /Task/t-1", routed[0].headers)
		}
	})
}

// A provider's answer to the read that repeats a member name can be read two
// ways, so it is not delivered, at every level: the payer's system is answered
// 502 naming why, the read is recorded refused by the payer's gateway on
// message integrity, and, though its token names a patient, no binding record
// is sent, since nothing was disclosed to the payer's system. The control: the
// same Task without the repeat is delivered as it came, and its binding sent.
func TestCDexReadIngress_AnAnswerRepeatingAMemberIsNotDelivered(t *testing.T) {
	for _, level := range allLevels {
		for _, repeat := range []bool{true, false} {
			name := "repeats a member"
			if !repeat {
				name = "control"
			}
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newReadPayer(t, level)
				task := readTaskAnswer(t, nil)
				if repeat {
					if !bytes.Contains(task, []byte(`"status":"requested"`)) {
						t.Fatalf("the Task names no status to repeat: %s", task)
					}
					task = bytes.Replace(task, []byte(`"status":"requested"`), []byte(`"status":"requested","status":"completed"`), 1)
				}
				p.hub.answerBody, p.hub.answerHeaders, p.hub.answerSubject = task, readAnswerHeaders, "pci:00000000000000000000000000000d15"
				rec := p.get(t, "/cdex/provider/"+readProviderID+"/Task/t-1")
				_, _, posts := p.hub.snapshot()
				x := p.recs.only(t)
				if !repeat {
					if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), task) {
						t.Fatalf("answer %d %s, want the provider's Task as it came", rec.Code, rec.Body)
					}
					if len(posts) != 1 || x.RequesterBinding == nil || x.RequesterBinding.Outcome != RequesterBindingAccepted {
						t.Fatalf("%d binding records, record %+v: want one, accepted", len(posts), x.RequesterBinding)
					}
					return
				}
				if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "the provider's answer can be read two ways: it repeats a member name") || bytes.Contains(rec.Body.Bytes(), task) {
					t.Fatalf("answer %d %s, want 502 naming the repeat, without the Task", rec.Code, rec.Body)
				}
				wantRefusal(t, x, http.StatusBadGateway, RefusedByPayerGateway, RefusalIntegrity)
				if len(posts) != 0 || x.RequesterBinding != nil {
					t.Fatalf("%d binding records, record %+v: want none for an answer not delivered", len(posts), x.RequesterBinding)
				}
			})
		}
	}
}

// What CONFIGURATION.md says of a read's path, pinned: an id holding an
// escaped "/" and a _history segment spelled escaped reach the read route and
// are refused 400 on routing, recorded; another segment after the id or the
// version is no read route, answered the plain 404 of a path the gateway does
// not serve, with no exchange record. Nothing is sent either way.
func TestCDexReadIngress_APathThatIsNoReadSendsNothing(t *testing.T) {
	base := "/cdex/provider/" + readProviderID
	for _, row := range []struct {
		target string
		status int
		record bool
	}{
		{base + "/Task/a%2Fb", http.StatusBadRequest, true},
		{base + "/Task/t-1/%5Fhistory/2", http.StatusBadRequest, true},
		{base + "/Task/t-1/extra", http.StatusNotFound, false},
		{base + "/Task/t-1/_history/2/extra", http.StatusNotFound, false},
		{"/Task/t-1/extra", http.StatusNotFound, false},
	} {
		t.Run(row.target, func(t *testing.T) {
			p := newReadPayer(t, EnforcementNone)
			rec := p.get(t, row.target)
			if rec.Code != row.status {
				t.Fatalf("answer %d %s, want %d", rec.Code, rec.Body, row.status)
			}
			if routed, authorized, _ := p.hub.snapshot(); len(routed) != 0 || len(authorized) != 0 {
				t.Fatalf("%d routed, %d authorized: want nothing sent", len(routed), len(authorized))
			}
			p.recs.mu.Lock()
			n := len(p.recs.recs)
			p.recs.mu.Unlock()
			if !row.record {
				if n != 0 {
					t.Fatalf("%d exchange records for a path the gateway does not serve", n)
				}
				return
			}
			if x := p.recs.only(t); x.Rule != RefusalRouting || x.RefusedBy != RefusedByPayerGateway || x.Status != http.StatusBadRequest {
				t.Fatalf("record %+v, want refused payer-gateway/routing 400", x)
			}
		})
	}
}
