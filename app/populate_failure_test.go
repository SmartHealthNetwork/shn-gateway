package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type populateOutput struct {
	mu sync.Mutex
	bytes.Buffer
}

func (o *populateOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.Buffer.Write(p)
}
func (o *populateOutput) text() string { o.mu.Lock(); defer o.mu.Unlock(); return o.Buffer.String() }

// The configured app and its FHIR/SMART connectors are real; loopback peers supply
// sealed CRD/DTR fixtures through the Hub. Live payer/CQL validation is a separate gate.
func TestBuildPopulateFailureEvidence(t *testing.T) {
	for _, row := range []struct {
		name                 string
		tokenStatus, status  int
		body, record, public string
		calls                int32
	}{
		{"token refusal", 401, 200, "", `gateway: populate_failure {"version":1,"stage":"token_acquisition","reason":"other","status":0}`, `{"error":"engine: $populate upstream failed"}` + "\n", 0},
		{"population status", 200, 503, "CANARY-POPULATION-BODY", `gateway: populate_failure {"version":1,"stage":"http_status","reason":"non_2xx","status":503}`, `{"error":"engine: $populate upstream failed"}` + "\n", 1},
		{"foreign subject", 200, 200, `{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/CANARY-FOREIGN"},"questionnaire":"https://example.test/CANARY-CANONICAL"}`, "", `{"error":"engine: populated QR subject does not match patient"}` + "\n", 1},
		{"foreign canonical", 200, 200, `{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/CANARY-STORE"},"questionnaire":"https://example.test/CANARY-FOREIGN"}`, "", `{"error":"populated QR questionnaire does not match canonical"}` + "\n", 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			var calls, tokenCalls atomic.Int32
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenCalls.Add(1)
				w.WriteHeader(row.tokenStatus)
				if row.tokenStatus == 200 {
					_, _ = io.WriteString(w, `{"access_token":"CANARY-BEARER","expires_in":3600}`)
				} else {
					_, _ = io.WriteString(w, "CANARY-TOKEN-BODY")
				}
			}))
			defer token.Close()
			pop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer CANARY-BEARER" {
					t.Error("missing configured bearer")
				}
				w.Header().Set("CANARY-HEADER", "CANARY-VALUE")
				w.WriteHeader(row.status)
				_, _ = io.WriteString(w, row.body)
			}))
			defer pop.Close()
			out := &populateOutput{}
			handler := populateFailureApp(t, map[string]string{"PROVIDER_DTR_POPULATE_URL": pop.URL + "/CANARY-URL", "PROVIDER_DTR_POPULATE_TOKEN_URL": token.URL + "/CANARY-TOKEN-URL", "PROVIDER_DTR_POPULATE_CLIENT_ID": "CANARY-CLIENT", "PROVIDER_DTR_POPULATE_CLIENT_SECRET": "CANARY-SECRET"}, out)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/scenario/uc08", strings.NewReader(`{}`)))
			if rr.Code != 502 || rr.Body.String() != row.public {
				t.Fatalf("public response=%d %q, want 502 %q", rr.Code, rr.Body.String(), row.public)
			}
			var records []string
			for _, line := range strings.Split(out.text(), "\n") {
				if strings.HasPrefix(line, "gateway: populate_failure ") {
					records = append(records, line)
				}
			}
			if row.record == "" {
				if len(records) != 0 {
					t.Fatalf("guard produced upstream record: %v", records)
				}
			} else if len(records) != 1 || records[0] != row.record {
				t.Fatalf("records=%q, want %q; stdout=%q", records, row.record, out.text())
			}
			if strings.Contains(out.text(), "CANARY") {
				t.Fatalf("stdout leaked sensitive fixture: %s", out.text())
			}
			if calls.Load() != row.calls || tokenCalls.Load() != 1 {
				t.Fatalf("population=%d token=%d", calls.Load(), tokenCalls.Load())
			}
		})
	}
}

