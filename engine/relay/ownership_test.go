package relay

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pinnedOwnership is a literal copy of the ownership table. Any change to
// the table must change this copy too, so it shows up as a reviewed diff.
func pinnedOwnership() map[Key]Rule {
	R, E, A := OwnershipRelayed, OwnershipEdited, OwnershipAuthored
	own := func(o ...Ownership) []Ownership { return o }
	b := func(ids ...BuilderID) []BuilderID { return ids }
	req, resp := DirectionRequest, DirectionResponse
	rq, rc := RoleRequester, RoleRecipient
	refusal := Rule{Allowed: own(A), Builders: b("gateway-refusal")}
	relayOnly := Rule{Allowed: own(R)}
	m := map[Key]Rule{
		{"crd-order-dispatch", rq, req, OutcomeCarried}:      {Allowed: own(R, E), Edits: []EditID{"E-01", "E-02"}},
		{"crd-order-select", rq, req, OutcomeCarried}:        {Allowed: own(R, E), Edits: []EditID{"E-01", "E-02"}},
		{"dtr-questionnaire-fetch", rq, req, OutcomeCarried}: {Allowed: own(R, E), Edits: []EditID{"E-04", "E-05"}},
		{"pas-claim", rq, req, OutcomeCarried}:               relayOnly,
		{"pas-claim-inquire", rq, req, OutcomeCarried}:       relayOnly,
		{"pas-claim-update", rq, req, OutcomeCarried}:        relayOnly,

		{"coverage-eligibility", rq, req, OutcomeOriginated}:    {Allowed: own(A), Builders: b("sdk-eligibility")},
		{"crd-order-dispatch", rq, req, OutcomeOriginated}:      {Allowed: own(A), Builders: b("sdk-crd-request")},
		{"crd-order-select", rq, req, OutcomeOriginated}:        {Allowed: own(A), Builders: b("sdk-crd-request")},
		{"dtr-questionnaire-fetch", rq, req, OutcomeOriginated}: {Allowed: own(A), Builders: b("sdk-dtr-package", "dtr-next-question")},
		{"federated-query", rq, req, OutcomeOriginated}:         {Allowed: own(A), Builders: b("sdk-federated-query")},
		{"patient-dtr", rq, req, OutcomeOriginated}:             {Allowed: own(A), Builders: b("sdk-patient-dtr")},
		{"pas-claim", rq, req, OutcomeOriginated}:               {Allowed: own(A), Builders: b("sdk-pas-submit")},
		{"pas-claim-inquire", rq, req, OutcomeOriginated}:       {Allowed: own(A), Builders: b("sdk-pas-inquiry")},
		{"pas-claim-update", rq, req, OutcomeOriginated}:        {Allowed: own(A), Builders: b("sdk-pas-update")},

		{"crd-order-dispatch", rq, resp, OutcomeAnswered}:      relayOnly,
		{"crd-order-select", rq, resp, OutcomeAnswered}:        relayOnly,
		{"dtr-questionnaire-fetch", rq, resp, OutcomeAnswered}: relayOnly,
		{"pas-claim", rq, resp, OutcomeAnswered}:               relayOnly,
		{"pas-claim-inquire", rq, resp, OutcomeAnswered}:       relayOnly,
		{"pas-claim-update", rq, resp, OutcomeAnswered}:        relayOnly,

		{"coverage-eligibility", rq, resp, OutcomeUpstreamError}:    relayOnly,
		{"crd-order-dispatch", rq, resp, OutcomeUpstreamError}:      relayOnly,
		{"crd-order-select", rq, resp, OutcomeUpstreamError}:        relayOnly,
		{"dtr-questionnaire-fetch", rq, resp, OutcomeUpstreamError}: relayOnly,
		{"federated-query", rq, resp, OutcomeUpstreamError}:         relayOnly,
		{"patient-dtr", rq, resp, OutcomeUpstreamError}:             relayOnly,
		{"pas-claim", rq, resp, OutcomeUpstreamError}:               relayOnly,
		{"pas-claim-inquire", rq, resp, OutcomeUpstreamError}:       relayOnly,
		{"pas-claim-update", rq, resp, OutcomeUpstreamError}:        relayOnly,

		{"crd-order-dispatch", rc, req, OutcomeCarried}:      {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"crd-order-select", rc, req, OutcomeCarried}:        {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"dtr-questionnaire-fetch", rc, req, OutcomeCarried}: {Allowed: own(R, E, A), Edits: []EditID{"E-03"}, Builders: b("defect-dtr-projection")},
		{"pas-claim", rc, req, OutcomeCarried}:               {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"pas-claim-inquire", rc, req, OutcomeCarried}:       {Allowed: own(R, E), Edits: []EditID{"E-03"}},
		{"pas-claim-update", rc, req, OutcomeCarried}:        {Allowed: own(R, E), Edits: []EditID{"E-03"}},

		{"coverage-eligibility", rc, resp, OutcomeAnswered}:    {Allowed: own(A), Builders: b("sdk-eligibility")},
		{"crd-order-dispatch", rc, resp, OutcomeAnswered}:      relayOnly,
		{"crd-order-select", rc, resp, OutcomeAnswered}:        relayOnly,
		{"dtr-questionnaire-fetch", rc, resp, OutcomeAnswered}: relayOnly,
		{"federated-query", rc, resp, OutcomeAnswered}:         {Allowed: own(A), Builders: b("cdex-fulfillment")},
		{"patient-dtr", rc, resp, OutcomeAnswered}:             {Allowed: own(A), Builders: b("sdk-patient-dtr")},
		{"pas-claim", rc, resp, OutcomeAnswered}:               {Allowed: own(R)},
		{"pas-claim-inquire", rc, resp, OutcomeAnswered}:       relayOnly,
		{"pas-claim-update", rc, resp, OutcomeAnswered}:        {Allowed: own(R)},

		{"crd-order-dispatch", rc, resp, OutcomeUpstreamError}:      {Allowed: own(R, A), Builders: b("defect-empty-error-substitution")},
		{"crd-order-select", rc, resp, OutcomeUpstreamError}:        {Allowed: own(R, A), Builders: b("defect-empty-error-substitution")},
		{"dtr-questionnaire-fetch", rc, resp, OutcomeUpstreamError}: {Allowed: own(R, A), Builders: b("defect-empty-error-substitution")},
		{"pas-claim", rc, resp, OutcomeUpstreamError}:               {Allowed: own(R, A), Builders: b("defect-empty-error-substitution")},
		{"pas-claim-inquire", rc, resp, OutcomeUpstreamError}:       {Allowed: own(R, A), Builders: b("defect-empty-error-substitution")},
		{"pas-claim-update", rc, resp, OutcomeUpstreamError}:        {Allowed: own(R, A), Builders: b("defect-empty-error-substitution")},
	}
	for _, leg := range []string{"", "coverage-eligibility", "crd-order-dispatch", "crd-order-select",
		"dtr-questionnaire-fetch", "federated-query", "patient-dtr", "pas-claim", "pas-claim-inquire", "pas-claim-update"} {
		m[Key{leg, rq, resp, OutcomeRefused}] = refusal
		m[Key{leg, rc, resp, OutcomeRefused}] = refusal
	}
	return m
}

