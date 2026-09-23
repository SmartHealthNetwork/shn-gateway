package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestAppBootReadsNativeReceivePublicationFromHolderFeed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cds-services" {
			_, _ = w.Write([]byte(`{"services":[]}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	}))
	defer keys.Close()
	var feedURL string
	id, err := shnsdk.GenerateIdentity("native-payer")
	if err != nil {
		t.Fatal(err)
	}
	versions := []string{shnsdk.ContractPAPDex21, "pa.pas@9.9"}
	reg := id.RegistrationWithDeclared("payer", "https://holder.example", versions)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery":
			fmt.Fprintf(w, `{"endpoints":{"registrar":%q},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, feedURL, keys.URL, keys.URL)
		case "/holders":
			_ = json.NewEncoder(w).Encode([]shnsdk.Holder{{ID: reg.ID, Role: reg.Role, EncPub: reg.EncPub, SignPub: reg.SignPub, BaseURL: reg.BaseURL, ContractVersions: reg.ContractVersions, MessageFrames: reg.MessageFrames, RequestFrames: reg.RequestFrames}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer feed.Close()
	feedURL = feed.URL
	dir := t.TempDir()
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"ROLE": "payer", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": feed.URL + "/discovery", "FHIR_DATA_URL": "https://sor.example/fhir", "PAYER_DAVINCI_BASE_URL": backend.URL, "PAYER_DAVINCI_CONTRACT_VERSIONS": "pa.pas@9.9", "CONFORMANCE_ENFORCEMENT": "none"}
	b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.gateway.Close()
	got, ok := b.reg.Lookup(id.HolderID)
	if !ok || len(got.ContractVersions) != 2 || got.ContractVersions[1] != "pa.pas@9.9" || b.nativeResponder == nil {
		t.Fatalf("app lost ordinary feed declaration: %+v native=%T", got, b.nativeResponder)
	}
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metadata", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "davinci-pdex") || strings.Contains(w.Body.String(), "davinci-pas") {
		t.Fatalf("payer metadata made an unsupported PAS claim: %d %s", w.Code, w.Body.String())
	}
	delete(env, "PAYER_DAVINCI_CONTRACT_VERSIONS")
	if _, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "native receive declaration") {
		t.Fatalf("unbacked published line was admitted: %v", err)
	}
}
