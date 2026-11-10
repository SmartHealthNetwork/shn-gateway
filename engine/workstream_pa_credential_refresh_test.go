//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

// A payer's own system that answers 401 to the bearer the gateway cached: the
// gateway sends the same request once more with a refreshed bearer, as a direct
// SMART client does. The system receives the request twice, byte for byte the
// same apart from Authorization (the correlation id included), its answer to
// the resend is the answer, and the exchange record counts both calls and says
// why the second was made, with the challenge the 401 carried as observed.
func TestNativeForward_A401RefreshesTheCredentialAndIsRecorded(t *testing.T) {
	for _, row := range []struct {
		name, challenge, want string
	}{
		{"invalid_token", `Bearer error="invalid_token"`, smartauth.ChallengeInvalidToken},
		{"no challenge", "", smartauth.ChallengeNone},
	} {
		t.Run(row.name, func(t *testing.T) {
			baseline, _, _, _ := refreshExchange(t, false, false, row.challenge)
			rec, got, tokens, _ := refreshExchange(t, true, false, row.challenge)
			if len(got) != 2 || tokens != 2 {
				t.Fatalf("the system received %d requests over %d token fetches, want 2 and 2", len(got), tokens)
			}
			if !bytes.Equal(got[0].body, got[1].body) {
				t.Fatalf("the resend's body differs:\n%s\n%s", got[0].body, got[1].body)
			}
			first, second := got[0].header.Clone(), got[1].header.Clone()
			if first.Get(CorrelationHeader) == "" || first.Get(CorrelationHeader) != second.Get(CorrelationHeader) {
				t.Fatalf("correlation ids %q and %q, want one, the same on both", first.Get(CorrelationHeader), second.Get(CorrelationHeader))
			}
			if first.Get("Authorization") != "Bearer AT-1" || second.Get("Authorization") != "Bearer AT-2" {
				t.Fatalf("bearers %q then %q", first.Get("Authorization"), second.Get("Authorization"))
			}
			first.Del("Authorization")
			second.Del("Authorization")
			if fmt.Sprint(first) != fmt.Sprint(second) {
				t.Fatalf("the resend's headers differ beyond Authorization:\n%v\n%v", first, second)
			}
			want := []BackendResend{{Reason: BackendResendCredentialRefreshed, Challenge: row.want}}
			if rec.BackendCalls != baseline.BackendCalls+1 || !slices.Equal(rec.BackendResends, want) {
				t.Fatalf("record: %d backend calls (%d without the 401), resends %+v; want one more call and %+v", rec.BackendCalls, baseline.BackendCalls, rec.BackendResends, want)
			}
			if baseline.BackendResends != nil {
				t.Fatalf("without a 401 the record names resends %+v", baseline.BackendResends)
			}
			if rec.Backend == nil || rec.Backend.Status != http.StatusOK {
				t.Fatalf("backend %+v, want the resend's 200", rec.Backend)
			}
		})
	}
}

type refreshReceived struct {
	header http.Header
	body   []byte
}

