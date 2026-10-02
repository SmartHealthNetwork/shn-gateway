package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The payor a coverage the EHR itself sent names by reference alone: an EHR
// that fulfils the coverage template exactly (Coverage?patient=...&
// status=active, no _include) sends no Organization. The provider gateway
// reads that Organization only to choose the payer (the network's routing
// read). "Organization/<id>" is an id on the EHR's server: with no fhirServer,
// or one at the system of record's own FHIR base, it is read in the system of
// record when that names the patient by context.patientId (and, at the same
// base, once through fhirServer when that system holds no such Organization);
// with a fhirServer at another base, only once through fhirServer. Nothing is
// added to or changed in what is carried.

// TestSentPayorRefusalTexts pins every refusal of a coverage the EHR sent
// whose payor Organization it names by reference alone (CRD, and DTR for the
// unresolved payor), as STABILITY.md lists them: the prefix of any coverage
// without a payer, ": " and the reason, as a PAS Bundle's refusal is; only the
// payor nothing could resolve adds a remedy after its reason, the one that
// resolves that reference. A client matching the prefix keeps matching. Rows
// in this file reach each text.
func TestSentPayorRefusalTexts(t *testing.T) {
	const prefix = "no payer identifier on member coverage: "
	twoRefs := prefix + "the request's coverages name more than one payor Organization by reference alone"
	pinned := [][2]string{
		{noPayerSendPayor, prefix + "Coverage.payor is a reference to an Organization the gateway could not read; send the payor Organization with the coverage, or a payor identifier"},
		{noPayerUnreadRef, prefix + "Coverage.payor is a reference to an Organization the gateway could not resolve; send the payor Organization as a Bundle entry whose fullUrl is that reference, or a payor identifier"},
		{noPayerIdentifierOnlyRef, prefix + "Coverage.payor is a reference to an Organization the gateway does not read; send a payor identifier with the coverage"},
		{noPayerSentTwoRefs, twoRefs},
		{sentPayorRefusal(fhirServerTwoPayors), twoRefs},
		{noPayerUnresolvedURN, prefix + "Coverage.payor is a urn reference no Bundle entry's fullUrl matches; send the payor Organization as a Bundle entry whose fullUrl is that reference, or a payor identifier"},
	}
	// The payor Organization read through the request's fhirServer: its
	// reason, without "no coverage to route by: ", and no remedy.
	for read, want := range map[string]string{
		fhirServerNotBase:         "fhirServer is not an absolute base URL (no query or fragment)",
		fhirServerNotHTTPS:        "fhirServer must be https",
		fhirServerCredentials:     "fhirServer must not carry credentials",
		fhirServerNotPort443:      "fhirServer must use port 443",
		fhirServerPublicNot443:    "fhirServer at a public address must use port 443",
		fhirServerNotPublic:       "fhirServer's address is not public",
		fhirServerNeverRead:       "fhirServer's address is one a gateway never reads (loopback, link-local, metadata or reserved)",
		fhirServerNoAddress:       "fhirServer's host did not resolve",
		fhirServerRedirected:      "fhirServer redirected; redirects are not followed",
		fhirServerTLSFailed:       "fhirServer's TLS could not be verified",
		fhirServerUnreachable:     "fhirServer could not be reached",
		fhirServerTimedOut:        "fhirServer did not answer in time",
		fhirServerTooLarge:        "fhirServer's answer exceeds 512 KiB",
		fhirServerRefusedToken:    "fhirServer refused the fhirAuthorization token",
		fhirServerAnswered:        "fhirServer answered with an error",
		fhirServerBadToken:        "fhirAuthorization is not a bearer token",
		fhirServerNoOrganization:  "fhirServer holds no Organization for the coverage's payor",
		fhirServerNotOrganization: "fhirServer's answer is not the payor Organization",
	} {
		pinned = append(pinned, [2]string{sentPayorRefusal(read), prefix + want})
	}
	for _, row := range pinned {
		got, want := row[0], row[1]
		if got != want {
			t.Errorf("refusal %q, want %q", got, want)
		}
		if !strings.HasPrefix(got, noPayerIdentifier+": ") {
			t.Errorf("refusal %q does not begin %q", got, noPayerIdentifier+": ")
		}
	}
}

// sentBareCoverage is a Coverage the EHR sends for member, naming its payor by
// reference alone.
func sentBareCoverage(member, ref string) string {
	return `{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/` + member + `"},"payor":[{"reference":"` + ref + `"}]}`
}

// sentCoverageRequest is the EHR's order-select request for member, carrying
// its patient and the coverage given, and naming base as its fhirServer ("" for
// none).
func sentCoverageRequest(member, base, coverage string) []byte {
	b := ehrRequest(patientOnly + `,"coverage":` + coverage)
	if member != prefetchMember {
		b = bytes.ReplaceAll(b, []byte(`"example"`), []byte(`"`+member+`"`))
		b = bytes.ReplaceAll(b, []byte(`Patient/example"`), []byte(`Patient/`+member+`"`))
	}
	if base == "" {
		return bytes.Replace(b, []byte(`"fhirServer" : "https://ehr.example/fhir",`), nil, 1)
	}
	return bytes.Replace(b, []byte("https://ehr.example/fhir"), []byte(base), 1)
}

// recordingEHR is an EHR FHIR server answering every Organization read with
// org, recording each path read with the token it was read with.
func recordingEHR(t *testing.T, org func(http.ResponseWriter, *http.Request)) (*ehrServer, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	e := newEHRServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		org(w, r)
	})
	return e, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(paths)
	}
}

// wantSentCoverageExactly asserts the carried request is the EHR's less the
// callback, and its prefetch.coverage the EHR's own bytes.
func wantSentCoverageExactly(t *testing.T, ehr, sent []byte) {
	t.Helper()
	wantCarriedLessCallback(t, ehr, sent)
	want, _ := valueOf(t, ehr, "prefetch", "coverage")
	got, ok := valueOf(t, sent, "prefetch", "coverage")
	if !ok || got != want {
		t.Fatalf("carried prefetch.coverage %q, want the EHR's bytes %q", got, want)
	}
	for _, key := range []string{`"resourceType":"Organization"`, "00001", "ehr-secret-token"} {
		if bytes.Contains(sent, []byte(key)) {
			t.Fatalf("the carried request holds %q: something read was added", key)
		}
	}
}