func populateFailureApp(t *testing.T, populate map[string]string, out io.Writer) http.Handler {
	t.Helper()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	provider, err := shnsdk.GenerateIdentity("provider")
	if err != nil {
		t.Fatal(err)
	}
	payer, err := shnsdk.GenerateIdentity("payer")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(tok shnsdk.Token) shnsdk.Token {
		b, e := json.Marshal(tok)
		if e != nil {
			t.Error(e)
		}
		tok.Signature = ed25519.Sign(priv, b)
		return tok
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"pubkey": base64.StdEncoding.EncodeToString(pub)})
	}))
	t.Cleanup(keys.Close)
	fhir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var resource string
		switch r.URL.Path {
		case "/metadata":
			_, _ = io.WriteString(w, `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`)
			return
		case "/Patient":
			resource = `{"resourceType":"Patient","id":"CANARY-STORE","birthDate":"1980-01-01","name":[{"family":"CANARY-FAMILY"}]}`
		case "/Coverage":
			resource = `{"resourceType":"Coverage","id":"CANARY-COVERAGE","status":"active","beneficiary":{"reference":"Patient/CANARY-STORE"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}`
		default:
			t.Errorf("unexpected FHIR path: %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		_, _ = fmt.Fprintf(w, `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":%s}]}`, resource)
	}))
	t.Cleanup(fhir.Close)
	authz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req shnsdk.AuthorizeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if req.Operation != "crd-order-select" && req.Operation != "dtr-questionnaire-fetch" {
			t.Errorf("unexpected authority operation: %s", req.Operation)
		}
		tok := sign(shnsdk.Token{Operation: req.Operation, Scope: "crd-context", Subject: req.SubjectPCI, Frame: req.Frame, Holder: "provider", CorrelationID: req.CorrelationID, PayloadHash: req.PayloadHash, Expiry: now.Add(time.Hour)})
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok})
	}))
	t.Cleanup(authz.Close)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		env, e := shnsdk.DecodeEnvelope(body)
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		var requestToken shnsdk.Token
		if e = json.Unmarshal([]byte(env.Metadata.AuthzToken), &requestToken); e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		digest := sha256.Sum256(env.Ciphertext)
		if e = shnsdk.VerifyBound(requestToken, pub, now, env.Metadata.AuthorityFrame, env.Metadata.TransactionType, env.Metadata.CorrelationID, "provider", shnsdk.ResolvePCI("MBR-D-UC08", "1980-01-01", "CANARY-FAMILY"), hex.EncodeToString(digest[:])); e != nil {
			t.Error(e)
			w.WriteHeader(403)
			return
		}
		if _, e = shnsdk.Open(env, payer.EncPub, payer.EncPriv); e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return
		}
		var payload []byte
		var op string
		switch env.Metadata.TransactionType {
		case "crd-order-select":
			payload, e = shnsdk.BuildCards(shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"https://example.test/CANARY-CANONICAL"}})
			op = "crd-cards"
		case "dtr-questionnaire-fetch":
			payload = []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","status":"active","url":"https://example.test/CANARY-CANONICAL"}}]}`)
			op = "dtr-questionnaire"
		default:
			t.Errorf("unexpected payer leg: %s", env.Metadata.TransactionType)
			w.WriteHeader(500)
			return
		}
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		response, e := shnsdk.Seal(shnsdk.Metadata{Sender: "payer", Recipient: "provider", TransactionType: env.Metadata.TransactionType, AuthorityFrame: "payer-coverage", Timestamp: now.Format(time.RFC3339), CorrelationID: env.Metadata.CorrelationID}, payload, provider.EncPub)
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		digest = sha256.Sum256(response.Ciphertext)
		tok := sign(shnsdk.Token{Operation: op, Scope: "crd-context", Subject: requestToken.Subject, Frame: "payer-coverage", Holder: "payer", CorrelationID: env.Metadata.CorrelationID, PayloadHash: hex.EncodeToString(digest[:]), Expiry: now.Add(time.Hour)})
		tokJSON, e := json.Marshal(tok)
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		response.Metadata.AuthzToken = string(tokJSON)
		b, e := shnsdk.EncodeEnvelope(response)
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(peer.Close)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, e := peer.Client().Post(peer.URL, "application/json", r.Body)
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(hub.Close)
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"endpoints": map[string]string{"authz": authz.URL, "hub": hub.URL}, "authzPublicKeyURL": keys.URL, "hubTransportKeyURL": keys.URL})
	}))
	t.Cleanup(disc.Close)
	dir := t.TempDir()
	if e := shnsdk.WriteBundle(dir, provider, "provider", "https://holder.example"); e != nil {
		t.Fatal(e)
	}
	env := map[string]string{"ROLE": "provider", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL, "SHN_FAKE_VALIDATOR": "1", "FHIR_DATA_URL": fhir.URL}
	for k, v := range populate {
		env[k] = v
	}
	b, e := build(context.Background(), func(k string) string { return env[k] }, out, func() time.Time { return now })
	if e != nil {
		t.Fatal(e)
	}
	b.reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: payer.EncPub, SignPub: payer.SignPub, BaseURL: peer.URL, PayerIDs: []shnsdk.PayerIdentifier{shnsdk.CMSPayerIdentity}})
	return b.handler
}