func ruleEqual(a, b Rule) bool {
	return slices.Equal(a.Allowed, b.Allowed) && slices.Equal(a.Edits, b.Edits) && slices.Equal(a.Builders, b.Builders)
}

func TestLegOwnershipTablePinned(t *testing.T) {
	got, want := LegOwnership(), pinnedOwnership()
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("missing %s", k)
			continue
		}
		if !ruleEqual(g, w) {
			t.Errorf("%s: got %+v, want %+v", k, g, w)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unpinned %s: %+v", k, got[k])
		}
	}
}

// TestLegOwnershipTableShape checks properties every row must have.
func TestLegOwnershipTableShape(t *testing.T) {
	if !slices.IsSorted(Legs()) || len(Legs()) == 0 {
		t.Fatalf("legs %v must be sorted", Legs())
	}
	registered := Builders()
	used := map[BuilderID]bool{}
	for k, r := range LegOwnership() {
		if k.Leg != LegUnestablished && !slices.Contains(Legs(), k.Leg) {
			t.Errorf("%s: unknown leg", k)
		}
		if k.Role != RoleRequester && k.Role != RoleRecipient {
			t.Errorf("%s: invalid role", k)
		}
		if !k.Outcome.validFor(k.Direction) {
			t.Errorf("%s: outcome does not fit the direction", k)
		}
		if k.Leg == LegUnestablished && k.Outcome != OutcomeRefused {
			t.Errorf("%s: only refusals precede an established leg", k)
		}
		if len(r.Allowed) == 0 {
			t.Errorf("%s: permits nothing", k)
		}
		for _, o := range r.Allowed {
			if o < OwnershipRelayed || o > OwnershipAuthored {
				t.Errorf("%s: invalid ownership %v", k, o)
			}
		}
		if slices.Contains(r.Allowed, OwnershipEdited) != (len(r.Edits) > 0) {
			t.Errorf("%s: edits listed without Edited, or Edited without edits", k)
		}
		if slices.Contains(r.Allowed, OwnershipAuthored) != (len(r.Builders) > 0) {
			t.Errorf("%s: builders listed without Authored, or Authored without builders", k)
		}
		for _, e := range r.Edits {
			if !slices.Contains(EditIDs(), e) {
				t.Errorf("%s: unregistered edit %s", k, e)
			}
		}
		for _, id := range r.Builders {
			if !slices.Contains(registered, id) {
				t.Errorf("%s: unregistered builder %s", k, id)
			}
			if id == builderTestInjected {
				t.Errorf("%s: the test builder is never listed", k)
			}
			used[id] = true
		}
		// A refusal is always the gateway's own; nothing else is.
		if (k.Outcome == OutcomeRefused) != slices.Contains(r.Builders, BuilderGatewayRefusal) {
			t.Errorf("%s: the refusal builder belongs to refusals only", k)
		}
		if k.Outcome == OutcomeRefused && !(len(r.Allowed) == 1 && len(r.Builders) == 1) {
			t.Errorf("%s: a refusal is only the gateway's own refusal", k)
		}
	}
	// Every interim builder is listed somewhere: an interim builder no
	// transmit permits could never send, so it would only hide a stale id.
	for _, id := range interimBuilders {
		if !used[id] {
			t.Errorf("interim builder %s is listed on no transmit", id)
		}
	}
	// The table copy is independent of the table.
	c := LegOwnership()
	for k, r := range c {
		if len(r.Builders) > 0 {
			r.Builders[0] = "changed"
			c[k] = r
			break
		}
	}
	if maps.EqualFunc(c, LegOwnership(), ruleEqual) {
		t.Fatal("LegOwnership shares storage with the table")
	}
}