// refreshExchange delivers one inquiry to a payer gateway whose native client
// is a SMART client, the payer's system answering 401 to the first bearer when
// refuseFirst (with challenge as its WWW-Authenticate), and returns the
// exchange's record, what the system received and the token fetches made.
func refreshExchange(t *testing.T, refuseFirst, failRefetch bool, challenge string) (ExchangeRecord, []refreshReceived, int32, payerAnswer) {
	t.Helper()
	var tokens int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&tokens, 1)
		if failRefetch && n > 1 {
			http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("AT-%d", n), "token_type": "bearer", "expires_in": 300})
	}))
	defer tokenSrv.Close()
	var mu sync.Mutex
	var got []refreshReceived
	answer := decidedAnswer(t)
	system := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, refreshReceived{r.Header.Clone(), b})
		mu.Unlock()
		if refuseFirst && r.Header.Get("Authorization") == "Bearer AT-1" {
			if challenge != "" {
				w.Header().Set("WWW-Authenticate", challenge)
			}
			http.Error(w, `{"resourceType":"OperationOutcome"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write(answer)
	}))
	defer system.Close()
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	client, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenSrv.URL, ClientID: "payer-gw", Scope: "system/*.read", Alg: "ES384", Key: key, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	p := newLevelPayer(t, EnforcementNone)
	n := NewNativeResponder(client, system.URL, fixedClock,
		WithCRDService("shn-order-select"), WithPASStore(p.store), WithConformancePolicy(NewConformancePolicy(EnforcementNone)))
	n.bindFindingEmitter(p.g.emitFinding)
	p.g.cfg.Responder = n
	var records exchangeRecords
	p.g.cfg.ExchangeObserved = records.observe

	body := inquiryBundle("MBR-COVERED", "", "TRN-1", "72148")
	ans := p.send(t, "pas-claim-inquire", "", body)
	if want := http.StatusOK; !failRefetch && ans.status != want {
		t.Fatalf("answer %d %s, want the system's answer", ans.status, ans.body)
	}
	mu.Lock()
	defer mu.Unlock()
	return records.only(t), slices.Clone(got), atomic.LoadInt32(&tokens), ans
}

// refreshingClient is a SMART client whose token endpoint mints AT-1, AT-2, …
func refreshingClient(t *testing.T) *http.Client {
	t.Helper()
	var tokens int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&tokens, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("AT-%d", n), "token_type": "bearer", "expires_in": 300})
	}))
	t.Cleanup(tokenSrv.Close)
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	client, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenSrv.URL, ClientID: "gw", Scope: "system/*.read", Alg: "ES384", Key: key, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// refusingFirstBearer answers 401 to AT-1 and ok to any other bearer.
func refusingFirstBearer(t *testing.T, ok string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer AT-1" {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, ok)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The CDS service listing read and a read of the system of record are calls to
// the participant's own system too: a resend after a 401 is one more call on
// the record, with its reason.
func TestBackendReads_A401ResendIsRecorded(t *testing.T) {
	want := []BackendResend{{Reason: BackendResendCredentialRefreshed, Challenge: smartauth.ChallengeInvalidToken}}
	t.Run("the CDS service listing", func(t *testing.T) {
		srv := refusingFirstBearer(t, `{"services":[{"id":"s","hook":"order-sign"}]}`)
		x := &exchangeRecorder{}
		ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
		if _, err := DiscoverCDSServices(ctx, refreshingClient(t), srv.URL, nil); err != nil {
			t.Fatal(err)
		}
		if x.rec.BackendCalls != 2 || !slices.Equal(x.rec.BackendResends, want) {
			t.Fatalf("%d calls, resends %+v; want 2 and %+v", x.rec.BackendCalls, x.rec.BackendResends, want)
		}
	})
	t.Run("a read of the system of record", func(t *testing.T) {
		srv := refusingFirstBearer(t, `{"resourceType":"Bundle","type":"searchset","entry":[]}`)
		x := &exchangeRecorder{rec: ExchangeRecord{Direction: DirectionInbound}}
		ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
		readCtx, note := noteRead(ctx)
		if note == nil {
			t.Fatal("the read is not noted")
		}
		req, err := http.NewRequestWithContext(readCtx, http.MethodGet, srv.URL+"/Patient?identifier=x", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := refreshingClient(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		note("")
		if resp.StatusCode != http.StatusOK || x.rec.BackendCalls != 2 || !slices.Equal(x.rec.BackendResends, want) {
			t.Fatalf("answer %d, %d calls, resends %+v; want 200, 2 and %+v", resp.StatusCode, x.rec.BackendCalls, x.rec.BackendResends, want)
		}
	})
}

// When the fresh bearer cannot be obtained, the payer's own 401 is its answer,
// as before: the system received the request once and answered it, so the
// record counts one call with the system's status, names no resend, and the
// forward was sent.
func TestNativeForward_ARefetchThatFailsRelaysTheSystems401(t *testing.T) {
	baseline, _, _, _ := refreshExchange(t, false, false, "")
	rec, got, tokens, ans := refreshExchange(t, true, true, `Bearer error="invalid_token"`)
	if len(got) != 1 || tokens != 2 {
		t.Fatalf("the system received %d requests over %d token fetches, want 1 and 2 (the failed refetch)", len(got), tokens)
	}
	if rec.BackendCalls != baseline.BackendCalls || rec.BackendResends != nil {
		t.Fatalf("record: %d calls (%d without the 401), resends %+v; want the same count and none", rec.BackendCalls, baseline.BackendCalls, rec.BackendResends)
	}
	if rec.Backend == nil || rec.Backend.Status != http.StatusUnauthorized || rec.Backend.ErrorClass != BackendAuth {
		t.Fatalf("backend %+v, want the system's 401 (auth)", rec.Backend)
	}
	if ans.status != http.StatusUnauthorized || string(ans.body) != "{\"resourceType\":\"OperationOutcome\"}\n" {
		t.Fatalf("answer %d %s, want the system's own 401 relayed as it sent it", ans.status, ans.body)
	}
	// The forward was sent: the record names whose message it carried and its
	// edits as the exchange without the 401 does.
	if rec.Ownership != baseline.Ownership || rec.Builder != baseline.Builder || rec.Ownership == "" ||
		!slices.Equal(rec.RequestEdits, baseline.RequestEdits) || !slices.Equal(rec.Edits, baseline.Edits) {
		t.Fatalf("record ownership %q builder %q request edits %q edits %q; want the sent forward's %q %q %q %q",
			rec.Ownership, rec.Builder, rec.RequestEdits, rec.Edits, baseline.Ownership, baseline.Builder, baseline.RequestEdits, baseline.Edits)
	}
}