func TestPopulateFailureEvidenceRejectsUnknownFields(t *testing.T) {
	for _, n := range []engine.PopulateFailure{
		{Stage: "CANARY-STAGE", Reason: "other"}, {Stage: "transport", Reason: "CANARY-REASON"}, {Stage: "transport", Reason: "non_2xx"}, {Stage: "qr_extract", Reason: "other", Status: 200}, {Stage: "http_status", Reason: "invalid_json", Status: 503}, {Stage: "http_status", Reason: "non_2xx", Status: 99}, {Stage: "http_status", Reason: "non_2xx", Status: 600}, {Stage: "http_status", Reason: "non_2xx", Status: -1},
	} {
		var out bytes.Buffer
		populateFailureObserver(&out)(n)
		if out.Len() != 0 {
			t.Fatalf("untrusted note logged: %q", out.String())
		}
	}
}

func TestPopulateFailureEvidenceSchemaAndConcurrentOutput(t *testing.T) {
	var out bytes.Buffer
	observe := populateFailureObserver(&out)
	var notes []engine.PopulateFailure
	for _, stage := range []string{"request_build", "token_acquisition", "transport", "body_read"} {
		for _, reason := range []string{"canceled", "deadline", "other"} {
			notes = append(notes, engine.PopulateFailure{Stage: stage, Reason: reason})
		}
	}
	notes = append(notes, engine.PopulateFailure{Stage: "http_status", Reason: "non_2xx", Status: 100}, engine.PopulateFailure{Stage: "http_status", Reason: "non_2xx", Status: 599}, engine.PopulateFailure{Stage: "qr_extract", Reason: "invalid_json", Status: 200}, engine.PopulateFailure{Stage: "qr_extract", Reason: "wrong_resource_type", Status: 200})
	var wg sync.WaitGroup
	for _, n := range notes {
		wg.Add(1)
		go func() { defer wg.Done(); observe(n) }()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != len(notes) {
		t.Fatalf("lines=%d want=%d", len(lines), len(notes))
	}
	seen := map[engine.PopulateFailure]int{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "gateway: populate_failure ") {
			t.Fatalf("bad prefix: %q", line)
		}
		body := strings.TrimPrefix(line, "gateway: populate_failure ")
		var record struct {
			Version int `json:"version"`
			engine.PopulateFailure
		}
		decoder := json.NewDecoder(strings.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.Version != 1 {
			t.Fatalf("version=%d", record.Version)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			t.Fatalf("trailing content: %v", err)
		}
		seen[record.PopulateFailure]++
	}
	for _, n := range notes {
		if seen[n] != 1 {
			t.Fatalf("note=%#v count=%d", n, seen[n])
		}
	}
}

type populateFailingWriter struct{}

func (populateFailingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestBuildPopulateFailureEvidenceWriterFailurePreservesResponse(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	h := populateFailureApp(t, map[string]string{"PROVIDER_DTR_POPULATE_URL": srv.URL}, populateFailingWriter{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/scenario/uc08", strings.NewReader(`{}`)))
	if rr.Code != 502 || rr.Body.String() != `{"error":"engine: $populate upstream failed"}`+"\n" || calls.Load() != 1 {
		t.Fatalf("status=%d body=%q calls=%d", rr.Code, rr.Body.String(), calls.Load())
	}
}