func TestKeyAndEnumText(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{Key{"pas-claim", RoleRecipient, DirectionResponse, OutcomeAnswered}.String(), "pas-claim/recipient/response/answered"},
		{Key{"", RoleRequester, DirectionResponse, OutcomeRefused}.String(), "(no leg)/requester/response/refused"},
		{Key{"x", RoleRequester, DirectionRequest, OutcomeCarried}.String(), "x/requester/request/carried"},
		{OutcomeOriginated.String(), "originated"},
		{OutcomeUpstreamError.String(), "upstream-error"},
		{Role(9).String(), "Role(9)"},
		{Direction(9).String(), "Direction(9)"},
		{Outcome(9).String(), "Outcome(9)"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if Outcome(9).validFor(DirectionRequest) || OutcomeCarried.validFor(Direction(0)) {
		t.Fatal("invalid outcome accepted")
	}
}

// checkRows are the table-check rows, run against a small table so each
// refusal has a baseline that is admitted.
func TestCheckRows(t *testing.T) {
	edited := Key{"leg-e", RoleRecipient, DirectionRequest, OutcomeCarried}
	authoredKey := Key{"leg-a", RoleRequester, DirectionRequest, OutcomeOriginated}
	relayKey := Key{"leg-r", RoleRecipient, DirectionResponse, OutcomeAnswered}
	table := map[Key]Rule{
		edited:      {Allowed: []Ownership{OwnershipRelayed, OwnershipEdited}, Edits: []EditID{EditPayorEdgeRestamp}},
		authoredKey: {Allowed: []Ownership{OwnershipAuthored}, Builders: []BuilderID{BuilderSDKPASSubmit}},
		relayKey:    {Allowed: []Ownership{OwnershipRelayed}},
	}
	body := NewBody([]byte(`{"payor":"a","hook":"order-sign"}`), OriginPeerFrame)
	d := mustDoc(t, body)
	restamp, err := Apply(body, fhirJSON, EditPayorEdgeRestamp, d.Replace(at(t, d, "payor"), []byte(`"b"`)))
	if err != nil {
		t.Fatal(err)
	}
	strip, err := Apply(body, fhirJSON, EditCDSCallbackStrip, d.RemoveMember(d.Root(), "hook"))
	if err != nil {
		t.Fatal(err)
	}
	both, err := ApplyChanges(body, fhirJSON,
		Change{Edit: EditPayorEdgeRestamp, Ops: []Op{d.Replace(at(t, d, "payor"), []byte(`"b"`))}},
		Change{Edit: EditCDSCallbackStrip, Ops: []Op{d.RemoveMember(d.Root(), "hook")}})
	if err != nil {
		t.Fatal(err)
	}
	submit, err := Authored(BuilderSDKPASSubmit, []byte(`{}`), fhirJSON)
	if err != nil {
		t.Fatal(err)
	}
	update, err := Authored(BuilderSDKPASUpdate, []byte(`{}`), fhirJSON)
	if err != nil {
		t.Fatal(err)
	}
	interim, err := Authored(BuilderInterimDTRProjection, []byte(`{}`), fhirJSON)
	if err != nil {
		t.Fatal(err)
	}
	exact := Exact(body, fhirJSON)
	injected := ForTest([]byte(`{"x":1}`), fhirJSON)
	unknown := Key{"leg-z", RoleRecipient, DirectionResponse, OutcomeAnswered}

	for _, r := range []struct {
		name  string
		key   Key
		p     Payload
		admit bool
	}{
		{"relayed at a relay transmit", relayKey, exact, true},
		{"relayed where edits are permitted", edited, exact, true},
		{"permitted edit", edited, restamp, true},
		{"registered builder", authoredKey, submit, true},
		{"test payload on any listed key", relayKey, injected, true},
		{"test payload on an authored key", authoredKey, injected, true},
		{"test payload on an edited key", edited, injected, true},
		{"zero payload", relayKey, Payload{}, false},
		{"unknown key", unknown, exact, false},
		{"test payload on an unknown key", unknown, injected, false},
		{"authored at a relay transmit", relayKey, submit, false},
		{"interim builder where not listed", relayKey, interim, false},
		{"interim builder at an authored transmit that does not list it", authoredKey, interim, false},
		{"builder not listed", authoredKey, update, false},
		{"relayed at an authored transmit", authoredKey, exact, false},
		{"edit not listed", edited, strip, false},
		{"one of two edits not listed", edited, both, false},
		{"edited at a relay transmit", relayKey, restamp, false},
		{"edited at an authored transmit", authoredKey, restamp, false},
	} {
		t.Run(r.name, func(t *testing.T) {
			err := checkIn(table, r.key)(r.p)
			if r.admit {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var oe *OwnershipError
			if !errors.Is(err, ErrOwnershipRefused) || !errors.As(err, &oe) || oe.Key != r.key {
				t.Fatalf("want an *OwnershipError for %s, got %v", r.key, err)
			}
			if _, terr := Transmit(r.p, checkIn(table, r.key)); terr == nil {
				t.Fatal("Transmit sent a refused payload")
			}
		})
	}

	// An edited payload that names no edit cannot be built; the check
	// refuses one anyway.
	forged := Payload{c: &sealed{b: seal([]byte(`{}`)), own: OwnershipEdited}}
	if err := checkIn(table, edited)(forged); !errors.Is(err, ErrOwnershipRefused) {
		t.Fatalf("edited payload without edits: %v", err)
	}
}

func TestCheckRefusesTestPayloadOutsideTests(t *testing.T) {
	p := ForTest([]byte(`{}`), fhirJSON)
	old := testBinary
	testBinary = func() bool { return false }
	defer func() { testBinary = old }()
	k := Key{"pas-claim", RoleRecipient, DirectionResponse, OutcomeAnswered}
	err := Check(k)(p)
	if !errors.Is(err, ErrOwnershipRefused) || !strings.Contains(err.Error(), "only in tests") {
		t.Fatalf("got %v", err)
	}
	for name, f := range map[string]func(){
		"ForTest":      func() { ForTest(nil, fhirJSON) },
		"BytesForTest": func() { BytesForTest(p) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic outside tests", name)
				}
			}()
			f()
		}()
	}
}

