package app

// pg_multi_test.go — the TWO-INSTANCE proof for the shared state seams.
//
// Every row here builds the SAME holder TWICE (two build() calls, two independent
// handlers with their own in-memory caches) over ONE store DSN, then drives real HTTP
// against one instance and observes the other. That is the property the in-process
// defaults cannot satisfy: a bearer minted at A verifies at B, a client_assertion spent
// at A is refused at B, and an exchange begun and appended at A is read back by a FRESH
// store instance over the same pool (the process-restart proof).
//
// Pg-gated: every row skips without SHN_TEST_DATABASE_URL. Row isolation is by holder,
// not by DROP: each row mints a random holder id and every gw_* row this package writes
// is holder-scoped, so a leftover row from another holder (or another package) can never
// be counted here. The schema itself is ensured by build() → pgstore.NewPgStore. These
// rows must NOT run concurrently with the pgstore package's own tests, which DROP the
// gw_* tables — the CI pg job runs the two commands in sequence for exactly that reason.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// pgMultiMember is the member the fake SoR below resolves, and the subject of the PAS
// bundle the exchange row submits.
const pgMultiMember = "MBR-multi"

// pgMultiPayerSystem/Value is the Coverage.payor identity the PAYER_DIRECTORY maps to a
// payer holder that is NOT in the peer registry. Line selection does NOT refuse for it
// (an undeclared peer takes selectContractToken's own-highest arm); the refusal is
// roundTripInner's `recipient %q not in registry`, which fires BEFORE any authz/Hub
// call — so the leg is recorded with outcome "error" and no network is touched. Do NOT
// "fix" this by registering the payer: that would turn these rows into a Hub round trip.
// The exchange record is what this file is about; the leg's outcome is not.
const (
	pgMultiPayerSystem = "urn:shn:payer"
	pgMultiPayerValue  = "PAYER-MULTI"
)