// refusedWith asserts the exact refusal, before the network.
func refusedWith(t *testing.T, env *inProcessExchange, rec *httptest.ResponseRecorder, status int, msg string) {
	t.Helper()
	// A CDS Hooks refusal is {"error": ...}; a FHIR operation's is an
	// OperationOutcome.
	var answer struct {
		Error string `json:"error"`
		Issue []struct {
			Diagnostics string `json:"diagnostics"`
		} `json:"issue"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	if len(answer.Issue) == 1 {
		answer.Error = answer.Issue[0].Diagnostics
	}
	if rec.Code != status || answer.Error != msg {
		t.Fatalf("answer %d %s, want %d %q", rec.Code, rec.Body.String(), status, msg)
	}
	if n := env.routeHitCount(); n != 0 {
		t.Fatalf("the refused request crossed the network (%d)", n)
	}
}

// Row 1 and the fidelity row: a member the system of record does not hold, a
// coverage (a Coverage, or the template's searchset) naming its payor by
// reference alone, and a fhirServer. Routed by exactly one Organization read
// there, with the request's own token, with or without the enrichment
// opt-in; the carried prefetch.coverage is byte-identical to the EHR's, and
// the read is recorded as a prefetch.obtained event with source fhirServer
// and logged, each saying it read the payor Organization of the coverage the
// EHR sent (no coverage was obtained).
func TestCRDIngressResolvesASentBarePayorThroughFHIRServer(t *testing.T) {
	forms := map[string]func(base string) string{
		"a Coverage":                                 func(string) string { return sentBareCoverage(strangerMember, "Organization/o1") },
		"the template's searchset":                   func(string) string { return bareRefSearchset("o1") },
		"two Coverages naming the same Organization": func(string) string { return bareRefSearchset("o1", "o1") },
		"a reference absolute on fhirServer": func(base string) string {
			return sentBareCoverage(strangerMember, base+"/Organization/o1")
		},
	}
	for name, form := range forms {
		for _, enrich := range []bool{false, true} {
			t.Run(name+"/enrich="+map[bool]string{false: "off", true: "on"}[enrich], func(t *testing.T) {
				e, paths := recordingEHR(t, ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))
				env, obs := fhirServerEnv(t, e, "")
				env.originator.cfg.EnrichNativeRequests = enrich
				logged := captureLog(t)
				body := sentCoverageRequest(strangerMember, e.base, form(e.base))
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(body))
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s (network hits %d)", rec.Code, rec.Body.String(), env.routeHitCount())
				}
				if want := []string{"/fhir/Organization/o1 Bearer ehr-secret-token"}; !slices.Equal(paths(), want) {
					t.Fatalf("read %v, want %v", paths(), want)
				}
				wantSentCoverageExactly(t, body, sentRequest(t, env))
				if ev := obs.prefetch(t)["coverage"]; ev.Source != sourceFHIRServer || ev.Query != "/fhir/Organization/o1" || ev.Outcome != SearchOK || ev.Count != 1 ||
					ev.Reason != "payor Organization of the coverage the EHR sent" {
					t.Fatalf("recorded %+v", ev)
				}
				log := logged.String()
				if !strings.Contains(log, "gateway: payor Organization of the coverage the EHR sent, read through fhirServer only to route: ok") ||
					strings.Contains(log, "prefetch coverage from fhirServer") {
					t.Fatalf("logged %q", log)
				}
			})
		}
	}
}

// sentPayorSoR is the system of record of the sent-payor rows: it holds
// prefetchMember (named by the same id unless sorID), at FHIR base base ("" names
// none), with Organization/o1 naming payer when payer is not "".
func sentPayorSoR(base, payer string) *prefetchSoR {
	s := newPrefetchSoR()
	s.fhirBase = base
	if payer != "" {
		s.reads["Organization/o1"] = []byte(payorOrganization("o1", payer))
	}
	return s
}

// Row 2: where "Organization/<id>" is read for a member the system of record
// holds depends on the request's fhirServer. With none, or one at the
// system's own FHIR base (compared with the scheme and host lowercased, the
// default port dropped and a trailing slash trimmed), the payor is read in the
// system of record when it names the patient by context.patientId, and
// fhirServer is read only when that system holds no such Organization or names
// the patient by another id. A fhirServer at another base (or a system naming
// no base) is the only place the id is read: the system of record is never
// asked for the Organization, even for a patient id it shares. A reference
// written absolute on fhirServer is read in the system of record by its
// "Organization/<id>". Every row is routed and carries the EHR's coverage
// exactly.
func TestCRDIngressResolvesASentBarePayorWhereItsIDIs(t *testing.T) {
	const fhirRead = "/fhir/Organization/o1 Bearer ehr-secret-token"
	same := func(e *ehrServer) string { return e.base }
	for name, row := range map[string]struct {
		mode     string
		sorBase  func(e *ehrServer) string
		reqBase  func(e *ehrServer) string // nil: e.base; returning "": no fhirServer
		sorPayer string                    // the system's Organization/o1, "" for none
		sorID    string
		refErr   error
		ref      func(e *ehrServer) string
		wantSoR  []string
		wantFHIR []string
	}{
		"the same base": {sorBase: same, sorPayer: "00001", wantSoR: []string{"Organization/o1"}},
		"the same base, the read off": {mode: FHIRServerReadOff, sorBase: same, sorPayer: "00001",
			wantSoR: []string{"Organization/o1"}},
		"the same base written with case and a trailing slash": {sorBase: func(*ehrServer) string { return "HTTPS://EXAMPLE.Com:8443/fhir/" },
			sorPayer: "00001", wantSoR: []string{"Organization/o1"}},
		"the same base, the default port written on the system": {sorBase: func(*ehrServer) string { return "https://example.com:443/fhir" },
			reqBase: func(e *ehrServer) string { return e.publicBase }, sorPayer: "00001", wantSoR: []string{"Organization/o1"}},
		"the same base, the default port written on fhirServer": {sorBase: func(e *ehrServer) string { return e.publicBase },
			reqBase: func(*ehrServer) string { return "https://example.com:443/fhir/" }, sorPayer: "00001", wantSoR: []string{"Organization/o1"}},
		"the same base, a reference absolute on fhirServer": {sorBase: same, sorPayer: "00001",
			ref: func(e *ehrServer) string { return e.base + "/Organization/o1" }, wantSoR: []string{"Organization/o1"}},
		"the same base, the system holds no such Organization": {sorBase: same,
			wantSoR: []string{"Organization/o1"}, wantFHIR: []string{fhirRead}},
		"the same base, the system names the patient by another id": {sorBase: same, sorPayer: "00002", sorID: "pat-elsewhere",
			wantFHIR: []string{fhirRead}},
		"the same base, the system cannot name the patient": {sorBase: same, sorPayer: "00002", refErr: &SoRReadError{Kind: SoRUnavailable},
			wantFHIR: []string{fhirRead}},
		"another host": {sorBase: func(*ehrServer) string { return "https://sor.example/fhir" }, sorPayer: "00002",
			wantFHIR: []string{fhirRead}},
		"another base path": {sorBase: func(e *ehrServer) string { return e.base + "/r4" }, sorPayer: "00002",
			wantFHIR: []string{fhirRead}},
		"another port": {sorBase: func(*ehrServer) string { return "https://example.com:9443/fhir" }, sorPayer: "00002",
			wantFHIR: []string{fhirRead}},
		"another scheme": {sorBase: func(*ehrServer) string { return "http://example.com:8443/fhir" }, sorPayer: "00002",
			wantFHIR: []string{fhirRead}},
		"another base, a reference absolute on fhirServer": {sorBase: func(*ehrServer) string { return "https://sor.example/fhir" }, sorPayer: "00002",
			ref: func(e *ehrServer) string { return e.base + "/Organization/o1" }, wantFHIR: []string{fhirRead}},
		"a system naming no base": {sorBase: func(*ehrServer) string { return "" }, sorPayer: "00002",
			wantFHIR: []string{fhirRead}},
		"no fhirServer": {sorBase: func(*ehrServer) string { return "https://sor.example/fhir" }, reqBase: func(*ehrServer) string { return "" },
			sorPayer: "00001", wantSoR: []string{"Organization/o1"}},
		"no fhirServer, a system naming no base": {sorBase: func(*ehrServer) string { return "" }, reqBase: func(*ehrServer) string { return "" },
			sorPayer: "00001", wantSoR: []string{"Organization/o1"}},
	} {
		t.Run(name, func(t *testing.T) {
			e, paths := recordingEHR(t, ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))
			env, _ := fhirServerEnv(t, e, row.mode)
			s := sentPayorSoR(row.sorBase(e), row.sorPayer)
			s.sorID, s.refErr = row.sorID, row.refErr
			env.originator.cfg.SoR = s.sor()
			base := e.base
			if row.reqBase != nil {
				base = row.reqBase(e)
			}
			ref := "Organization/o1"
			if row.ref != nil {
				ref = row.ref(e)
			}
			body := sentCoverageRequest(prefetchMember, base, sentBareCoverage(prefetchMember, ref))
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if _, read := s.calls(); !slices.Equal(read, row.wantSoR) {
				t.Fatalf("system of record read %v, want %v", read, row.wantSoR)
			}
			if got := paths(); !slices.Equal(got, row.wantFHIR) {
				t.Fatalf("fhirServer read %v, want %v", got, row.wantFHIR)
			}
			wantSentCoverageExactly(t, body, sentRequest(t, env))
		})
	}
}

// The base comparison: the scheme and host lowercased, the scheme's default
// port dropped and a trailing slash trimmed; anything else that differs, or a
// URL that is not an absolute http(s) base, is another base.
func TestSameFHIRBase(t *testing.T) {
	for _, row := range []struct {
		a, b string
		want bool
	}{
		{"https://ehr.example/fhir", "https://ehr.example/fhir", true},
		{"https://ehr.example/fhir/", "https://ehr.example/fhir", true},
		{"HTTPS://EHR.Example/fhir", "https://ehr.example/fhir", true},
		{"https://ehr.example:443/fhir", "https://ehr.example/fhir", true},
		{"http://ehr.example:80/fhir", "http://ehr.example/fhir/", true},
		{"https://[::1]:443/fhir", "https://[::1]/fhir", true},
		{"https://ehr.example", "https://ehr.example/", true},
		{"https://ehr.example/FHIR", "https://ehr.example/fhir", false},
		{"https://\u0130.example/fhir", "https://i.example/fhir", false},      // "İ" would lowercase to "i"
		{"https://\u0130.example/fhir", "https://\u0130.example/fhir", false}, // a non-ASCII host is never compared
		{"https://ehr.example/fhir/r4", "https://ehr.example/fhir", false},
		{"https://ehr.example:8443/fhir", "https://ehr.example/fhir", false},
		{"http://ehr.example:443/fhir", "https://ehr.example/fhir", false},
		{"http://ehr.example/fhir", "https://ehr.example/fhir", false},
		{"https://other.example/fhir", "https://ehr.example/fhir", false},
		{"https://ehr.example/fhir?x=1", "https://ehr.example/fhir", false},
		{"https://ehr.example/fhir#f", "https://ehr.example/fhir", false},
		{"https://u@ehr.example/fhir", "https://ehr.example/fhir", false},
		{"ftp://ehr.example/fhir", "ftp://ehr.example/fhir", false},
		{"/fhir", "/fhir", false},
		{"", "", false},
		{"", "https://ehr.example/fhir", false},
	} {
		if got := sameFHIRBase(row.a, row.b); got != row.want {
			t.Errorf("sameFHIRBase(%q, %q) = %v, want %v", row.a, row.b, got, row.want)
		}
		if got := sameFHIRBase(row.b, row.a); got != row.want {
			t.Errorf("sameFHIRBase(%q, %q) = %v, want %v", row.b, row.a, got, row.want)
		}
	}
}

// Row 3: nothing to read the payor through (the read off, no fhirServer, a
// null one, the system of record holding no such Organization and no
// fhirServer, a system at another base with the read off, or a system that
// cannot name the patient and no fhirServer): refused 422 naming the remedy,
// at every level, before the network, with no fhirServer read. A reference on
// another server, versioned, with a fragment, a leading slash or a dot
// segment is never read: refused 422 with the remedy that resolves it. An
// absolute reference a Bundle fullUrl can equal names the coverage Bundle's
// entry (noPayerUnreadRef), and so does one on fhirServer itself with the
// read off; any other only a payor identifier (noPayerIdentifierOnlyRef).
func TestCRDIngressSentBarePayorUnreadableNamesTheRemedy(t *testing.T) {
	for name, row := range map[string]struct {
		mode    string
		member  string
		named   bool
		edit    func(body []byte) []byte
		ref     func(base string) string
		sor     func(s *prefetchSoR)
		wantSoR []string
		want    string // "": noPayerSendPayor
	}{
		"the read off":      {mode: FHIRServerReadOff, member: strangerMember, named: true},
		"no fhirServer":     {member: strangerMember},
		"a null fhirServer": {member: strangerMember, named: true, edit: nullFHIRServer},
		"held, no such Organization, no fhirServer": {member: prefetchMember, wantSoR: []string{"Organization/o1"}},
		"held, no such Organization, the read off":  {mode: FHIRServerReadOff, member: prefetchMember, named: true},
		"held with the Organization at another base, the read off": {mode: FHIRServerReadOff, member: prefetchMember, named: true,
			sor: func(s *prefetchSoR) {
				s.fhirBase = "https://sor.example/fhir"
				s.reads["Organization/o1"] = []byte(payorOrganization("o1", "00001"))
			}},
		"held with the Organization, the system cannot name the patient, no fhirServer": {member: prefetchMember,
			sor: func(s *prefetchSoR) {
				s.refErr = &SoRReadError{Kind: SoRUnavailable}
				s.reads["Organization/o1"] = []byte(payorOrganization("o1", "00001"))
			}},
		"a reference on another server": {member: strangerMember, named: true,
			ref: func(string) string { return "https://other.example/fhir/Organization/o1" }, want: noPayerUnreadRef},
		"a reference absolute on fhirServer, the read off": {mode: FHIRServerReadOff, member: strangerMember, named: true,
			ref: func(base string) string { return base + "/Organization/o1" }, want: noPayerUnreadRef},
		"a reference absolute on fhirServer, no fhirServer": {member: strangerMember,
			ref: func(base string) string { return base + "/Organization/o1" }, want: noPayerUnreadRef},
		"a versioned reference": {member: strangerMember, named: true, ref: func(string) string { return "Organization/o1/_history/2" },
			want: noPayerIdentifierOnlyRef},
		"a dot segment": {member: strangerMember, named: true, ref: func(string) string { return "Organization/.." },
			want: noPayerIdentifierOnlyRef},
		"a single dot": {member: strangerMember, named: true, ref: func(string) string { return "Organization/." },
			want: noPayerIdentifierOnlyRef},
		"a leading slash": {member: strangerMember, named: true, ref: func(string) string { return "/Organization/o1" },
			want: noPayerIdentifierOnlyRef},
		"a fragment": {member: strangerMember, named: true, ref: func(string) string { return "Organization/o1#x" },
			want: noPayerIdentifierOnlyRef},
		"a versioned reference on another server": {member: strangerMember, named: true,
			ref: func(string) string { return "https://other.example/fhir/Organization/o1/_history/2" }, want: noPayerIdentifierOnlyRef},
		"a reference on another server with a fragment": {member: strangerMember, named: true,
			ref: func(string) string { return "https://other.example/fhir/Organization/o1#x" }, want: noPayerIdentifierOnlyRef},
		"a reference on another server with a dot segment": {member: strangerMember, named: true,
			ref: func(string) string { return "https://other.example/fhir/../Organization/o1" }, want: noPayerIdentifierOnlyRef},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				e, paths := recordingEHR(t, ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))
				env, _ := fhirServerEnv(t, e, row.mode)
				env.originator.cfg.ConformanceEnforcement = level
				s := newPrefetchSoR()
				if row.sor != nil {
					row.sor(s)
				}
				env.originator.cfg.SoR = s.sor()
				base := ""
				if row.named {
					base = e.base
				}
				ref := "Organization/o1"
				if row.ref != nil {
					ref = row.ref(e.base)
				}
				body := sentCoverageRequest(row.member, base, sentBareCoverage(row.member, ref))
				if row.edit != nil {
					body = row.edit(body)
				}
				want := row.want
				if want == "" {
					want = noPayerSendPayor
				}
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(body))
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, want)
				if got := paths(); len(got) != 0 {
					t.Fatalf("fhirServer read %v, want none", got)
				}
				if _, read := s.calls(); !slices.Equal(read, row.wantSoR) {
					t.Fatalf("system of record read %v, want %v", read, row.wantSoR)
				}
			})
		}
	}
}

func nullFHIRServer(body []byte) []byte {
	i := bytes.Index(body, []byte(`"fhirServer" : "`))
	j := bytes.Index(body[i:], []byte(`",`))
	return append(append(append([]byte{}, body[:i]...), []byte(`"fhirServer" : null,`)...), body[i+j+2:]...)
}

// Row 4: the Organization read's refusals keep the fhirServer read's statuses,
// at every level, before the network: none there (404, 410) is a 412; an
// answer that is not the Organization asked for is a 502; a refused token is a
// 412. The request carried a coverage, so each reason follows the prefix of a
// coverage without a payer, never "no coverage to route by". Two different
// Organizations by reference alone are a 422 after one read; an Organization
// with no payer identifier is a 422 that says so.
func TestCRDIngressSentBarePayorReadRefusals(t *testing.T) {
	for name, row := range map[string]struct {
		coverage string
		org      func(http.ResponseWriter, *http.Request)
		calls    int
		status   int
		msg      string
	}{
		"no such Organization": {sentBareCoverage(strangerMember, "Organization/o1"), ehrAnswer(404, "", ""), 1, 412,
			"no payer identifier on member coverage: fhirServer holds no Organization for the coverage's payor"},
		"the Organization gone": {sentBareCoverage(strangerMember, "Organization/o1"), ehrAnswer(410, "", ""), 1, 412,
			"no payer identifier on member coverage: fhirServer holds no Organization for the coverage's payor"},
		"another Organization answered": {sentBareCoverage(strangerMember, "Organization/o1"), ehrAnswer(200, "application/json", payorOrganization("o2", "00001")),
			1, 502, "no payer identifier on member coverage: fhirServer's answer is not the payor Organization"},
		"not an Organization": {sentBareCoverage(strangerMember, "Organization/o1"), ehrAnswer(200, "application/json", `{"resourceType":"Patient","id":"o1"}`),
			1, 502, "no payer identifier on member coverage: fhirServer's answer is not the payor Organization"},
		"the token refused": {sentBareCoverage(strangerMember, "Organization/o1"), ehrAnswer(403, "", ""), 1, 412,
			"no payer identifier on member coverage: fhirServer refused the fhirAuthorization token"},
		"an error": {sentBareCoverage(strangerMember, "Organization/o1"), ehrAnswer(500, "", ""), 1, 412,
			"no payer identifier on member coverage: fhirServer answered with an error"},
		"two Organizations by reference alone": {bareRefSearchset("o1", "o2"), ehrAnswer(200, "application/json", payorOrganization("o1", "00001")),
			1, 422, noPayerSentTwoRefs},
		"an Organization with no payer identifier": {sentBareCoverage(strangerMember, "Organization/o1"), ehrAnswer(200, "application/json", `{"resourceType":"Organization","id":"o1","name":"Payer One"}`),
			1, 422, noPayerOrganizationNoIdentifier},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				e, paths := recordingEHR(t, row.org)
				env, _ := fhirServerEnv(t, e, "")
				env.originator.cfg.ConformanceEnforcement = level
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(sentCoverageRequest(strangerMember, e.base, row.coverage)))
				refusedWith(t, env, rec, row.status, row.msg)
				if got := paths(); len(got) != row.calls {
					t.Fatalf("fhirServer read %v, want %d reads", got, row.calls)
				}
			})
		}
	}
	// A refused read is recorded and logged as the payor Organization read.
	t.Run("recorded", func(t *testing.T) {
		e, _ := recordingEHR(t, ehrAnswer(404, "", ""))
		env, obs := fhirServerEnv(t, e, "")
		logged := captureLog(t)
		rec := httptest.NewRecorder()
		env.originator.handleCRDIngress(rec, crdIngressPost(sentCoverageRequest(strangerMember, e.base, sentBareCoverage(strangerMember, "Organization/o1"))))
		ev := obs.prefetch(t)["coverage"]
		if ev.Source != sourceFHIRServer || ev.Outcome != SearchZero ||
			ev.Reason != "payor Organization of the coverage the EHR sent: fhirServer holds no Organization for the coverage's payor" {
			t.Fatalf("recorded %+v", ev)
		}
		if want := "gateway: payor Organization of the coverage the EHR sent, read through fhirServer only to route: zero"; !strings.Contains(logged.String(), want) {
			t.Fatalf("logged %q, want %q", logged.String(), want)
		}
	})
}