func TestOwnershipErrorText(t *testing.T) {
	body := NewBody([]byte(`{"payor":"a"}`), OriginPeerFrame)
	d := mustDoc(t, body)
	restamp, err := Apply(body, fhirJSON, EditPayorEdgeRestamp, d.Replace(at(t, d, "payor"), []byte(`"b"`)))
	if err != nil {
		t.Fatal(err)
	}
	k := Key{"pas-claim", RoleRecipient, DirectionResponse, OutcomeAnswered}
	err = Check(k)(restamp)
	want := "relay: edited[E-03] payload not permitted at pas-claim/recipient/response/answered: ownership not permitted"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
	refusalPayload, _ := Authored(BuilderGatewayRefusal, []byte(`{}`), "application/json")
	err = Check(k)(refusalPayload)
	if err == nil || !strings.Contains(err.Error(), "authored gateway-refusal payload") {
		t.Fatalf("got %v", err)
	}
	if err := Check(k)(Payload{}); err == nil || !strings.Contains(err.Error(), "unset payload") {
		t.Fatalf("got %v", err)
	}
}

func TestForTest(t *testing.T) {
	src := []byte(`{"resourceType":"Bundle"}`)
	p := ForTest(src, fhirJSON)
	src[0] = 'X'
	if p.Ownership() != OwnershipAuthored || p.Builder() != builderTestInjected || p.ContentType() != fhirJSON {
		t.Fatalf("payload %v", p)
	}
	got := BytesForTest(p)
	if string(got) != `{"resourceType":"Bundle"}` {
		t.Fatalf("bytes %q", got)
	}
	got[0] = 'Y'
	if string(BytesForTest(p)) != `{"resourceType":"Bundle"}` {
		t.Fatal("BytesForTest shares storage with the payload")
	}
	if BytesForTest(Payload{}) != nil {
		t.Fatal("zero payload has bytes")
	}
	// A reserved id cannot be reached through Authored, only through ForTest.
	if _, err := Authored(p.Builder(), []byte(`{}`), fhirJSON); !errors.Is(err, ErrReservedBuilder) {
		t.Fatalf("Authored accepted the reserved id: %v", err)
	}
	out, err := Transmit(p, Check(Key{"pas-claim", RoleRequester, DirectionRequest, OutcomeCarried}))
	if err != nil || string(out) != `{"resourceType":"Bundle"}` {
		t.Fatalf("transmit %q %v", out, err)
	}
}

