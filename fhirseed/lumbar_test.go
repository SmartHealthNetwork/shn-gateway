package fhirseed

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestSandboxLumbarLibrary(t *testing.T) {
	lib, err := SandboxLumbarLibrary()
	if err != nil {
		t.Fatalf("SandboxLumbarLibrary: %v", err)
	}
	var l struct {
		ResourceType string `json:"resourceType"`
		URL          string `json:"url"`
		Name         string `json:"name"`
		Version      string `json:"version"`
		Content      []struct {
			ContentType string `json:"contentType"`
			Data        string `json:"data"`
		} `json:"content"`
	}
	if err := json.Unmarshal(lib, &l); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if l.ResourceType != "Library" || l.URL != "http://smarthealth.network/fhir/Library/LumbarMRICQL" {
		t.Fatalf("lib = %s / %s", l.ResourceType, l.URL)
	}
	// name/version MUST match the CQL header or the clinical-reasoning engine can't load the source.
	if l.Name != "LumbarMRICQL" || l.Version != "1.0.0" {
		t.Fatalf("name/version = %q/%q, want LumbarMRICQL/1.0.0", l.Name, l.Version)
	}
	if len(l.Content) == 0 || l.Content[0].ContentType != "text/cql" || l.Content[0].Data == "" {
		t.Fatalf("missing base64 text/cql content")
	}
	// the decoded CQL must reference the real seeded code/system (drift guard).
	raw, err := base64.StdEncoding.DecodeString(l.Content[0].Data)
	if err != nil {
		t.Fatalf("decode cql: %v", err)
	}
	cql := string(raw)
	if !strings.Contains(cql, shnsdk.SystemSHNClinical) || !strings.Contains(cql, shnsdk.ConservativeTherapyWeeksCode) {
		t.Fatalf("CQL missing seeded code/system:\n%s", cql)
	}
	for _, want := range []string{"ConservativeTherapyWeeks", "PriorSurgery", "HighDisability", "PatientReportedRequired"} {
		if !strings.Contains(cql, want) {
			t.Fatalf("CQL missing define %q:\n%s", want, cql)
		}
	}
	// HighDisability is PRESENCE-based — must NOT carry a value comparison.
	if strings.Contains(cql, `"ODI"`) && (strings.Contains(cql, ">=") || strings.Contains(cql, "> ")) {
		t.Fatalf("HighDisability must be exists-only (presence), no threshold:\n%s", cql)
	}
	// Drift guard: every ProcedureValueSet code must appear in the prior-surgery retrieve.
	for _, code := range shnsdk.ProcedureValueSet {
		if !strings.Contains(cql, code) {
			t.Fatalf("prior-surgery CQL missing ProcedureValueSet code %q:\n%s", code, cql)
		}
	}
}

func TestPutGlobalArtifact(t *testing.T) {
	var gotPath, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCT = r.URL.Path, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	lib, err := SandboxLumbarLibrary()
	if err != nil {
		t.Fatal(err)
	}
	v := shnsdk.NewFakeValidator()
	if err := PutGlobalArtifact(context.Background(), srv.URL+"/fhir/DEFAULT", v, "Library", "LumbarMRICQL", lib); err != nil {
		t.Fatalf("PutGlobalArtifact: %v", err)
	}
	if gotPath != "/fhir/DEFAULT/Library/LumbarMRICQL" || gotCT != "application/fhir+json" || !bytes.Equal(gotBody, lib) {
		t.Fatalf("PUT shape wrong: path=%q ct=%q", gotPath, gotCT)
	}
}

func TestPutGlobalArtifact_InvalidRejected(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	v := shnsdk.NewFakeValidator()
	v.RejectIfContains = "LumbarMRICQL" // force invalid
	lib, _ := SandboxLumbarLibrary()
	err := PutGlobalArtifact(context.Background(), srv.URL+"/fhir/DEFAULT", v, "Library", "LumbarMRICQL", lib)
	if err == nil {
		t.Fatal("invalid artifact must not be PUT")
	}
	// Prove the validate gate — not a transport failure — is what blocked the PUT:
	// the server must never have been reached.
	if hits != 0 {
		t.Fatalf("PUT reached the server %d time(s); validate gate did not block it", hits)
	}
}