// The exchange record of a refused sent-payor read classifies by status, as
// the fhirServer read's refusals do: a 412 or 422 is the provider gateway's
// routing refusal; a 502 (an answer that is not the Organization asked for)
// is the EHR's server answering wrongly, recorded other.
func TestExchangeRecord_SentBarePayorRefusals(t *testing.T) {
	for name, row := range map[string]struct {
		coverage string
		org      func(http.ResponseWriter, *http.Request)
		status   int
		routing  bool
	}{
		"no such Organization":                 {sentBareCoverage(strangerMember, "Organization/o1"), ehrAnswer(404, "", ""), 412, true},
		"two Organizations by reference alone": {bareRefSearchset("o1", "o2"), ehrAnswer(200, "application/json", payorOrganization("o1", "00001")), 422, true},
		"nothing to read it through":           {sentBareCoverage(strangerMember, "Organization/o1"), nil, 422, true},
		"another Organization answered":        {sentBareCoverage(strangerMember, "Organization/o1"), ehrAnswer(200, "application/json", payorOrganization("o2", "00001")), 502, false},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := recordingEHR(t, ehrAnswer(200, "application/json", payorOrganization("o1", "00001")))
			if row.org != nil {
				e, _ = recordingEHR(t, row.org)
			}
			env, _ := fhirServerEnv(t, e, "")
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			base := e.base
			if row.org == nil {
				base = ""
			}
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCRD)(rec, crdIngressPost(sentCoverageRequest(strangerMember, base, row.coverage)))
			if rec.Code != row.status {
				t.Fatalf("answer %d %s, want %d", rec.Code, rec.Body.String(), row.status)
			}
			r := got.only(t)
			if row.routing {
				wantRefusal(t, r, row.status, RefusedByProviderGateway, RefusalRouting)
				return
			}
			if r.Outcome != ExchangeOther || r.RefusedBy != "" || r.Rule != "" || r.Status != row.status {
				t.Fatalf("record = %s %s/%s status %d, want other with no refusal, status %d", r.Outcome, r.RefusedBy, r.Rule, r.Status, row.status)
			}
		})
	}
}

