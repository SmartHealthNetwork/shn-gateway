package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestDiscoveredLaneQualification(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "terminal failure"}[fail], func(t *testing.T) {
			v := shnsdk.NewFakeValidator()
			d := NewDiscoveredLane("2.1", "http://validator/fhir", v)
			if d.Ready() {
				t.Fatal("new lane is ready")
			}
			if _, err := d.Validate(context.Background(), []byte(`{"resourceType":"Patient"}`), ""); err == nil {
				t.Fatal("unqualified lane validated")
			}
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			q := func(ctx context.Context, base, line string) error {
				calls.Add(1)
				close(started)
				<-release
				if fail {
					return errors.New("negative control passed")
				}
				return nil
			}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := d.Qualify(context.Background(), q)
					if (err != nil) != fail {
						t.Errorf("qualification error=%v", err)
					}
				}()
			}
			<-started
			if d.Ready() {
				t.Fatal("warming lane ready")
			}
			close(release)
			wg.Wait()
			if d.Ready() == fail || calls.Load() != 1 {
				t.Fatalf("ready=%v calls=%d", d.Ready(), calls.Load())
			}
			_ = d.Qualify(context.Background(), q)
			if calls.Load() != 1 {
				t.Fatal("terminal qualifier retried")
			}
		})
	}
}

func TestUnavailableDefaultsCannotRouteOrHonorFrames(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	canonical := shnsdk.NewFakeValidator()
	d := NewDiscoveredLane("2.1", server.URL, shnsdk.NewOperationValidator(server.URL))
	g := d9Gateway(nil, nil)
	g.cfg.ValidatorsByContractLine = ContractLineLanes(map[string]shnsdk.Validator{"2.0": canonical, "2.1": canonical},
		map[string]*DiscoveredLane{"2.1": d}, map[string]bool{"2.1": true}, nil)
	for i := 0; i < 100; i++ {
		if _, ok := g.selectNativeReachRoute("pa.dtr", map[string]bool{"2.1": true}, ""); ok {
			t.Fatal("unavailable default entered routing")
		}
		// An unqualified lane is never answered at: the claim is carried as
		// a bare request is (the participant's own setting refuses it).
		_, tok, carried, status, _ := g.unframeRequestFrom("requester", "dtr-questionnaire-fetch", framedRequest(t, "pa.dtr@2.1", []byte(`{}`)))
		if status != 0 || tok == "pa.dtr@2.1" || carried != "pa.dtr@2.1" {
			t.Fatalf("unqualified frame = (%d, %q, carried %q), want carried and not answered at its line", status, tok, carried)
		}
		_, tok, _, status, _ = g.unframeRequestFrom("requester", "pas-claim", framedRequest(t, "pa.pas@2.0", []byte(`{}`)))
		if status != 0 || tok != "pa.pas@2.0" {
			t.Fatalf("canonical frame = (%d, %q), want answered at pa.pas@2.0", status, tok)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("lookup sent %d HTTP requests", requests.Load())
	}
	if err := d.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.selectNativeReachRoute("pa.dtr", map[string]bool{"2.1": true}, ""); !ok {
		t.Fatal("qualified default not routable")
	}
}

// singleLineStandIn is a contract declared at one line and built natively at
// none: no native contract is single-line, so the single-line rule is pinned
// through ContractLineLanes's declared argument.
const singleLineStandIn = "x.single@2.1"

func TestContractLaneFallbackCollision(t *testing.T) {
	canonical, explicit := shnsdk.NewFakeValidator(), shnsdk.NewFakeValidator()
	d := NewDiscoveredLane("2.1", "http://unused.invalid/fhir", explicit)
	g := &Gateway{cfg: Config{Validator: canonical, ValidatorsByContractLine: ContractLineLanes(map[string]shnsdk.Validator{"2.0": canonical, "2.1": canonical},
		map[string]*DiscoveredLane{"2.1": d}, map[string]bool{"2.1": true}, []string{singleLineStandIn})}}
	if g.validatorForContractLine("pa.dtr", "2.1") != nil {
		t.Fatal("PA borrowed the single-contract fallback")
	}
	if status, _ := g.validateFHIRForContract(context.Background(), []byte(`{"resourceType":"Patient"}`), "egress", "pa.dtr", "2.1", ""); status != http.StatusInternalServerError {
		t.Fatal("PA validated through the single-line contract's fallback")
	}
	if status, _ := g.validateFHIRForContract(context.Background(), []byte(`{"resourceType":"Patient"}`), "egress", "x.single", "2.1", ""); status != 0 {
		t.Fatal("single-line canonical validation refused")
	}
	if g.validatorForContractLine("x.single", "2.1") != canonical {
		t.Fatal("single-line canonical alias lost")
	}
	// pa.pdex is no contract this gateway builds: it has no lane, alias or not.
	if g.validatorForContractLine("pa.pdex", "2.1") != nil {
		t.Fatal("pa.pdex resolved a lane")
	}
	if g.validatorForContractLine("pa.pas", "2.0") != canonical {
		t.Fatal("configured canonical unavailable")
	}
	if err := d.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if g.validatorForContractLine("pa.dtr", "2.1") != d {
		t.Fatal("qualified default missing")
	}
	if g.validatorForContractLine("x.single", "2.1") != canonical {
		t.Fatal("qualified PA default replaced the single-line fallback")
	}
	// An explicit 2.1 lane, with no alias, serves every contract at 2.1.
	g.cfg.ValidatorsByContractLine = ContractLineLanes(map[string]shnsdk.Validator{"2.0": canonical, "2.1": explicit},
		map[string]*DiscoveredLane{"2.1": d}, nil, []string{singleLineStandIn})
	if g.validatorForContractLine("pa.dtr", "2.1") != explicit || g.validatorForContractLine("x.single", "2.1") != explicit {
		t.Fatal("override did not win")
	}
}

func TestDefaultLaneURL(t *testing.T) {
	if got := DefaultLaneURL("2.1"); got != "http://shn-validator-2-1:8080/fhir" {
		t.Fatal(got)
	}
	if got := DefaultLaneURL("2.2"); got != "http://shn-validator-2-2:8080/fhir" {
		t.Fatal(got)
	}
}

func TestValidationVerdictDoesNotChangeDefaultReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "validator unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	d := NewDiscoveredLane("2.1", server.URL, shnsdk.NewOperationValidator(server.URL))
	if err := d.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Validate(context.Background(), []byte(`{"resourceType":"Patient"}`), ""); err == nil {
		t.Fatal("expected call error")
	}
	if !d.Ready() {
		t.Fatal("ordinary call verdict changed synthetic readiness")
	}
}

