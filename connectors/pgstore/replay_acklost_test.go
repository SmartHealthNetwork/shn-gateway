package pgstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// ackLostReplayStore wraps the REAL Postgres one-time-use record so the INSERT truly
// commits and only the acknowledgment is lost: CheckAndRecord runs the statement and
// then, on the first call, returns the error a connection dropped (or a deadline fired)
// after COMMIT produces. Fail closed with replay=true, exactly as this store's own error
// path answers. The engine has the hermetic twin of this wrapper
// (TestIngressToken_AckLostRecordWrite_FreshAssertionIsTheRetry); the row below is the
// one where the record is really in the table, so the sibling replica's refusal is the
// database's answer and not a stub's.
type ackLostReplayStore struct {
	inner engine.ReplayStore
	calls int
}

func (a *ackLostReplayStore) CheckAndRecord(scope, clientID, key string, now, expiresAt time.Time) (bool, error) {
	a.calls++
	replayed, err := a.inner.CheckAndRecord(scope, clientID, key, now, expiresAt)
	if a.calls == 1 {
		return true, errors.New("pgstore: replay: connection reset after the record committed")
	}
	return replayed, err
}

// ackLostSoR is the SystemOfRecord seam the engine requires at construction. This row
// drives /oauth/token only — no route it calls reads the holder's records — so every
// method answers "not found" rather than standing in for a data source.
type ackLostSoR struct{}

func (ackLostSoR) ResolvePatient(string) (string, engine.Demo, bool) { return "", engine.Demo{}, false }
func (ackLostSoR) PatientFHIRRef(string) (string, bool)              { return "", false }
func (ackLostSoR) CoverageInforce(string) (bool, string)             { return false, "" }
func (ackLostSoR) ClinicalContext(string) (shnsdk.ClinicalContext, bool) {
	return shnsdk.ClinicalContext{}, false
}
func (ackLostSoR) SupplementalReport(string) ([]byte, bool)         { return nil, false }
func (ackLostSoR) FacilityRecords(string) (map[string][]byte, bool) { return nil, false }
func (ackLostSoR) OpenOrder(string) ([]byte, bool)                  { return nil, false }
func (ackLostSoR) OpenCoverage(string) ([]byte, bool)               { return nil, false }
func (ackLostSoR) ResolveByReference(string) ([]byte, bool)         { return nil, false }

const ackLostIngressBase = "https://pg-ingress.test"

// ackLostReplica builds ONE ingress-enabled gateway over the shared pool: its own
// in-process caches, the holder's shared signing key and the one-time-use record it is
// handed. Two calls with the same holder id are two replicas of one holder, which is the
// only way the retry below reaches a record it did not write itself.
func ackLostReplica(t *testing.T, pool *pgxpool.Pool, holderID string, clientPub []byte, replay engine.ReplayStore) *httptest.Server {
	t.Helper()
	id, err := shnsdk.GenerateIdentity(holderID)
	if err != nil {
		t.Fatal(err)
	}
	g, err := engine.New(engine.Config{
		Role:           "provider",
		HolderID:       holderID,
		Identity:       id,
		SoR:            ackLostSoR{},
		Store:          engine.NewMemStore(),
		IngressEnabled: true,
		IngressBaseURL: ackLostIngressBase,
		IngressClients: map[string]engine.IngressClientRegistration{
			"br-provider": {Alg: "ES384", PublicKeyPEM: clientPub, Scopes: []string{"system/Davinci.write"}},
		},
		IngressKeys: NewIngressKeyStore(pool, holderID, time.Now),
		Replay:      replay,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// ackLostToken posts a client_assertion to srv's token endpoint and returns the status
// and, on 200, the bearer. A non-200 that carried a token would be the failure this whole
// row exists to rule out, so it is checked here rather than at each call site.
func ackLostToken(t *testing.T, srv *httptest.Server, assertion string) (int, string) {
	t.Helper()
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	resp, err := http.PostForm(srv.URL+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK && strings.Contains(string(body), "access_token") {
		t.Fatalf("status %d carried a token: %s", resp.StatusCode, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("token response %q: %v", body, err)
		}
	}
	return resp.StatusCode, out.AccessToken
}

// ackLostAssertion mints a private_key_jwt client_assertion carrying the given jti, aud
// pinned to the CONFIG base rather than the httptest host.
func ackLostAssertion(t *testing.T, key *ecdsa.PrivateKey, jti string) string {
	t.Helper()
	now := time.Now()
	s, err := jwt.NewWithClaims(jwt.SigningMethodES384, jwt.MapClaims{
		"iss": "br-provider", "sub": "br-provider", "aud": ackLostIngressBase + "/oauth/token",
		"jti": jti, "iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(),
	}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ackLostClientKey mints the ingress client's ES384 key and its PKIX public PEM.
func ackLostClientKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// TestReplayPg_AckLostRecordWrite_FreshAssertionIsTheRetry: the retry contract when the
// record's INSERT commits at replica A and the acknowledgment is lost. A answers 503 and
// no token. The same assertion presented at replica B — a process that never saw the
// write — is refused 401 by the row A left in gw_replay, which is the contract: a 503
// issues no token, and the retry carries a FRESH assertion. That fresh assertion issues
// at B and the bearer verifies, so the client is never stuck.
//
// An unavailable or closed pool does NOT exercise this: those never reach the table, so
// they can only prove the case where nothing was spent.
func TestReplayPg_AckLostRecordWrite_FreshAssertionIsTheRetry(t *testing.T) {
	pool := testPool(t)
	holderID := "h-acklost"
	key, pub := ackLostClientKey(t)

	// Replica A's record is the real store behind the ack-lost wrapper; B's is the same
	// table, unwrapped — B is a healthy replica reading what A wrote.
	wrapped := &ackLostReplayStore{inner: NewReplayStore(pool, holderID, time.Now)}
	a := ackLostReplica(t, pool, holderID, pub, wrapped)
	b := ackLostReplica(t, pool, holderID, pub, NewReplayStore(pool, holderID, time.Now))

	spent := ackLostAssertion(t, key, "pg-acklost-1")
	if code, bearer := ackLostToken(t, a, spent); code != http.StatusServiceUnavailable || bearer != "" {
		t.Fatalf("ack-lost record write at A = %d (bearer %q), want 503 and no token", code, bearer)
	}
	// The record really is in the table — the ambiguity is only in what A could observe.
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM gw_replay WHERE holder_id=$1 AND scope=$2 AND client_id=$3 AND key=$4`,
		holderID, engine.ReplayScopeIngressJTI, "br-provider", "pg-acklost-1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("gw_replay rows for the 503'd jti = %d, want 1 — the wrapper must model a COMMITTED write, not an outage", n)
	}

	// The same assertion at a sibling replica is refused as replayed, and that refusal is
	// correct: the jti was recorded. A client that retries the same assertion after a 503
	// has to be able to see this 401 in the docs.
	if code, _ := ackLostToken(t, b, spent); code != http.StatusUnauthorized {
		t.Fatalf("the same assertion at B = %d, want 401 — B did not see the record A committed", code)
	}

	// The retry the contract asks for: a FRESH assertion, at either replica.
	code, bearer := ackLostToken(t, b, ackLostAssertion(t, key, "pg-acklost-2"))
	if code != http.StatusOK || bearer == "" {
		t.Fatalf("fresh assertion at B = %d (bearer %q), want 200 — the documented retry must work", code, bearer)
	}
	// The bearer B issued verifies at A: one holder, one shared signing key.
	req, err := http.NewRequest(http.MethodGet, a.URL+"/cds-services", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B's bearer at A = %d, want 200", resp.StatusCode)
	}
}