// bareSoRCoverage is the system of record's own Coverage for its patient,
// naming its payor by reference alone, with no Organization included.
const bareSoRCoverage = `{"resourceType":"Coverage","id":"sc1","status":"active","beneficiary":{"reference":"Patient/` + prefetchSoRID + `"},"payor":[{"reference":"Organization/o1"}]}`

// A system-of-record read of the payor Organization that fails refuses the
// request with the system-of-record failure status (unavailable 503, an
// invalid response 502), before the network, at every level, and nothing
// else is read for it: not fhirServer, even at the system's own base. So does
// the same failure for a coverage the gateway read itself from the system of
// record (which reads its payor there); without the failure each row routes.
func TestCRDIngressSentBarePayorSoRReadFails(t *testing.T) {
	unavailable := &SoRReadError{Kind: SoRUnavailable}
	for name, row := range map[string]struct {
		named  bool // fhirServer at the system's own base
		sent   bool // the EHR sent the coverage; else the gateway reads it
		err    error
		status int
		msg    string
	}{
		"sent, fhirServer at the system's base": {named: true, sent: true, err: unavailable, status: 503, msg: "system of record unavailable"},
		"sent, no fhirServer":                   {sent: true, err: unavailable, status: 503, msg: "system of record unavailable"},
		"sent, an invalid response": {named: true, sent: true, err: &SoRReadError{Kind: SoRInvalidResponse}, status: 502,
			msg: "system of record returned an invalid response"},
		"read by the gateway":                {named: true, err: unavailable, status: 503, msg: "system of record unavailable"},
		"sent, the read answered":            {named: true, sent: true},
		"read by the gateway, read answered": {named: true},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				e, paths := recordingEHR(t, ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))
				env, _ := fhirServerEnv(t, e, "")
				env.originator.cfg.ConformanceEnforcement = level
				s := sentPayorSoR(e.base, "00001")
				if row.err != nil {
					s.readErrs = map[string]error{"Organization/o1": row.err}
				}
				if !row.sent {
					s.answer(t, "Coverage", searchsetOf(matchOf(bareSoRCoverage)))
				}
				env.originator.cfg.SoR = s.sor()
				base := ""
				if row.named {
					base = e.base
				}
				body := sentCoverageRequest(prefetchMember, base, sentBareCoverage(prefetchMember, "Organization/o1"))
				if !row.sent {
					body = ehrRequest(patientOnly)
					body = bytes.Replace(body, []byte("https://ehr.example/fhir"), []byte(e.base), 1)
				}
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(body))
				if row.err == nil {
					if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
						t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
					}
				} else {
					refusedWith(t, env, rec, row.status, row.msg)
				}
				if _, read := s.calls(); !slices.Equal(read, []string{"Organization/o1"}) {
					t.Fatalf("system of record read %v, want the payor once", read)
				}
				if got := paths(); len(got) != 0 {
					t.Fatalf("fhirServer read %v, want none", got)
				}
			})
		}
	}
}