// TestReservedBuilderIDRefusedByAuthored: the id reserved for tests is
// refused by Authored whatever its spelling source, and is not a registered
// builder.
func TestReservedBuilderIDRefusedByAuthored(t *testing.T) {
	for _, id := range []BuilderID{builderTestInjected, BuilderID("test-" + "injected")} {
		p, err := Authored(id, []byte(`{}`), fhirJSON)
		if !errors.Is(err, ErrReservedBuilder) || p.Ownership() != 0 {
			t.Fatalf("%q: %v %v", id, p, err)
		}
	}
	if slices.Contains(Builders(), builderTestInjected) {
		t.Fatal("the reserved id is registered")
	}
	// Near spellings are simply unknown builders.
	for _, id := range []BuilderID{"Test-Injected", "test-injected ", "test_injected"} {
		if _, err := Authored(id, []byte(`{}`), fhirJSON); !errors.Is(err, ErrUnknownBuilder) {
			t.Fatalf("%q: %v", id, err)
		}
	}
}

const forTestProbe = `package main

import (
	"os"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

func main() {
	switch os.Args[1] {
	case "fortest":
		_ = relay.ForTest([]byte("{}"), "application/json")
	case "bytesfortest":
		_ = relay.BytesForTest(relay.Payload{})
	}
	os.Stdout.WriteString("returned\n")
}
`

// TestForTestAdmittedOnlyInTests builds an ordinary program (not a test
// binary) that calls ForTest and BytesForTest, and requires both calls to
// panic. The build uses only the local module cache.
func TestForTestAdmittedOnlyInTests(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("go tool not available: the subprocess row cannot run")
	}
	module, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(module, "go.mod")); err != nil {
		t.Fatalf("module root: %v", err)
	}
	dir := t.TempDir()
	gomod := "module fortestprobe\n\ngo 1.26.0\n\nrequire github.com/SmartHealthNetwork/shn-gateway v0.0.0\n\n" +
		"replace github.com/SmartHealthNetwork/shn-gateway => " + filepath.ToSlash(module) + "\n"
	for name, content := range map[string]string{"go.mod": gomod, "main.go": forTestProbe} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "probe")
	build := exec.Command(gobin, "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, out)
	}
	for _, c := range []struct{ arg, msg string }{
		{"fortest", "relay: ForTest is available only in tests"},
		{"bytesfortest", "relay: BytesForTest is available only in tests"},
	} {
		out, err := exec.Command(bin, c.arg).CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() == 0 {
			t.Fatalf("%s: want a failing exit, got %v\n%s", c.arg, err, out)
		}
		if !strings.Contains(string(out), "panic: "+c.msg) || strings.Contains(string(out), "returned") {
			t.Fatalf("%s: want a panic, got\n%s", c.arg, out)
		}
	}
}