// randHex returns n random bytes hex-encoded (the gateway module has no uuid dependency;
// do not add one for a test).
func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// writeTempJSON writes body to a fresh temp file and returns its path.
func writeTempJSON(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// pgMultiSoR is the holder's own FHIR system of record: it answers the ONE search
// fhirsor.ResolvePatient issues (Patient?identifier=<member system>|<member>) with a
// US Core-shaped Patient, so the ingress subject-bind resolves a pci. Any other path
// answers an empty searchset.
func pgMultiSoR(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		if strings.HasSuffix(r.URL.Path, "/Patient") &&
			r.URL.Query().Get("identifier") == shnsdk.MemberSystem+"|"+pgMultiMember {
			fmt.Fprintf(w, `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":`+
				`{"resourceType":"Patient","id":%q,"birthDate":"1970-01-01","name":[{"family":"Multi","given":["Rep"]}]}}]}`,
				pgMultiMember)
			return
		}
		_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// pgMultiEnv builds one provider gateway env over the shared DSN with the DaVinci
// ingress enabled for client "br-provider" (whose key the caller holds). Two build()
// calls over the same dir + dsn are two replicas of the same holder.
func pgMultiEnv(t *testing.T, dir, dsn string, key *ecdsa.PrivateKey) map[string]string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	clients, err := json.Marshal([]map[string]any{{
		"client_id": "br-provider", "alg": "ES384",
		"public_key_pem": string(pemBytes), "scopes": []string{"system/Davinci.write"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	t.Cleanup(keys.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)
	payerDir := fmt.Sprintf(`[{"system":%q,"value":%q,"holderId":"h-multi-payer"}]`, pgMultiPayerSystem, pgMultiPayerValue)
	return map[string]string{
		"ROLE": "provider", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL, "SHN_FAKE_VALIDATOR": "1",
		"FHIR_DATA_URL":                     pgMultiSoR(t),
		"PROVIDER_DTR_POPULATE_URL":         "https://populate.test/fhir/Questionnaire/$populate",
		"PROVIDER_DAVINCI_INGRESS":          "1",
		"PROVIDER_DAVINCI_INGRESS_BASE_URL": "http://ingress.test",
		"INGRESS_CLIENTS_FILE":              writeClientsFile(t, string(clients)),
		"PAYER_DIRECTORY":                   writeTempJSON(t, "payers.json", payerDir),
		"SHN_STORE_DATABASE_URL":            dsn,
	}
}

// pgMultiSetup skips without the DSN, mints a fresh holder bundle + ingress client key,
// and returns the env both replicas are built from.
func pgMultiSetup(t *testing.T) (env map[string]string, key *ecdsa.PrivateKey, dsn, holderID string) {
	t.Helper()
	dsn = os.Getenv("SHN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SHN_TEST_DATABASE_URL to run Postgres integration tests")
	}
	holderID = "h-multi-" + randHex(t, 4)
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity(holderID)
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "provider", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pgMultiEnv(t, dir, dsn, key), key, dsn, holderID
}

// pgMultiPair returns TWO independently built instances of one holder over one DSN,
// plus a pool for the assertions and the holder id every gw_* row is scoped by.
func pgMultiPair(t *testing.T) (a, b *httptest.Server, key *ecdsa.PrivateKey, pool *pgxpool.Pool, holderID string) {
	t.Helper()
	env, key, dsn, holderID := pgMultiSetup(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	getenv := func(k string) string { return env[k] }
	ba, err := build(context.Background(), getenv, io.Discard, nil)
	if err != nil {
		t.Fatalf("build A: %v", err)
	}
	bb, err := build(context.Background(), getenv, io.Discard, nil)
	if err != nil {
		t.Fatalf("build B: %v", err)
	}
	a, b = httptest.NewServer(ba.handler), httptest.NewServer(bb.handler)
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	return a, b, key, pool, holderID
}

// assertion mints a private_key_jwt client assertion for br-provider with a fresh jti,
// aud pinned to the CONFIG base (http://ingress.test), never the httptest host.
func assertion(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES384, jwt.MapClaims{
		"iss": "br-provider", "sub": "br-provider", "aud": "http://ingress.test/oauth/token",
		"jti": randHex(t, 16), "iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(),
	})
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ingressToken exchanges a client assertion for a bearer at srv's /oauth/token.
func ingressToken(t *testing.T, srv *httptest.Server, clientAssertion string) (int, string) {
	t.Helper()
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {clientAssertion},
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

// postIngress POSTs body to path on srv with the bearer, and returns the status.
func postIngress(t *testing.T, srv *httptest.Server, bearer, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// pgMultiPASBundle is the minimum conformant-shaped PAS submit that reaches the
// ingress's exchange seam: one Claim, one order and one Coverage, all bound to the same
// member, with an inline payor identity the PAYER_DIRECTORY resolves. It reaches the
// exchange seam (ingress.go's Begin precedes leg selection on this route), origination
// then fails closed at the registry lookup, and the handler records exactly one leg and
// answers 502.
func pgMultiPASBundle() string {
	return fmt.Sprintf(`{"resourceType":"Bundle","type":"collection","entry":[
{"resource":{"resourceType":"Claim","id":"c1","patient":{"reference":"Patient/%[1]s"}}},
{"resource":{"resourceType":"ServiceRequest","id":"sr1","subject":{"reference":"Patient/%[1]s"}}},
{"resource":{"resourceType":"Coverage","id":"cov1","beneficiary":{"reference":"Patient/%[1]s"},
 "payor":[{"identifier":{"system":%[2]q,"value":%[3]q}}]}}
]}`, pgMultiMember, pgMultiPayerSystem, pgMultiPayerValue)
}

// TestPgMulti_TokenFromAAcceptedAtB: the ingress bearer signing key is SHARED. B has
// never seen the kid A signed with, so it can only accept the bearer by resolving that
// kid out of the store.
func TestPgMulti_TokenFromAAcceptedAtB(t *testing.T) {
	a, b, key, _, _ := pgMultiPair(t)
	code, bearer := ingressToken(t, a, assertion(t, key))
	if code != http.StatusOK || bearer == "" {
		t.Fatalf("token at A: %d %q", code, bearer)
	}
	// EXACT status, not "anything but 401": 502 is the far end of B's PAS ingress —
	// auth, subject-bind, payer routing, exchange Begin and origination all ran on a
	// bearer B never issued. A 401/403/400 would each name a different stage failing,
	// and "not 401" would pass for all of them.
	if got := postIngress(t, b, bearer, "/Claim/$submit", pgMultiPASBundle()); got != http.StatusBadGateway {
		t.Fatalf("PAS submit at B on A's bearer = %d, want 502 — B did not accept a bearer A issued, "+
			"or did not run the request to origination (the signing key is not shared)", got)
	}
	// The same route with no bearer is 401: the gate is on, so the 502 above is
	// acceptance, not an open door.
	if got := postIngress(t, b, "", "/Claim/$submit", pgMultiPASBundle()); got != http.StatusUnauthorized {
		t.Fatalf("no-bearer PAS submit at B = %d, want 401", got)
	}
	// A second route on the same bearer, pinned exactly: 400 is the CRD handler's own
	// "missing context.patientId" — past the bearer gate, refused on content.
	if got := postIngress(t, b, bearer, "/cds-services/order-select-crd", "{}"); got != http.StatusBadRequest {
		t.Fatalf("CRD at B on A's bearer = %d, want 400 (past the bearer gate, refused on content)", got)
	}
}

// TestPgMulti_AssertionReplayedAtBIs401: the one-time-use record on the client_assertion
// jti is SHARED — spending it at A burns it at B too.
func TestPgMulti_AssertionReplayedAtBIs401(t *testing.T) {
	a, b, key, _, _ := pgMultiPair(t)
	as := assertion(t, key)
	if code, _ := ingressToken(t, a, as); code != http.StatusOK {
		t.Fatalf("first use at A: %d", code)
	}
	if code, _ := ingressToken(t, b, as); code != http.StatusUnauthorized {
		t.Fatalf("replay at B = %d, want 401 (jti record not shared)", code)
	}
	// Control: a FRESH assertion still mints at B, so the 401 above is the replay
	// record, not a broken instance.
	if code, bearer := ingressToken(t, b, assertion(t, key)); code != http.StatusOK || bearer == "" {
		t.Fatalf("fresh assertion at B = %d %q, want 200 + a bearer", code, bearer)
	}
}

// pgMultiExchangeIDs lists this holder's exchange ids. The ingress never hands the id
// back to the caller, so the assertions below read it out of the table.
func pgMultiExchangeIDs(t *testing.T, pool *pgxpool.Pool, holderID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT exchange_id FROM gw_exchange WHERE holder_id=$1`, holderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestPgMulti_ExchangeRowsVisibleAcrossInstances: an exchange begun and appended by
// instance A is read back — with its legs — by a FRESH ExchangeStore over the same pool.
// That fresh store shares no memory with A: it is the process-restart proof.
func TestPgMulti_ExchangeRowsVisibleAcrossInstances(t *testing.T) {
	a, _, key, pool, holderID := pgMultiPair(t)
	code, bearer := ingressToken(t, a, assertion(t, key))
	if code != http.StatusOK || bearer == "" {
		t.Fatalf("token at A: %d %q", code, bearer)
	}
	// 502: origination stops at roundTripInner's registry lookup, so the leg is recorded
	// with outcome "error" and nothing goes on the wire. The exchange RECORD is the
	// subject here, not the outcome.
	if got := postIngress(t, a, bearer, "/Claim/$submit", pgMultiPASBundle()); got != http.StatusBadGateway {
		t.Fatalf("PAS submit at A = %d, want 502 (origination fails closed, leg still recorded)", got)
	}

	ctx := context.Background()
	// Read the exchange back through a store the running instances know nothing about.
	ids := pgMultiExchangeIDs(t, pool, holderID)
	if len(ids) != 1 {
		t.Fatalf("exchanges for %s = %d, want 1 (exchange seam not durable)", holderID, len(ids))
	}

	fresh, err := pgxpool.New(ctx, os.Getenv("SHN_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	store := pgstore.NewExchangeStore(fresh, holderID, 0, nil)
	ex, ok := store.Get(ids[0])
	if !ok {
		t.Fatalf("fresh instance could not read exchange %s", ids[0])
	}
	if len(ex.Legs) != 1 {
		t.Fatalf("legs read by the fresh instance = %d, want 1", len(ex.Legs))
	}
	if ex.Legs[0].Type != "pas-claim" {
		t.Fatalf("leg type = %q, want pas-claim", ex.Legs[0].Type)
	}
	// Secondary: the same count straight off the table, so a store-side filter cannot
	// hide a missing row.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gw_exchange_leg WHERE holder_id=$1`, holderID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("leg rows for %s = %d, want 1", holderID, n)
	}
}

// TestPgMulti_BuildUnderDSNStartsKeyRefresh: the shared-key background reload is carried
// on the built value under the DSN (and only under it), and it returns on a cancelled
// context — Run starts it, so a keyRefresh that never returned would outlive shutdown.
func TestPgMulti_BuildUnderDSNStartsKeyRefresh(t *testing.T) {
	env, key, _, _ := pgMultiSetup(t)
	b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if b.keyRefresh == nil {
		t.Fatal("keyRefresh is nil under SHN_STORE_DATABASE_URL — the shared key store is not being refreshed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.keyRefresh(ctx)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second): // liveness fence only — the return is immediate
		t.Fatal("keyRefresh did not return on a cancelled context")
	}

	// The ingress key is stored only where the ingress runs: same DSN, ingress off ⇒
	// no key store, so nothing to refresh (and no signing key persisted or rotated for
	// a gateway that mounts no token endpoint). The replay and exchange stores are NOT
	// gated this way — they back guards every role runs.
	noIngress := map[string]string{}
	for k, v := range env {
		noIngress[k] = v
	}
	delete(noIngress, "PROVIDER_DAVINCI_INGRESS")
	ib, err := build(context.Background(), func(k string) string { return noIngress[k] }, io.Discard, nil)
	if err != nil {
		t.Fatalf("build with the ingress disabled: %v", err)
	}
	if ib.keyRefresh != nil {
		t.Fatal("keyRefresh is non-nil with the ingress disabled — a signing key is being kept for routes that are not mounted")
	}
	// The ingress really is off in that build, so the nil above is the gate and not a
	// broken fixture: the token endpoint is not mounted.
	isrv := httptest.NewServer(ib.handler)
	t.Cleanup(isrv.Close)
	if code, _ := ingressToken(t, isrv, assertion(t, key)); code != http.StatusNotFound {
		t.Fatalf("/oauth/token with the ingress disabled = %d, want 404", code)
	}

	// The other direction: with no DSN there is no shared store, so nothing to refresh.
	noDSN := map[string]string{}
	for k, v := range env {
		noDSN[k] = v
	}
	delete(noDSN, "SHN_STORE_DATABASE_URL")
	nb, err := build(context.Background(), func(k string) string { return noDSN[k] }, io.Discard, nil)
	if err != nil {
		t.Fatalf("build without the store DSN: %v", err)
	}
	if nb.keyRefresh != nil {
		t.Fatal("keyRefresh is non-nil without SHN_STORE_DATABASE_URL")
	}
}

// TestPgMulti_StoreOutageTokenIs503: the outage pairing on the REAL wiring. Under one
// DSN the replay record and the ingress signing key share one pool, so an unreachable
// database fails whichever of them the endpoint consults first — the signing key, resolved
// before the one-time-use record is written. The endpoint must answer the retryable 503 the deployment docs
// promise, never the 401 that reads as a bad credential; and a bearer already issued
// must keep verifying, since its key is in this replica's cache and needs no database.
func TestPgMulti_StoreOutageTokenIs503(t *testing.T) {
	env, key, _, _ := pgMultiSetup(t)
	b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if b.closeStore == nil {
		t.Fatal("closeStore is nil under SHN_STORE_DATABASE_URL — no way to make the store unreachable")
	}
	srv := httptest.NewServer(b.handler)
	t.Cleanup(srv.Close)

	// Control: a healthy database issues, so the 503 below is the outage.
	code, bearer := ingressToken(t, srv, assertion(t, key))
	if code != http.StatusOK || bearer == "" {
		t.Fatalf("token on a healthy store: %d %q", code, bearer)
	}

	b.closeStore() // the database is now unreachable to this gateway

	if code, _ := ingressToken(t, srv, assertion(t, key)); code != http.StatusServiceUnavailable {
		t.Fatalf("token with the store unreachable = %d, want 503 — a database outage must not answer 401 (a replayed/bad credential)", code)
	}
	// The bearer minted before the outage still verifies: 400 is the CRD handler's own
	// "missing context.patientId", i.e. past the bearer gate and refused on content.
	if got := postIngress(t, srv, bearer, "/cds-services/order-select-crd", "{}"); got != http.StatusBadRequest {
		t.Fatalf("CRD on the pre-outage bearer = %d, want 400 (the issued bearer must keep verifying from the replica cache)", got)
	}
	// …and the gate is still on: no bearer is still 401, so the 400 above is acceptance.
	if got := postIngress(t, srv, "", "/cds-services/order-select-crd", "{}"); got != http.StatusUnauthorized {
		t.Fatalf("no-bearer CRD during the outage = %d, want 401", got)
	}
}

// TestPgMulti_ResetOverAnUnreachableStoreIs503: the demo reset's durable half on the REAL
// wiring. POST /scenario/reset clears the holder's gw_exchange rows; with the database
// unreachable that DELETE cannot run, and answering 200 would tell the operator the
// records are gone while they are all still there — the next run then starts on stale
// state with nothing said. The route must answer the same retryable 503 the rest of the
// shared-state seams do, and name the store rather than the database's own error text.
func TestPgMulti_ResetOverAnUnreachableStoreIs503(t *testing.T) {
	env, _, _, _ := pgMultiSetup(t)
	b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if b.closeStore == nil {
		t.Fatal("closeStore is nil under SHN_STORE_DATABASE_URL — no way to make the store unreachable")
	}
	srv := httptest.NewServer(b.handler)
	t.Cleanup(srv.Close)

	// Control: on a healthy database the reset succeeds, so the 503 below is the outage.
	if code, body := postPlain(t, srv, "/scenario/reset"); code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("reset on a healthy store = %d %s, want 200 {\"ok\":true}", code, body)
	}

	b.closeStore() // the database is now unreachable to this gateway

	code, body := postPlain(t, srv, "/scenario/reset")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("reset with the store unreachable = %d, want 503 — a reset that cleared nothing must not report success", code)
	}
	if !strings.Contains(body, `"exchange store reset failed"`) {
		t.Fatalf("503 body = %s, want the store-named cause", body)
	}
	if strings.Contains(body, "closed pool") || strings.Contains(body, "conn") {
		t.Fatalf("503 body leaks the database's own error text: %s", body)
	}
}

// postPlain POSTs an empty body to path on srv and returns the status and body.
func postPlain(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// TestPgMulti_StoreOutageCountsEveryRefusingPath: an outage is only actionable if it is
// counted AND it is answered as an outage. Both shared-state seams a request touches are
// driven here through the real wiring under one DSN — the one-time-use record on the
// token endpoint, and the ingress key store behind a bearer whose kid this replica has
// never cached — and each must answer a retryable 503 and count itself exactly once in
// the EMF the gateway wrote to its own stdout. Neither may answer 401: a database outage
// is not a bad credential, and the partner client is not expected to retry one.
func TestPgMulti_StoreOutageCountsEveryRefusingPath(t *testing.T) {
	env, key, _, _ := pgMultiSetup(t)
	env["METRICS_SERVICE"] = "pg-multi-gw"
	var out bytes.Buffer
	b, err := build(context.Background(), func(k string) string { return env[k] }, &out, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	srv := httptest.NewServer(b.handler)
	t.Cleanup(srv.Close)

	// Control: a healthy store issues and counts nothing.
	if code, bearer := ingressToken(t, srv, assertion(t, key)); code != http.StatusOK || bearer == "" {
		t.Fatalf("token on a healthy store: %d %q", code, bearer)
	}
	if got := storeErrorDims(t, &out); len(got) != 0 {
		t.Fatalf("a healthy store counted %v", got)
	}

	b.closeStore() // the database is now unreachable to this gateway
	out.Reset()

	if code, _ := ingressToken(t, srv, assertion(t, key)); code != http.StatusServiceUnavailable {
		t.Fatalf("token with the store unreachable = %d, want 503", code)
	}
	if got := storeErrorDims(t, &out); len(got) != 1 || got[0] != "replay" {
		t.Fatalf("StoreError dims after a refused token = %v, want [replay]", got)
	}

	// A bearer whose kid a replica has never cached: resolving it needs the database, so
	// the refusal the caller gets is really an outage — the key store says it could not
	// tell, and the route answers 503 and counts it. A SECOND instance carries the row:
	// this one's key store reloaded while issuing the control token above, and the miss
	// throttle (one reload per second per replica) would swallow the query — a fresh
	// replica has nothing stamped.
	var out2 bytes.Buffer
	b2, err := build(context.Background(), func(k string) string { return env[k] }, &out2, nil)
	if err != nil {
		t.Fatalf("build second instance: %v", err)
	}
	srv2 := httptest.NewServer(b2.handler)
	t.Cleanup(srv2.Close)
	b2.closeStore()
	out2.Reset()

	unknownKid := randHex(t, 16)
	stranger, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES384, jwt.MapClaims{
		"client_id": "br-provider", "aud": "http://ingress.test",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(),
	})
	tok.Header["kid"] = unknownKid
	signed, err := tok.SignedString(stranger)
	if err != nil {
		t.Fatal(err)
	}
	if got := postIngress(t, srv2, signed, "/cds-services/order-select-crd", "{}"); got != http.StatusServiceUnavailable {
		t.Fatalf("unknown-kid bearer during the outage = %d, want 503 — an unreadable key store is an outage, not a bad credential", got)
	}
	if got := storeErrorDims(t, &out2); len(got) != 1 || got[0] != "ingresskey" {
		t.Fatalf("StoreError dims after an unresolvable kid = %v, want [ingresskey] — the key store's reload error is not being counted", got)
	}
}

// storeErrorDims returns the `store` dimension of every StoreError EMF line the gateway
// has written so far (other metric lines are ignored).
func storeErrorDims(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var dims []string
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" || !strings.Contains(ln, `"StoreError"`) {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			continue // not an EMF line (a plain log line on the same stream)
		}
		if m["StoreError"] == nil {
			continue
		}
		s, _ := m["store"].(string)
		dims = append(dims, s)
	}
	return dims
}

// syncBuffer is a bytes.Buffer with a lock: the pool sampler emits from Run's goroutine
// while the row reads what it wrote.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestPgMulti_PoolStatsCarriedOnlyWithMetricsOptIn: the pool sampler is carried on the
// built value under BOTH a store DSN and METRICS_SERVICE, never started by build (the
// boot gate drives build and must not inherit a ticker), and it emits the pool's stats
// against the REAL pool the gateway serves from.
func TestPgMulti_PoolStatsCarriedOnlyWithMetricsOptIn(t *testing.T) {
	env, _, _, _ := pgMultiSetup(t)
	// No METRICS_SERVICE: nothing to emit to, so nothing is carried.
	off, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if off.poolStats != nil {
		t.Fatal("poolStats is non-nil without METRICS_SERVICE — a gateway with no emitter is sampling its pool")
	}

	env["METRICS_SERVICE"] = "pg-multi-gw"
	// The sampler writes from its own goroutine while this row polls, and bytes.Buffer is
	// not safe for that — a plain buffer here is a data race, not a flake to live with.
	var out syncBuffer
	on, err := build(context.Background(), func(k string) string { return env[k] }, &out, nil)
	if err != nil {
		t.Fatalf("build with metrics: %v", err)
	}
	if on.poolStats == nil {
		t.Fatal("poolStats is nil under SHN_STORE_DATABASE_URL + METRICS_SERVICE — the shared pool is not being sampled")
	}
	if strings.Contains(out.String(), `"StorePool"`) {
		t.Fatal("build emitted a pool sample: the sampler must not run until Run starts it")
	}
	// Run's goroutine, driven directly: one sample, then a return on the run context.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { on.poolStats(ctx); close(done) }()
	deadline := time.Now().Add(30 * time.Second) // liveness fence only
	for !strings.Contains(out.String(), `"StorePool"`) && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the pool sampler did not return on a cancelled context")
	}
	var stats []string
	for _, ln := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.Contains(ln, `"StorePool"`) {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			continue
		}
		if s, _ := m["stat"].(string); s != "" {
			stats = append(stats, s)
		}
		if m["Service"] != "pg-multi-gw" {
			t.Fatalf("StorePool dims wrong: %v", m)
		}
	}
	if len(stats) < 4 {
		t.Fatalf("StorePool stats emitted = %v, want one per pool stat", stats)
	}
}
