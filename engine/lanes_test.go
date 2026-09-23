package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestDiscoveredLaneQualification(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "terminal failure"}[fail], func(t *testing.T) {
			v := syntheticFakeValidator()
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

func TestUnavailableDefaultsCannotCertifyButCarryNativeFrames(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	canonical := syntheticFakeValidator()
	d := NewDiscoveredLane("2.1", server.URL, shnsdk.NewOperationValidator(server.URL))
	g := d9Gateway(map[string]shnsdk.Validator{"2.0": canonical, "2.1": canonical}, nil)
	g.cfg.CanonicalFallbackLines = map[string]bool{"2.1": true}
	g.cfg.DefaultValidatorsByLine = map[string]*DiscoveredLane{"2.1": d}
	for i := 0; i < 100; i++ {
		if _, ok := g.selectNativeReachRoute("pa.dtr", map[string]bool{"2.1": true}); !ok {
			t.Fatal("native capability incorrectly depends on optional checker qualification")
		}
		if g.validatorForContractLine("pa.dtr", "2.1") != nil {
			t.Fatal("unqualified checker became certification evidence")
		}
		_, _, status, _ := g.unframeRequest("dtr-questionnaire-fetch", framedRequest(t, "pa.dtr@2.1", []byte(`{}`)))
		if status != 0 {
			t.Fatalf("native frame blocked by unqualified checker: status=%d", status)
		}
		_, _, status, _ = g.unframeRequest("pas-claim", framedRequest(t, "pa.pas@2.0", []byte(`{}`)))
		if status != 0 {
			t.Fatalf("canonical frame status=%d", status)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("lookup sent %d HTTP requests", requests.Load())
	}
	if err := d.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.selectNativeReachRoute("pa.dtr", map[string]bool{"2.1": true}); !ok {
		t.Fatal("qualified default not routable")
	}
}

func TestContractLaneFallbackCollision(t *testing.T) {
	canonical, explicit := syntheticFakeValidator(), syntheticFakeValidator()
	d := NewDiscoveredLane("2.1", "http://unused.invalid/fhir", explicit)
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementStrict, Validator: canonical, ValidatorsByLine: map[string]shnsdk.Validator{"2.0": canonical, "2.1": canonical}, CanonicalFallbackLines: map[string]bool{"2.1": true}, DefaultValidatorsByLine: map[string]*DiscoveredLane{"2.1": d}}}
	if g.validatorForContractLine("pa.dtr", "2.1") != nil {
		t.Fatal("PA borrowed the single-contract fallback")
	}
	if status, _ := g.validateFHIRForContract(context.Background(), []byte(`{"resourceType":"Patient"}`), "egress", "pa.dtr", "2.1", ""); status != http.StatusServiceUnavailable {
		t.Fatal("PA validated through PDex compatibility fallback")
	}
	if status, _ := g.validateFHIRForContract(context.Background(), []byte(`{"resourceType":"Patient"}`), "egress", "pa.pdex", "2.1", ""); status != 0 {
		t.Fatal("PDex canonical validation refused")
	}
	if g.validatorForContractLine("pa.pdex", "2.1") != canonical {
		t.Fatal("PDex canonical compatibility lost")
	}
	if g.validatorForContractLine("pa.pas", "2.0") != canonical {
		t.Fatal("configured canonical unavailable")
	}
	if err := d.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if g.validatorForContractLine("pa.dtr", "2.1") != d || g.validatorForLine("2.1") != d {
		t.Fatal("qualified default missing")
	}
	if g.validatorForContractLine("pa.pdex", "2.1") != canonical {
		t.Fatal("qualified PA default replaced PDex fallback")
	}
	g.cfg.CanonicalFallbackLines = nil
	g.cfg.ValidatorsByLine["2.1"] = explicit
	if g.validatorForContractLine("pa.dtr", "2.1") != explicit || g.validatorForContractLine("pa.pdex", "2.1") != explicit {
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
	canonical := syntheticFakeValidator()
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementStrict, Validator: canonical, ValidatorsByLine: map[string]shnsdk.Validator{"2.0": canonical, "2.1": canonical}, CanonicalFallbackLines: map[string]bool{"2.1": true}}}
	if status, _ := g.validateDTRQuestionnaireResponse(context.Background(), []byte(`{"resourceType":"QuestionnaireResponse"}`), "2.1"); status != http.StatusServiceUnavailable {
		t.Fatalf("DTR context status=%d; unavailable PA lane borrowed PDex fallback", status)
	}
	if status, _ := g.validateFHIRPayerIngress(context.Background(), []byte(`{"resourceType":"Patient"}`), "2.1", "pa.dtr"); status != http.StatusServiceUnavailable {
		t.Fatalf("DTR ingress status=%d", status)
	}
	if status, _ := g.validateFHIR(context.Background(), []byte(`{"resourceType":"Patient"}`), "egress", ""); status != 0 {
		t.Fatalf("generic canonical status=%d", status)
	}
}