// $questionnaire-package: a coverage parameter the EHR sent naming its payor
// by reference alone is resolved through the system of record when it names
// the patient by the request's member id (a $questionnaire-package request
// names no fhirServer, so the system of record is where "Organization/<id>"
// is read). The request is carried exactly. Otherwise it is refused 422 naming
// the remedy, with no Organization read: a system that cannot name the
// patient counts as one that does not hold it, as on a CDS Hooks request. A
// failed read of the Organization there is the system-of-record failure.
func TestDTRIngressResolvesASentBarePayorThroughTheSystemOfRecord(t *testing.T) {
	param := `{"name":"coverage","resource":` + sentBareCoverage(prefetchMember, "Organization/o1") + `}`
	body := ehrParams(param, dtrQuestionnaire)
	t.Run("held", func(t *testing.T) {
		s := newPrefetchSoR()
		s.reads["Organization/o1"] = []byte(payorOrganization("o1", "00001"))
		env, rec := dtrIngressRow(t, s, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		if _, read := s.calls(); !slices.Equal(read, []string{"Organization/o1"}) {
			t.Fatalf("system of record read %v, want the payor", read)
		}
		if _, sent := sentOperation(t, env); !bytes.Equal(sent, body) {
			t.Fatalf("carried %s, want the EHR's bytes exactly", sent)
		}
	})
	for name, row := range map[string]struct {
		sor  func() *prefetchSoR
		read []string
	}{
		"held, no such Organization": {func() *prefetchSoR { return newPrefetchSoR() }, []string{"Organization/o1"}},
		"named by another id": {func() *prefetchSoR {
			s := newPrefetchSoR()
			s.sorID = "pat-elsewhere"
			s.reads["Organization/o1"] = []byte(payorOrganization("o1", "00001"))
			return s
		}, nil},
		"the system of record cannot name the patient": {func() *prefetchSoR {
			s := newPrefetchSoR()
			s.refErr = &SoRReadError{Kind: SoRUnavailable}
			s.reads["Organization/o1"] = []byte(payorOrganization("o1", "00001"))
			return s
		}, nil},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				s := row.sor()
				env, rec := dtrIngressRowAt(t, s, body, level)
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, noPayerSendPayor)
				if _, read := s.calls(); !slices.Equal(read, row.read) {
					t.Fatalf("system of record read %v, want %v", read, row.read)
				}
			})
		}
	}
	for name, row := range map[string]struct {
		err    error
		status int
		msg    string
	}{
		"the Organization read unavailable": {&SoRReadError{Kind: SoRUnavailable}, http.StatusServiceUnavailable, "system of record unavailable"},
		"the Organization read invalid":     {&SoRReadError{Kind: SoRInvalidResponse}, http.StatusBadGateway, "system of record returned an invalid response"},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				s.reads["Organization/o1"] = []byte(payorOrganization("o1", "00001"))
				s.readErrs = map[string]error{"Organization/o1": row.err}
				env, rec := dtrIngressRowAt(t, s, body, level)
				refusedWith(t, env, rec, row.status, row.msg)
				if _, read := s.calls(); !slices.Equal(read, []string{"Organization/o1"}) {
					t.Fatalf("system of record read %v, want the payor once", read)
				}
			})
		}
	}
	// With no fhirServer to be absolute on, only "Organization/<id>" is read,
	// with or without the opt-in (under which a request carrying no Patient
	// has the patient's own appended; the EHR's coverage is still its own).
	// An absolute reference is refused naming the Bundle entry whose fullUrl
	// would resolve it; any other reference to an Organization naming a payor
	// identifier only.
	for ref, want := range map[string]string{
		"https://other.example/fhir/Organization/o1": noPayerUnreadRef,
		"/Organization/o1":                           noPayerIdentifierOnlyRef,
		"Organization/o1/_history/2":                 noPayerIdentifierOnlyRef,
		"Organization/o1#x":                          noPayerIdentifierOnlyRef,
		"Organization/..":                            noPayerIdentifierOnlyRef,
	} {
		for _, level := range allLevels {
			for _, enrich := range []bool{false, true} {
				t.Run(fmt.Sprintf("never read: %s/%s/enrich=%t", ref, level, enrich), func(t *testing.T) {
					// The system of record holds the reference as written too, so
					// a refusal is the guard's, never a read that found nothing
					// (an edited request, under the opt-in, once read it so).
					s := newPrefetchSoR()
					s.reads["Organization/o1"] = []byte(payorOrganization("o1", "00001"))
					s.reads[ref] = []byte(payorOrganization("o1", "00001"))
					other := ehrParams(`{"name":"coverage","resource":`+sentBareCoverage(prefetchMember, ref)+`}`, dtrQuestionnaire)
					env, rec := dtrIngressRowWith(t, s, other, level, enrich)
					refusedWith(t, env, rec, http.StatusUnprocessableEntity, want)
					// Under the opt-in the one read is the Patient appended
					// (E-05); never the payor.
					var wantRead []string
					if enrich {
						wantRead = []string{"Patient/" + prefetchSoRID}
					}
					if searched, read := s.calls(); !slices.Equal(read, wantRead) || len(searched) != 0 {
						t.Fatalf("system of record read %v, searched %v; want read %v, no search", read, searched, wantRead)
					}
				})
			}
		}
	}
	// Under the opt-in, a held member whose system of record lacks the
	// Organization is refused with the remedy too, at every level, after
	// reading the Patient appended (E-05) and the payor.
	for _, level := range allLevels {
		t.Run("opt-in, held, no such Organization/"+level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			env, rec := dtrIngressRowWith(t, s, body, level, true)
			refusedWith(t, env, rec, http.StatusUnprocessableEntity, noPayerSendPayor)
			if searched, read := s.calls(); !slices.Equal(read, []string{"Patient/" + prefetchSoRID, "Organization/o1"}) || len(searched) != 0 {
				t.Fatalf("system of record read %v, searched %v; want the Patient and the payor read, no search", read, searched)
			}
		})
	}
}

// Under the opt-in, a coverage the EHR sent naming its payor as
// "Organization/<id>" alone resolves through the system of record exactly as
// at the default: the Organization is read only to route by, the request is
// routed, and the EHR's coverage is carried as sent, at every level. On CDS
// Hooks (no fhirServer, so the system of record is where the id is read) the
// history keys the request left out are filled; on $questionnaire-package the
// patient's own Patient is appended (E-05) after the EHR's parameters. Nothing
// the routing read returned is added.
func TestSentBarePayorResolvesThroughTheSystemOfRecordUnderTheOptIn(t *testing.T) {
	for _, level := range allLevels {
		t.Run("CDS Hooks/"+level.String(), func(t *testing.T) {
			s := sentPayorSoR("", "00001")
			body := sentCoverageRequest(prefetchMember, "", sentBareCoverage(prefetchMember, "Organization/o1"))
			env, rec, _ := levelIngressRowWith(t, s, level, body, true)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			searched, read := s.calls()
			if !slices.Equal(read, []string{"Organization/o1"}) {
				t.Fatalf("system of record read %v, want the payor once", read)
			}
			if slices.ContainsFunc(searched, func(q string) bool { return strings.HasPrefix(q, "Coverage?") }) {
				t.Fatalf("searched %v: the request carries its coverage", searched)
			}
			sent := sentRequest(t, env)
			for _, key := range []string{"patient", "coverage"} {
				want, _ := valueOf(t, body, "prefetch", key)
				if got, ok := valueOf(t, sent, "prefetch", key); !ok || got != want {
					t.Fatalf("carried prefetch.%s %q, want the EHR's bytes %q", key, got, want)
				}
			}
			if got := names(membersOf(t, sent, "prefetch")); !slices.Equal(got, []string{"patient", "coverage", "serviceHistory", "deviceHistory", "medicationHistory", "questionnaireResponses"}) {
				t.Fatalf("prefetch keys %v, want the EHR's and the filled histories", got)
			}
			for _, key := range []string{`"resourceType":"Organization"`, "00001", "ehr-secret-token"} {
				if bytes.Contains(sent, []byte(key)) {
					t.Fatalf("the carried request holds %q: something read only to route by was added", key)
				}
			}
		})
		t.Run("questionnaire-package/"+level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.reads["Organization/o1"] = []byte(payorOrganization("o1", "00001"))
			body := ehrParams(`{"name":"coverage","resource":`+sentBareCoverage(prefetchMember, "Organization/o1")+`}`, dtrQuestionnaire)
			env, rec := dtrIngressRowWith(t, s, body, level, true)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if searched, read := s.calls(); !slices.Equal(read, []string{"Patient/" + prefetchSoRID, "Organization/o1"}) || len(searched) != 0 {
				t.Fatalf("system of record read %v, searched %v; want the Patient appended and the payor, no search", read, searched)
			}
			_, sent := sentOperation(t, env)
			k := bytes.LastIndex(body, []byte("\n  ]"))
			if !bytes.HasPrefix(sent, body[:k]) || !bytes.HasSuffix(sent, body[k:]) {
				t.Fatalf("the EHR's bytes changed:\n%s", sent)
			}
			added := string(sent[k : len(sent)-(len(body)-k)])
			if !strings.Contains(added, `{"name":"referenced","resource":`+string(s.reads["Patient/"+prefetchSoRID])+`}`) ||
				strings.Contains(added, "Organization") || strings.Contains(added, "Coverage") {
				t.Fatalf("added %q, want only the patient's own Patient", added)
			}
		})
	}
}