func TestDTRContextCannotUseSingleContractFallback(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()
	g := &Gateway{cfg: Config{Validator: canonical, ValidatorsByContractLine: ContractLineLanes(map[string]shnsdk.Validator{"2.0": canonical, "2.1": canonical}, nil, map[string]bool{"2.1": true}, nil)}}
	if status, _ := g.validateDTRQuestionnaireResponse(context.Background(), []byte(`{"resourceType":"QuestionnaireResponse"}`), "2.1"); status != http.StatusInternalServerError {
		t.Fatalf("DTR context status=%d; unavailable PA lane borrowed the single-line fallback", status)
	}
	// A payer's DTR answer routed at 2.1 is not certified on the borrowed fallback
	// either: 2.1 is no candidate line, and the answer is judged on the one DTR lane
	// this build really has.
	lanes := g.candidateLanes("pa.dtr", "2.1", []byte(`{"resourceType":"Patient"}`))
	if len(lanes) != 2 || lanes[0].Line != "2.1" || lanes[0].V != nil || lanes[1].Line != "2.0" || lanes[1].V == nil {
		t.Fatalf("DTR ingress candidate lanes=%+v, want 2.1 unserved and the real 2.0 lane", lanes)
	}
	// At the call: the borrowed 2.1 fallback is never called, and with the routed
	// line unserved and 2.0 failing, the answer is refused as the missing lane.
	reg := shnsdk.NewRegistry()
	g2 := &Gateway{cfg: Config{Reg: reg, ValidatorsByContractLine: ContractLineLanes(map[string]shnsdk.Validator{
		"2.0": &recordingValidator{valid: false}, "2.1": failIfCalledValidator{t},
	}, nil, map[string]bool{"2.1": true}, nil)}}
	status, msg := g2.validateFHIRPayerIngress(context.Background(), []byte(`{"resourceType":"Patient"}`), "2.1", "pa.dtr",
		shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "00002"})
	if status != http.StatusInternalServerError || !strings.Contains(msg, "no FHIR validator lane configured for contract line 2.1") {
		t.Fatalf("DTR ingress status=%d %q, want the unserved 2.1 line refused as unavailable", status, msg)
	}
	if status, _ := g.validateFHIR(context.Background(), []byte(`{"resourceType":"Patient"}`), "egress", ""); status != 0 {
		t.Fatalf("generic canonical status=%d", status)
	}
}

// The lane shape a gateway with no default validator lanes has with a
// single-line contract declared: 2.0 on the canonical validator, and 2.1 on it
// only as the single-line contract's fallback alias. PA traffic at 2.1 has no
// lane and is never answered at it; the single-line contract at 2.1 keeps its
// alias.
func TestNoDefaultLanesShapeRefusesPAAtTheAliasedLine(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()
	g := &Gateway{cfg: Config{Reg: shnsdk.NewRegistry(), Validator: canonical, ValidatorsByContractLine: ContractLineLanes(map[string]shnsdk.Validator{"2.0": canonical, "2.1": canonical}, nil, map[string]bool{"2.1": true}, []string{singleLineStandIn})}}
	for _, leg := range []struct{ legType, token string }{{"pas-claim", "pa.pas@2.1"}, {"crd-order-select", "pa.crd@2.1"}} {
		if _, tok, carried, _, _ := g.unframeRequestFrom("requester", leg.legType, framedRequest(t, leg.token, []byte(`{}`))); tok == leg.token || carried != leg.token {
			t.Errorf("%s frame answered at %q (carried %q); a line with only an alias has no PA lane", leg.token, tok, carried)
		}
	}
	for _, contract := range []string{"pa.pas", "pa.crd", "pa.dtr"} {
		if g.validatorForContractLine(contract, "2.1") != nil {
			t.Errorf("%s@2.1 borrowed the canonical alias", contract)
		}
	}
	if g.validatorForContractLine("x.single", "2.1") == nil {
		t.Error("the single-line contract lost its canonical alias")
	}
}