// dtrIngressRowAt is dtrIngressRow at a conformance level: a routing refusal
// is the network's, so it refuses at every level.
func dtrIngressRowAt(t *testing.T, s *prefetchSoR, body []byte, level ConformanceEnforcement) (*inProcessExchange, *httptest.ResponseRecorder) {
	t.Helper()
	return dtrIngressRowWith(t, s, body, level, false)
}

// dtrIngressRowWith is dtrIngressRowAt with the enrichment opt-in set.
func dtrIngressRowWith(t *testing.T, s *prefetchSoR, body []byte, level ConformanceEnforcement, enrich bool) (*inProcessExchange, *httptest.ResponseRecorder) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.ConformanceEnforcement = level
	env.originator.cfg.EnrichNativeRequests = enrich
	declareFramedDTR(t, env, true)
	env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
	return env, postDTRIngress(env, body)
}

// The observer's decoration of the system of record (installed whenever an
// Observer is configured) passes the connector's FHIR base through, so a
// gateway with an observer compares the same base as one without; a connector
// naming none stays none.
func TestObservingSoRPassesTheFHIRBase(t *testing.T) {
	decorated := observingSoR{inner: sentPayorSoR("https://sor.example/fhir", "").sor(), observer: func(ObserverEvent) {}, clock: time.Now}
	if got := sorFHIRBase(decorated); got != "https://sor.example/fhir" {
		t.Fatalf("sorFHIRBase = %q through the decoration, want the connector's", got)
	}
	if got := sorFHIRBase(observingSoR{inner: newCensusSoR(), observer: func(ObserverEvent) {}, clock: time.Now}); got != "" {
		t.Fatalf("sorFHIRBase = %q for a connector naming none", got)
	}
}

// Only an Organization or urn reference nothing could read gets a remedy: a
// sent coverage whose payor is another kind of reference (a self-pay
// RelatedPerson, a Patient, a RelatedPerson on a host named Organization)
// that the request does not resolve is
// refused with the bare text, on CDS Hooks and on $questionnaire-package, at
// every level, with no read.
func TestSentBarePayorOfAnotherKindKeepsTheBareRefusal(t *testing.T) {
	for _, ref := range []string{"RelatedPerson/r1", "Patient/nobody", "https://Organization/fhir/RelatedPerson/r1"} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			t.Run("CDS Hooks/"+ref+"/"+level.String(), func(t *testing.T) {
				e, paths := recordingEHR(t, ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))
				env, _ := fhirServerEnv(t, e, "")
				env.originator.cfg.ConformanceEnforcement = level
				s := newPrefetchSoR()
				env.originator.cfg.SoR = s.sor()
				body := sentCoverageRequest(strangerMember, e.base, sentBareCoverage(strangerMember, ref))
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(body))
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, noPayerIdentifier)
				if strings.Contains(rec.Body.String(), "send the payor Organization") {
					t.Fatalf("answer %s names the remedy for a payor that is not an Organization", rec.Body.String())
				}
				if got := paths(); len(got) != 0 {
					t.Fatalf("fhirServer read %v, want none", got)
				}
			})
			t.Run("questionnaire-package/"+ref+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				body := ehrParams(`{"name":"coverage","resource":`+sentBareCoverage(prefetchMember, ref)+`}`, dtrQuestionnaire)
				env, rec := dtrIngressRowAt(t, s, body, level)
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, noPayerIdentifier)
				if strings.Contains(rec.Body.String(), "send the payor Organization") {
					t.Fatalf("answer %s names the remedy for a payor that is not an Organization", rec.Body.String())
				}
			})
		}
	}
}

// A reference names an Organization only when the reference's own type is
// Organization: a relative reference's first segment (a leading slash
// dropped), an absolute URL's second-to-last path segment once its fragment
// and "/_history/<v>" are dropped; never "Organization" in a host or a base
// path.
func TestNamesOrganization(t *testing.T) {
	for ref, want := range map[string]bool{
		"Organization/o1": true, "https://ehr.example/fhir/Organization/o1": true, "Organization/o1/_history/2": true,
		"Organization/..": true, "RelatedPerson/r1": false, "Patient/p1": false, "urn:uuid:7c0e3b4a": false,
		"#contained": false, "Organization/": false, "": false,
		"https://Organization/fhir/Patient/p1": false, "Organization/o1#x": true, "/Organization/o1": true,
		"https://ehr.example/fhir/Organization/o1/_history/2": true, "https://ehr.example/fhir/Organization/o1#x": true,
		"https://ehr.example/Organization/fhir/Patient/p1": false, "https://Organization/o1": false,
		"https://ehr.example/fhir/Organization/": false, "Patient/p1/Organization/o1": false,
	} {
		if got := namesOrganization(ref); got != want {
			t.Errorf("%q: %t, want %t", ref, got, want)
		}
	}
}

// An absolute reference a Bundle entry's fullUrl may equal: an absolute URL
// ending "Organization/<id>", the id valid, with no "/_history/", query,
// fragment or dot segment anywhere (a fullUrl never contains "/_history/",
// bdl-8, and agrees with its resource's id).
func TestAbsoluteOrganizationRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"https://ehr.example/fhir/Organization/o1": true, "http://ehr.example/Organization/o-1.2": true,
		"Organization/o1": false, "/Organization/o1": false, "Organization/o1#x": false,
		"https://ehr.example/fhir/Organization/o1/_history/2": false, "https://ehr.example/fhir/Organization/o1#x": false,
		"https://ehr.example/fhir/Organization/o1?x=1": false, "https://ehr.example/fhir/../Organization/o1": false,
		"https://ehr.example/fhir/Organization/..": false, "https://ehr.example/fhir/Organization/.": false,
		"https://Organization/o1": false, "https:///Organization/o1": false, "https://ehr.example/fhir/Patient/p1": false,
		"https://ehr.example/fhir/Organization/": false, "urn:uuid:7c0e3b4a": false, "": false,
		"https://ehr.example/fhir?q=/Organization/o1": false, "https://ehr.example/fhir#/Organization/o1": false,
		"https://ehr.example/_history/Organization/o1": false,
	} {
		if got := absoluteOrganizationRef(ref); got != want {
			t.Errorf("%q: %t, want %t", ref, got, want)
		}
	}
}

// noPayerOrganizationNoIdentifier and noPayerNotAnOrganization are the
// refusals of a payor reference that resolved to a resource naming no payer
// identifier, pinned as partner-visible text (organizationMiss).
const (
	noPayerOrganizationNoIdentifier = noPayerIdentifier + ": the payor Organization carries no identifier with both a system and a value, such as a NAIC code or payer id"
	noPayerNotAnOrganization        = noPayerIdentifier + ": Coverage.payor references a resource that is not an Organization"
)

// A payor the request itself resolves, to a resource that names no payer
// identifier, was resolved: the refusal says why, as a PAS Bundle's does, and
// never names the remedy for a reference nothing could read. On
// $questionnaire-package and on CDS Hooks, at every level, before the
// network.
func TestIngressResolvedPayorWithoutIdentifierSaysWhy(t *testing.T) {
	const urn = "urn:uuid:7c0e3b4a-1f0d-4b55-9a1e-3c2f1d0e9b77"
	noID := `{"resourceType":"Organization","id":"o1","name":"Payer One"}`
	practitioner := `{"resourceType":"Practitioner","id":"p1"}`
	inBundle := func(fullURL, res string) string {
		return `{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"` + fullURL + `","resource":` + res + `}]}`
	}
	for name, row := range map[string]struct {
		ref, res, msg string
	}{
		"an Organization with no payer identifier":      {"Organization/o1", noID, noPayerOrganizationNoIdentifier},
		"an Organization by fullUrl with no identifier": {urn, inBundle(urn, noID), noPayerOrganizationNoIdentifier},
		"a resource that is not an Organization":        {urn, inBundle(urn, practitioner), noPayerNotAnOrganization},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			t.Run("questionnaire-package/"+name+"/"+level.String(), func(t *testing.T) {
				body := ehrParams(`{"name":"coverage","resource":`+sentBareCoverage(prefetchMember, row.ref)+`}`,
					`{"name":"referenced","resource":`+row.res+`}`, dtrQuestionnaire)
				env, rec := dtrIngressRowAt(t, newPrefetchSoR(), body, level)
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, row.msg)
			})
			t.Run("CDS Hooks/"+name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.SoR = newPrefetchSoR().sor()
				env.originator.cfg.ConformanceEnforcement = level
				body := sentCoverageRequest(prefetchMember, "", sentBareCoverage(prefetchMember, row.ref))
				body = bytes.Replace(body, []byte(`"coverage":`), []byte(`"payer":`+row.res+`,"coverage":`), 1)
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(body))
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, row.msg)
			})
		}
	}
}

// A payor that resolved to an Organization whose identifier this gateway
// reads but routing cannot (its id is not a string) gives no reason the
// gateway cannot name: the refusal is the bare text, on both ingresses, at
// every level, as a PAS Bundle's is.
func TestIngressResolvedPayorUnnamedReasonKeepsTheBareRefusal(t *testing.T) {
	const urn = "urn:uuid:7c0e3b4a-1f0d-4b55-9a1e-3c2f1d0e9b77"
	odd := `{"resourceType":"Organization","id":9,"identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}`
	bundle := `{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"` + urn + `","resource":` + odd + `}]}`
	bare := func(t *testing.T, env *inProcessExchange, rec *httptest.ResponseRecorder) {
		t.Helper()
		refusedWith(t, env, rec, http.StatusUnprocessableEntity, noPayerIdentifier)
		if strings.Contains(rec.Body.String(), noPayerIdentifier+":") {
			t.Fatalf("a reason the gateway cannot name is not guessed: %s", rec.Body.String())
		}
	}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		t.Run("CDS Hooks/"+level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = newPrefetchSoR().sor()
			env.originator.cfg.ConformanceEnforcement = level
			body := sentCoverageRequest(prefetchMember, "", sentBareCoverage(prefetchMember, urn))
			body = bytes.Replace(body, []byte(`"coverage":`), []byte(`"organizations":`+bundle+`,"coverage":`), 1)
			rec := httptest.NewRecorder()
			env.originator.handleCRDIngress(rec, crdIngressPost(body))
			bare(t, env, rec)
		})
		t.Run("questionnaire-package/"+level.String(), func(t *testing.T) {
			body := ehrParams(`{"name":"coverage","resource":`+sentBareCoverage(prefetchMember, urn)+`}`,
				`{"name":"referenced","resource":`+bundle+`}`, dtrQuestionnaire)
			env, rec := dtrIngressRowAt(t, newPrefetchSoR(), body, level)
			bare(t, env, rec)
		})
	}
}

// A Coverage the system of record supplied, whose payor that system resolves
// to an Organization with no payer identifier, or to a resource that is not
// an Organization, is refused with the reason, on CDS Hooks and
// $questionnaire-package, at every level.
func TestSystemCoveragePayorWithoutIdentifierSaysWhy(t *testing.T) {
	cov := strings.Replace(refCoverage(prefetchSoRID, "Organization/pay-9"), `"id":"c1"`, `"id":"cov-9"`, 1)
	for name, row := range map[string]struct{ res, msg string }{
		"an Organization with no payer identifier": {`{"resourceType":"Organization","id":"pay-9","name":"Payer"}`, noPayerOrganizationNoIdentifier},
		"a resource that is not an Organization":   {`{"resourceType":"Practitioner","id":"pay-9"}`, noPayerNotAnOrganization},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			sor := func() *prefetchSoR {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(cov)))
				s.reads["Organization/pay-9"] = []byte(row.res)
				return s
			}
			t.Run("CDS Hooks/"+name+"/"+level.String(), func(t *testing.T) {
				s := sor()
				env, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), false)
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, row.msg)
				if read := payorReads(s); len(read) != 1 {
					t.Fatalf("read %v, want the payor read once from the system of record", read)
				}
			})
			t.Run("questionnaire-package/"+name+"/"+level.String(), func(t *testing.T) {
				s := sor()
				env, rec := dtrPayorRow(t, s, level, false, dtrPayorParams(""))
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, row.msg)
			})
		}
	}
}

// A urn:uuid or urn:oid payor reference nothing resolves names the remedy
// that resolves it: the payor Organization as an entry, with that fullUrl, of
// a Bundle the request carries. Sending it so routes the request. On
// CDS Hooks and $questionnaire-package, at every level.
func TestSentBarePayorURNNamesTheFullURLRemedy(t *testing.T) {
	for _, ref := range []string{"urn:uuid:7c0e3b4a-1f0d-4b55-9a1e-3c2f1d0e9b77", "urn:oid:2.16.840.1.113883.3.9999"} {
		org := `{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"` + ref + `","resource":` + payorOrganization("o1", "00001") + `}]}`
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			t.Run("CDS Hooks/"+ref+"/"+level.String(), func(t *testing.T) {
				for _, sent := range []bool{false, true} {
					// A fhirServer and a system of record that would answer
					// are both present: a urn reference is read from neither.
					e, paths := recordingEHR(t, ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))
					env, _ := fhirServerEnv(t, e, "")
					env.originator.cfg.ConformanceEnforcement = level
					s := newPrefetchSoR()
					s.fhirBase = e.base // the system of record is at the request's fhirServer: it would be read
					s.reads["Organization/o1"] = []byte(payorOrganization("o1", "00001"))
					env.originator.cfg.SoR = s.sor()
					body := sentCoverageRequest(prefetchMember, e.base, sentBareCoverage(prefetchMember, ref))
					if sent {
						body = bytes.Replace(body, []byte(`"coverage":`), []byte(`"organizations":`+org+`,"coverage":`), 1)
					}
					rec := httptest.NewRecorder()
					env.originator.handleCRDIngress(rec, crdIngressPost(body))
					if !sent {
						refusedWith(t, env, rec, http.StatusUnprocessableEntity, noPayerUnresolvedURN)
					} else if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
						t.Fatalf("with the remedy applied: answer %d %s, %d sent", rec.Code, rec.Body.String(), env.routeHitCount())
					}
					if got := paths(); len(got) != 0 {
						t.Fatalf("fhirServer read %v for a urn reference", got)
					}
					if read := payorReads(s); len(read) != 0 {
						t.Fatalf("the system of record was read %v for a urn reference", read)
					}
				}
			})
			t.Run("questionnaire-package/"+ref+"/"+level.String(), func(t *testing.T) {
				cov := `{"name":"coverage","resource":` + sentBareCoverage(prefetchMember, ref) + `}`
				env, rec := dtrIngressRowAt(t, newPrefetchSoR(), ehrParams(cov, dtrQuestionnaire), level)
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, noPayerUnresolvedURN)
				env, rec = dtrIngressRowAt(t, newPrefetchSoR(), ehrParams(cov, `{"name":"referenced","resource":`+org+`}`, dtrQuestionnaire), level)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("with the remedy applied: answer %d %s, %d sent", rec.Code, rec.Body.String(), env.routeHitCount())
				}
			})
		}
	}
}

// The payor Organization sent beside a coverage that references it other than
// as "Organization/<id>" does not resolve it: the request's own resources
// resolve a reference by type and id only. On CDS Hooks (the Organization as
// another prefetch value) an absolute reference is refused naming the coverage
// Bundle's fullUrl entry, whether on another server or on fhirServer with the
// read off, and any other only a payor identifier; on $questionnaire-package
// (the Organization as another parameter) an absolute reference is refused
// naming a Bundle parameter's fullUrl entry, and any other a payor identifier
// only. At every level, before the network, with no read:
// never the remedy that sending the Organization would not satisfy.
func TestSentBarePayorWithTheOrganizationBesideAnUnreadReference(t *testing.T) {
	org := payorOrganization("o1", "00001")
	for name, row := range map[string]struct {
		mode string
		ref  func(base string) string
		want string
	}{
		"on another server": {ref: func(string) string { return "https://other.example/fhir/Organization/o1" }, want: noPayerUnreadRef},
		"on fhirServer, the read off": {mode: FHIRServerReadOff, ref: func(base string) string { return base + "/Organization/o1" },
			want: noPayerUnreadRef},
		"versioned on another server": {ref: func(string) string { return "https://other.example/fhir/Organization/o1/_history/2" },
			want: noPayerIdentifierOnlyRef},
		"a leading slash": {ref: func(string) string { return "/Organization/o1" }, want: noPayerIdentifierOnlyRef},
		"a fragment":      {ref: func(string) string { return "Organization/o1#x" }, want: noPayerIdentifierOnlyRef},
		"versioned":       {ref: func(string) string { return "Organization/o1/_history/2" }, want: noPayerIdentifierOnlyRef},
	} {
		for _, level := range allLevels {
			t.Run("CDS Hooks/"+name+"/"+level.String(), func(t *testing.T) {
				e, paths := recordingEHR(t, ehrAnswer(200, "application/fhir+json", org))
				env, _ := fhirServerEnv(t, e, row.mode)
				env.originator.cfg.ConformanceEnforcement = level
				s := newPrefetchSoR()
				env.originator.cfg.SoR = s.sor()
				body := sentCoverageRequest(strangerMember, e.base, sentBareCoverage(strangerMember, row.ref(e.base))+`,"payer":`+org)
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(body))
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, row.want)
				if got := paths(); len(got) != 0 {
					t.Fatalf("fhirServer read %v, want none", got)
				}
				if searched, read := s.calls(); len(read) != 0 || len(searched) != 0 {
					t.Fatalf("system of record read %v, searched %v; want neither", read, searched)
				}
			})
		}
	}
	for ref, want := range map[string]string{
		"https://other.example/fhir/Organization/o1": noPayerUnreadRef,
		"/Organization/o1":                           noPayerIdentifierOnlyRef,
		"Organization/o1#x":                          noPayerIdentifierOnlyRef,
		"Organization/o1/_history/2":                 noPayerIdentifierOnlyRef,
	} {
		for _, level := range allLevels {
			t.Run("questionnaire-package/"+ref+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				body := ehrParams(`{"name":"coverage","resource":`+sentBareCoverage(prefetchMember, ref)+`}`,
					`{"name":"payor","resource":`+org+`}`, dtrQuestionnaire)
				env, rec := dtrIngressRowAt(t, s, body, level)
				refusedWith(t, env, rec, http.StatusUnprocessableEntity, want)
				if searched, read := s.calls(); len(read) != 0 || len(searched) != 0 {
					t.Fatalf("system of record read %v, searched %v; want neither", read, searched)
				}
			})
		}
	}
}

// Each remedy resolves the reference it is given for, at every level, with no
// read: "Organization/<id>" by the payor Organization sent with the coverage
// (another prefetch value on CDS Hooks, another parameter on
// $questionnaire-package); an absolute reference by the coverage's Bundle
// holding the Organization as an entry whose fullUrl is that reference, on
// another server or on fhirServer with the read off. The EHR's coverage is
// carried exactly.
func TestSentBarePayorRemediesRoute(t *testing.T) {
	org := payorOrganization("o1", "00001")
	withEntry := func(ref string) string {
		return `{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"https://ehr.example/fhir/Coverage/c1","resource":` +
			sentBareCoverage(strangerMember, ref) + `,"search":{"mode":"match"}},{"fullUrl":"` + ref + `","resource":` + org +
			`,"search":{"mode":"include"}}]}`
	}
	for name, row := range map[string]struct {
		mode     string
		coverage func(base string) string
		extra    string
	}{
		"Organization/<id>, the Organization another value": {
			coverage: func(string) string { return sentBareCoverage(strangerMember, "Organization/o1") }, extra: `,"payer":` + org},
		"an absolute reference on another server, the coverage Bundle's entry": {
			coverage: func(string) string { return withEntry("https://other.example/fhir/Organization/o1") }},
		"an absolute reference on fhirServer, the read off, the coverage Bundle's entry": {mode: FHIRServerReadOff,
			coverage: func(base string) string { return withEntry(base + "/Organization/o1") }},
		"an absolute reference on another server, another value's Bundle entry": {
			coverage: func(string) string {
				return sentBareCoverage(strangerMember, "https://other.example/fhir/Organization/o1")
			},
			extra: `,"payer":{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://other.example/fhir/Organization/o1","resource":` +
				org + `}]}`},
	} {
		for _, level := range allLevels {
			t.Run("CDS Hooks/"+name+"/"+level.String(), func(t *testing.T) {
				e, paths := recordingEHR(t, ehrAnswer(200, "application/fhir+json", org))
				env, _ := fhirServerEnv(t, e, row.mode)
				env.originator.cfg.ConformanceEnforcement = level
				body := sentCoverageRequest(strangerMember, e.base, row.coverage(e.base)+row.extra)
				rec := httptest.NewRecorder()
				env.originator.handleCRDIngress(rec, crdIngressPost(body))
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				if got := paths(); len(got) != 0 {
					t.Fatalf("fhirServer read %v, want none", got)
				}
				want, _ := valueOf(t, body, "prefetch", "coverage")
				if got, ok := valueOf(t, sentRequest(t, env), "prefetch", "coverage"); !ok || got != want {
					t.Fatalf("carried prefetch.coverage %q, want the EHR's bytes %q", got, want)
				}
			})
		}
	}
	for name, row := range map[string]struct{ ref, beside string }{
		"Organization/<id>, the Organization another parameter": {ref: "Organization/o1", beside: `{"name":"payor","resource":` + org + `}`},
		"an absolute reference, a Bundle parameter's entry": {ref: "https://other.example/fhir/Organization/o1",
			beside: `{"name":"payor","resource":{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://other.example/fhir/Organization/o1","resource":` +
				org + `}]}}`},
	} {
		for _, level := range allLevels {
			t.Run("questionnaire-package/"+name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				body := ehrParams(`{"name":"coverage","resource":`+sentBareCoverage(prefetchMember, row.ref)+`}`,
					row.beside, dtrQuestionnaire)
				env, rec := dtrIngressRowAt(t, s, body, level)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				if searched, read := s.calls(); len(read) != 0 || len(searched) != 0 {
					t.Fatalf("system of record read %v, searched %v; want neither", read, searched)
				}
				if _, sent := sentOperation(t, env); !bytes.Equal(sent, body) {
					t.Fatalf("carried %s, want the EHR's bytes exactly", sent)
				}
			})
		}
	}
}
