//go:build !shn_coreonly

package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	metrics "github.com/SmartHealthNetwork/shn-sdk/metrics"
)

// goldenExchangeRecords are the golden fixture's exchanges: successes in both
// directions, a refusal on each side, backend errors of several classes, and
// values outside the closed lists. The two successes also carry their stages
// and body sizes (MON-R22, MON-R23).
func goldenExchangeRecords() []engine.ExchangeRecord {
	return []engine.ExchangeRecord{
		{Direction: engine.DirectionInbound, Exchange: "pas-claim", Outcome: engine.ExchangeAnswered, Latency: 1500 * time.Millisecond, Backend: &engine.BackendCall{Status: 200, Latency: 900 * time.Millisecond},
			Stages:       &engine.LegStages{Unwrap: 20 * time.Millisecond, Reads: 100 * time.Millisecond, Forward: 900 * time.Millisecond, Seal: 300 * time.Millisecond, Write: 30 * time.Millisecond},
			RequestBytes: goldenBytes(4096), ResponseBytes: goldenBytes(70000)},
		{Direction: engine.DirectionIngress, Exchange: "pas-claim", Outcome: engine.ExchangeAnswered, Latency: 2345678 * time.Microsecond,
			Network:      &engine.NetworkWait{Latency: 2 * time.Second, Calls: 1},
			RequestBytes: goldenBytes(3000), ResponseBytes: goldenBytes(65536)},
		{Direction: engine.DirectionInbound, Exchange: "pas-claim", Outcome: engine.ExchangeOther, Latency: 29 * time.Second, Backend: &engine.BackendCall{Latency: 29 * time.Second, ErrorClass: engine.BackendCancelled}},
		{Direction: engine.DirectionInbound, Exchange: "crd-order-select", Outcome: engine.ExchangeUpstreamError, Latency: 30 * time.Second, Backend: &engine.BackendCall{Latency: 30 * time.Second, ErrorClass: engine.BackendTimeout}},
		{Direction: engine.DirectionInbound, Exchange: "dtr-questionnaire-fetch", Outcome: engine.ExchangeUpstreamError, Backend: &engine.BackendCall{Status: 404, Latency: 40 * time.Millisecond, ErrorClass: engine.BackendHTTP4xx}},
		{Direction: engine.DirectionInbound, Exchange: "coverage-eligibility", Outcome: engine.ExchangeUpstreamError, Backend: &engine.BackendCall{ErrorClass: engine.ExchangeOther}},
		{Direction: engine.DirectionInbound, Exchange: "crd-order-select", Outcome: engine.ExchangeOther, Latency: 25 * time.Second, Backend: &engine.BackendCall{Status: 503, Latency: 80 * time.Millisecond, ErrorClass: engine.BackendHTTP5xx}},
		{Direction: engine.DirectionIngress, Exchange: "pas-claim", Outcome: engine.ExchangeRefused, RefusedBy: engine.RefusedByProviderGateway, Rule: engine.RefusalAuthentication, Operation: "order-sign", Sender: "holder-a", Recipient: "holder-b", CorrelationID: "leg-1", Trace: "trace-1", Latency: 3 * time.Millisecond},
		{Direction: engine.DirectionInbound, Exchange: "crd-order-select", Outcome: engine.ExchangeRefused, RefusedBy: engine.RefusedByPayerGateway, Rule: engine.RefusalConsent, Latency: 7 * time.Millisecond},
		{Direction: "sideways", Exchange: "caller-chosen", Outcome: "made-up", Backend: &engine.BackendCall{Latency: time.Millisecond, ErrorClass: "novel"}},
	}
}

// goldenBytes is a known body size.
func goldenBytes(n int64) *int64 { return &n }

// driveTelemetryFixture records the golden fixture through the gateway's own
// hooks: every exchange record, each leg outcome, each store, each involved
// reason, a pool sample, each audit reason, validator lanes, each token
// outcome, and one sample of the operational gauges; and the exchange
// additions' own hooks: requests in flight, findings (a tolerated one among
// them), validator calls, a dropped observe check and two requests to the
// main listener.
func driveTelemetryFixture(reg *telemetry.Registry) {
	ex := exchangeMetricHook(reg)
	for _, r := range goldenExchangeRecords() {
		ex(r)
	}
	leg := legMetricHook(reg)
	for _, o := range []string{engine.LegOutcomeRouted, engine.LegOutcomeAnswered, engine.LegOutcomeDenied, engine.LegOutcomeFailed, engine.LegOutcomeUnreachable} {
		leg(o)
	}
	se := storeErrorMetricHook(reg)
	for _, s := range []string{"exchange", "replay", "ingresskey", "pended"} {
		se(s)
	}
	io := involvedOmittedMetricHook(reg)
	for _, r := range []string{"overflow", "system-of-record-unavailable", "unknown-member", "authorization-refused", "authorization-failed", "involvement-not-carried", "budget"} {
		io(r)
	}
	emitStorePool(reg, storePoolStat{TotalConns: 7, AcquiredConns: 3, IdleConns: 4, EmptyAcquireCount: 11})
	am := auditMetrics{reg: reg}
	am.Sent()
	for _, r := range []string{"unknown-sender", "unavailable", "unreachable", "store"} {
		am.Retried(r)
	}
	for i, r := range []string{"overflow", "invalid", "store", "refused", "conflict", "stopped"} {
		am.Dropped(r, i+1)
	}
	am.Depth(7)
	am.EndpointUnknown(5)
	emitValidatorLanes(reg, []laneGauge{{"2.0", "routing", true}, {"2.2", "routing", false}, {"2.2", "certification", true}})
	att := validatorLaneAttemptHook(reg)
	att("2.1", true)
	att("2.2", false)
	tok := tokenMetricHook(reg)
	for i, o := range []string{engine.TokenOK, engine.TokenDenied, engine.TokenUnavailable, engine.TokenFailed, "novel"} {
		tok(o, time.Duration(i+1)*120*time.Millisecond)
	}
	now := time.Unix(1_761_350_400, 0).UTC()
	clock := func() time.Time { return now }
	certs := endpointcert.NewWithClock(clock)
	certs.Record(endpointcert.Of(endpointcert.Hub), endpointcert.Server, "hub.example", now.Add(90*24*time.Hour))
	certs.Record(endpointcert.Lane(endpointcert.Validator, "2.2"), endpointcert.Server, "validator.example", now.Add(20*24*time.Hour))
	certs.Record(endpointcert.Of(endpointcert.Ingress), endpointcert.Self, "TLS_CERT_FILE", now.Add(60*24*time.Hour))
	certs.Record(endpointcert.Of(endpointcert.TrustRootKind), endpointcert.TrustRoot, customTrustRoot, now.Add(5*24*time.Hour))
	feed := newFeedFreshness(func() time.Time { return now.Add(-42 * time.Second) })
	feed.succeeded(3)
	emitOps(reg, opsSources{
		info:  infoPoints("2.0.0", []string{"pa.pas@2.1", "pa.crd@2.2"}, "observe"),
		certs: certs,
		keys: func() pgstore.IngressKeyTimes {
			return pgstore.IngressKeyTimes{SigningNotAfter: now.Add(3 * time.Hour), HasSigning: true, NewestCreated: now.Add(-21 * time.Hour), HasNewest: true}
		},
		feed: feed, now: clock,
	})
	active := activeRequestHook(reg)
	active(engine.DirectionInbound, 1)
	active(engine.DirectionInbound, 1)
	active(engine.DirectionInbound, -1)
	active(engine.DirectionIngress, 1)
	found := findingMetricHook(reg)
	found(engine.FindingMetric{Exchange: "pas-claim", Level: "observe", Kind: string(engine.KindFHIRIngress), Action: "relayed"})
	found(engine.FindingMetric{Exchange: "crd-order-select", Level: "strict", Kind: string(engine.KindCDSEnvelope), Action: "refused"})
	found(engine.FindingMetric{Exchange: "pas-claim", Level: "strict", Kind: string(engine.KindFHIRIngress), Action: "relayed", Tolerated: true})
	found(engine.FindingMetric{Exchange: "caller-chosen", Level: "made-up", Kind: "novel", Action: "novel"})
	took := validatorMetricHook(reg)
	took("2.2", engine.ValidatorLaneRouting, 120*time.Millisecond)
	took("2.1", engine.ValidatorLaneCertification, 2*time.Second)
	took("9.9", "novel", time.Millisecond)
	observeCheckDroppedHook(reg)()
	// Two requests to the main listener (MON-R29): a matched route answered
	// 502, and a path no route serves, whose route is absent.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /Claim/$submit", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	step := time.Unix(1_761_350_400, 0)
	served := httpServerMetrics(reg, func() time.Time { step = step.Add(250 * time.Millisecond); return step }, mux)
	served.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/Claim/$submit", nil))
	served.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/Patient/123", nil))
}

// goldenEmitter is the fixture's EMF emitter: fixed namespace, Env and clock.
func goldenEmitter(buf *bytes.Buffer) *metrics.Emitter {
	return metrics.New(buf, "SHN/Golden", map[string]string{"Env": "golden"}, func() time.Time { return time.Unix(1_761_350_400, 0) })
}

// exchangeAdditionsEMFMetrics are the exchange additions' EMF metrics
// (MON-R20 to R24), whose lines testdata/emf_additions_golden.jsonl holds.
// Every other line, the 18 metrics the gateway wrote before the telemetry
// registry existed and the operational additions', is
// testdata/emf_golden.jsonl's (MON-R17).
var exchangeAdditionsEMFMetrics = []string{
	"ExchangeRefused", "ExchangeRefusedByRule", "ExchangeStageLatency", "ActiveRequests", "RequestBytes",
	"ResponseBytes", "ConformanceFinding", "ValidatorLatency", "ObserveCheckDropped",
}

// emfMetricName is the name of the metric an EMF line writes.
func emfMetricName(t *testing.T, line string) string {
	t.Helper()
	var doc struct {
		AWS struct {
			CloudWatchMetrics []struct {
				Metrics []struct{ Name string }
			}
		} `json:"_aws"`
	}
	if err := json.Unmarshal([]byte(line), &doc); err != nil || len(doc.AWS.CloudWatchMetrics) != 1 || len(doc.AWS.CloudWatchMetrics[0].Metrics) != 1 {
		t.Fatalf("not a one-metric EMF line (%v): %s", err, line)
	}
	return doc.AWS.CloudWatchMetrics[0].Metrics[0].Name
}

// goldenEMFLines drives the fixture into the EMF sink, samples the requests
// in flight once, and returns the sorted lines existing selects:
// testdata/emf_golden.jsonl's, or the exchange additions'.
func goldenEMFLines(t *testing.T, existing bool) []string {
	t.Helper()
	var buf bytes.Buffer
	emf := telemetry.NewEMF(goldenEmitter(&buf), "golden-gw", "payer")
	driveTelemetryFixture(telemetry.New(emf))
	emf.SampleActiveRequests()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !slices.Contains(exchangeAdditionsEMFMetrics, emfMetricName(t, line)) == existing {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

// TestEMFGolden_MON_R17: the 18 existing EMF metrics' lines are byte-identical
// to the lines the hooks wrote before the telemetry registry existed.
// testdata/emf_golden.jsonl was captured from those hooks, driven by this
// fixture, and is never regenerated from the registry: a difference is a
// change to a promised line. The operational additions' lines (MON-R25 to
// R27, R45, and their CertificateExpiryMin and DependencyAgeMax rollups) were
// added to it with them, and every earlier line kept byte for byte. The
// exchange additions' lines are new lines beside them
// (TestEMFAdditionsGolden_MON_R20). EMF extraction is per line, so the
// comparison is of the sorted lines.
func TestEMFGolden_MON_R17(t *testing.T) {
	got := goldenEMFLines(t, true)
	want, err := os.ReadFile("testdata/emf_golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Split(strings.TrimSpace(string(want)), "\n")
	if len(got) != len(wantLines) {
		t.Errorf("%d EMF lines, golden has %d", len(got), len(wantLines))
	}
	for i := 0; i < len(got) && i < len(wantLines); i++ {
		if got[i] != wantLines[i] {
			t.Errorf("EMF line %d differs from the golden:\n got  %s\n want %s", i, got[i], wantLines[i])
		}
	}
}

// emfAdditionsGoldenPath is the additions' EMF lines for the fixture. Set
// SHN_UPDATE_EMF_ADDITIONS_GOLDEN=1 to rewrite it; a change to it is a change
// to an EMF metric's name, unit or dimensions.
const emfAdditionsGoldenPath = "testdata/emf_additions_golden.jsonl"

// TestEMFAdditionsGolden_MON_R20: the exchange additions' EMF lines, each
// dimension from its closed list (a value outside one is other), pinned. The
// requests in flight are the peak since the last sample: two inbound, one
// ingress.
func TestEMFAdditionsGolden_MON_R20(t *testing.T) {
	got := strings.Join(goldenEMFLines(t, false), "\n") + "\n"
	for _, leak := range []string{"holder-a", "holder-b", "leg-1", "trace-1", "caller-chosen", "made-up", "novel", "sideways", "9.9"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q reached an EMF line", leak)
		}
	}
	if os.Getenv("SHN_UPDATE_EMF_ADDITIONS_GOLDEN") == "1" {
		if err := os.WriteFile(emfAdditionsGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(emfAdditionsGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("the additions' EMF lines differ from %s (SHN_UPDATE_EMF_ADDITIONS_GOLDEN=1 rewrites it):\n%s", emfAdditionsGoldenPath, got)
	}
}

// TestEMFLinesPerExchange_MON_R47 counts the EMF lines each fixture exchange
// writes, the figure behind the published CloudWatch cost per 1,000
// exchanges. A change to the count is a change to every hosted tenant's
// EMF volume: update the pin and the published figure together.
func TestEMFLinesPerExchange_MON_R47(t *testing.T) {
	// The exchange additions write the rest: a refusal's two lines, one
	// line for each stage an exchange entered, and one for each known body
	// size (MON-R20 to MON-R23).
	want := []int{12, 6, 4, 5, 4, 4, 5, 5, 4, 5}
	total := 0
	for i, r := range goldenExchangeRecords() {
		var buf bytes.Buffer
		exchangeMetricHook(emfRegistry(goldenEmitter(&buf), "golden-gw", "payer"))(r)
		n := strings.Count(buf.String(), "\n")
		total += n
		if n != want[i] {
			t.Errorf("exchange %d (%s %s %s) wrote %d EMF lines, pinned %d", i, r.Direction, r.Exchange, r.Outcome, n, want[i])
		}
	}
	if total != 54 {
		t.Errorf("the fixture's %d exchanges wrote %d EMF lines, pinned 54", len(goldenExchangeRecords()), total)
	}
}

// TestEMFMetricCombinations_MON_R47 counts the distinct metric-and-dimension
// combinations the whole fixture writes, which CloudWatch bills as custom
// metrics per gateway. A change to it is a change to every hosted tenant's
// custom-metric count: update the pin and the published figure together.
func TestEMFMetricCombinations_MON_R47(t *testing.T) {
	var buf bytes.Buffer
	driveTelemetryFixture(emfRegistry(goldenEmitter(&buf), "golden-gw", "payer"))
	combos := map[string]bool{}
	for _, m := range decodeEMFLines(t, &buf) {
		aws := m["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)
		key := aws["Metrics"].([]any)[0].(map[string]any)["Name"].(string)
		for _, d := range aws["Dimensions"].([]any)[0].([]any) {
			key += "|" + d.(string) + "=" + m[d.(string)].(string)
		}
		combos[key] = true
	}
	// 68 of the 18 existing metrics, 18 of the operational additions' lines,
	// and 24 of the exchange additions' (ActiveRequests writes only when
	// sampled).
	if len(combos) != 110 {
		t.Errorf("the fixture writes %d distinct metric-and-dimension combinations, pinned 110", len(combos))
	}
	// The most the exchange record's, conformance and in-flight metrics can
	// write for one gateway, at today's lists in a release build: the
	// exchange record's 373 series before the additions, and the additions'
	// 593 (TestExchangeMetricBound and TestConformanceMetricBound drive them).
	// The gateway's other EMF metrics have series of their own.
	if !toyProfileBuild && exchangeSeriesBound+conformanceSeriesBound != 373+593 {
		t.Errorf("the exchange, conformance and in-flight EMF series bound is %d, pinned %d", exchangeSeriesBound+conformanceSeriesBound, 373+593)
	}
}

// emfCostShape is one exchange shape of ../engine/testdata/exchange_shapes.json
// (engine TestExchangeShapes_MON_R47): what the engine's record carries that
// decides its EMF lines, and what the exchange reported through the engine's
// metric hooks.
type emfCostShape struct {
	Name, Source, Level, Direction, Exchange, Outcome, Rule, RefusedBy string
	Backend                                                            *struct {
		Status     int
		ErrorClass string
	}
	Stages                                   []string
	Rest                                     bool
	NetworkCalls                             int
	Self                                     bool
	RequestBytes, ResponseBytes              bool
	Legs, Tokens, Involved, StoreErrors      []string
	ValidatorCalls, Findings, ObserveDropped int
}

// write writes shape s's EMF lines through the gateway's own hooks: its
// record (a millisecond for each stage the engine's record spent time in, for
// the rest of an inbound leg's latency, and for an ingress call's network wait
// and its own time; a known size of 4 KiB), then each report.
func (s emfCostShape) write(t *testing.T, reg *telemetry.Registry) {
	t.Helper()
	ms := time.Millisecond
	size := int64(4096)
	r := engine.ExchangeRecord{Direction: s.Direction, Exchange: s.Exchange, Outcome: s.Outcome, Rule: s.Rule, RefusedBy: s.RefusedBy}
	if s.Backend != nil {
		r.Backend = &engine.BackendCall{Status: s.Backend.Status, ErrorClass: s.Backend.ErrorClass, Latency: ms}
	}
	if s.Direction == engine.DirectionInbound {
		st := &engine.LegStages{}
		stage := map[string]*time.Duration{"unwrap": &st.Unwrap, "reads": &st.Reads, "forward": &st.Forward, "validate": &st.Validate,
			"seal": &st.Seal, "ledger": &st.Ledger, "write": &st.Write}
		for _, name := range s.Stages {
			d, ok := stage[name]
			if !ok {
				t.Fatalf("%s: stage %q is none of the record's", s.Name, name)
			}
			*d = ms
			r.Latency += ms
		}
		r.Stages = st
		if s.Rest {
			r.Latency += ms
		}
	}
	if s.Direction == engine.DirectionIngress {
		if s.NetworkCalls > 0 {
			r.Network = &engine.NetworkWait{Latency: ms, Calls: s.NetworkCalls}
			r.Latency = ms
		}
		if s.Self {
			r.Latency += ms
		}
	}
	if s.RequestBytes {
		r.RequestBytes = &size
	}
	if s.ResponseBytes {
		r.ResponseBytes = &size
	}
	exchangeMetricHook(reg)(r)
	for _, o := range s.Legs {
		legMetricHook(reg)(o)
	}
	for _, o := range s.Tokens {
		tokenMetricHook(reg)(o, 120*ms)
	}
	for _, reason := range s.Involved {
		involvedOmittedMetricHook(reg)(reason)
	}
	for _, store := range s.StoreErrors {
		storeErrorMetricHook(reg)(store)
	}
	for range s.ValidatorCalls {
		validatorMetricHook(reg)("2.2", engine.ValidatorLaneRouting, 120*ms)
	}
	for range s.Findings {
		findingMetricHook(reg)(engine.FindingMetric{Exchange: s.Exchange, Level: s.Level, Kind: string(engine.KindContent), Action: "refused"})
	}
	for range s.ObserveDropped {
		observeCheckDroppedHook(reg)()
	}
}

// emfMetricsBeforeMonitoring are the 18 EMF metrics the gateway wrote before
// its monitoring additions (MON-R17), the "before" of the cost figures.
var emfMetricsBeforeMonitoring = []string{
	"Exchange", "ExchangeLatency", "BackendCall", "BackendLatency", "BackendError", "LegOutcome", "LegError",
	"StoreError", "InvolvedOmitted", "StorePool", "AuditAppendSent", "AuditAppendRetried", "AuditAppendDroppedBy",
	"AuditAppendDropped", "AuditQueueDepth", "AuditEndpointUnknown", "ValidatorLane", "ValidatorLaneAttempt",
}

// emfCostMixes are the exchange mixes the published cost per 1,000 exchanges
// is stated for, by shape name: a payer's gateway answering the network, and a
// provider's gateway calling it for its own system. A partner applies its own.
var emfCostMixes = map[string]map[string]int{
	"payer": {
		"inbound crd-order-select answered":                                                 400,
		"inbound dtr-questionnaire-fetch answered":                                          150,
		"inbound pas-claim answered (pended)":                                               150,
		"inbound pas-claim-inquire answered":                                                100,
		"inbound coverage-eligibility answered":                                             150,
		"inbound crd-order-select refused by the payer's gateway before dispatch (routing)": 20,
		"inbound crd-order-select, the payer's own system answers http-5xx":                 10,
		"inbound crd-order-select, the payer's own system answers http-4xx":                 10,
		"inbound crd-order-select, the payer's own system times out":                        10,
	},
	"provider": {
		"ingress crd-order-select answered":                                           960,
		"ingress pas-claim refused by the provider's gateway (routing)":               10,
		"ingress crd-order-select refused by the provider's gateway (authentication)": 10,
		"ingress crd-order-select refused by the Hub (replay)":                        10,
		"ingress crd-order-select, the recipient answers 5xx":                         10,
	},
}

// emfCostDocRows are MONITORING.md's "Lines per exchange" rows, each with the
// shapes it gives.
var emfCostDocRows = []struct {
	row    string
	shapes []string
}{
	{"A leg from the network, answered: CRD order-select, DTR questionnaire package, eligibility", []string{
		"inbound crd-order-select answered", "inbound dtr-questionnaire-fetch answered", "inbound coverage-eligibility answered"}},
	{"A leg from the network, answered: PAS submit or inquiry", []string{"inbound pas-claim answered (pended)", "inbound pas-claim-inquire answered"}},
	{"A CRD leg from the network, answered at `observe` (three `$validate` calls, no finding)", []string{"inbound crd-order-select answered at observe"}},
	{"A leg from the network refused by your gateway before it is dispatched", []string{"inbound crd-order-select refused by the payer's gateway before dispatch (routing)"}},
	{"A leg from the network refused by your gateway at `strict`, on one finding", []string{"inbound crd-order-select refused by the payer's gateway at strict (conformance)"}},
	{"A leg from the network your own system answers with a 4xx", []string{"inbound crd-order-select, the payer's own system answers http-4xx"}},
	{"A leg from the network your own system answers with a 5xx, or does not answer in time", []string{
		"inbound crd-order-select, the payer's own system answers http-5xx", "inbound crd-order-select, the payer's own system times out"}},
	{"A call from your own system, answered", []string{"ingress crd-order-select answered"}},
	{"A call from your own system refused by your gateway", []string{
		"ingress pas-claim refused by the provider's gateway (routing)", "ingress crd-order-select refused by the provider's gateway (authentication)"}},
	{"A call from your own system refused by the Hub", []string{"ingress crd-order-select refused by the Hub (replay)"}},
	{"A call from your own system that fails at the recipient", []string{"ingress crd-order-select, the recipient answers 5xx"}},
}

// emfCostMixRows are the mixes' rows in MONITORING.md's "Per 1,000
// exchanges" table.
var emfCostMixRows = map[string]string{"payer": "Payer's", "provider": "Provider's"}

// thousands is n with comma thousands separators, as the docs write it.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// emfCostShapeLines are each shape's EMF lines, before the monitoring additions
// and after.
var emfCostShapeLines = map[string][2]int{
	"inbound crd-order-select answered":                                                 {4, 14},
	"inbound crd-order-select answered at observe":                                      {4, 18},
	"inbound dtr-questionnaire-fetch answered":                                          {4, 14},
	"inbound pas-claim answered (pended)":                                               {4, 15},
	"inbound pas-claim-inquire answered":                                                {4, 15},
	"inbound coverage-eligibility answered":                                             {4, 14},
	"inbound crd-order-select refused by the payer's gateway at strict (conformance)":   {2, 13},
	"inbound crd-order-select, the payer's own system answers http-5xx":                 {5, 15},
	"inbound crd-order-select, the payer's own system answers http-4xx":                 {4, 14},
	"inbound crd-order-select, the payer's own system times out":                        {5, 15},
	"inbound crd-order-select refused by the payer's gateway before dispatch (routing)": {2, 8},
	"ingress crd-order-select answered":                                                 {4, 10},
	"ingress pas-claim refused by the provider's gateway (routing)":                     {2, 7},
	"ingress crd-order-select refused by the provider's gateway (authentication)":       {2, 6},
	"ingress crd-order-select refused by the Hub (replay)":                              {5, 13},
	"ingress crd-order-select, the recipient answers 5xx":                               {5, 11},
}

// emfCostMixPins are each mix's lines and bytes per 1,000 exchanges: before
// the exchange additions, then after.
var emfCostMixPins = map[string][4]int{
	"payer":    {3980, 1233750, 14150, 4583260},
	"provider": {3980, 1214820, 9970, 3066860},
}

// TestEMFCost_MON_R47 counts the EMF lines each engine exchange shape writes,
// before its monitoring additions (the 18 metrics of emfMetricsBeforeMonitoring) and
// after, and their bytes, as hosted tenants write them (namespace SHN/Preview,
// Env shn-preview, a Service of shn-cloud-acme); then the lines and bytes per
// 1,000 exchanges for each stated mix. The figures are MONITORING.md's "What
// EMF costs" and the platform's running guide's: a change to a pin is a change to every
// hosted tenant's EMF volume, so update the pin and the published figures
// together.
func TestEMFCost_MON_R47(t *testing.T) {
	raw, err := os.ReadFile("../engine/testdata/exchange_shapes.json")
	if err != nil {
		t.Fatal(err)
	}
	var shapes []emfCostShape
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&shapes); err != nil {
		t.Fatalf("%v: a field the cost does not count is a line it leaves out", err)
	}
	doc, err := os.ReadFile("../docs/MONITORING.md")
	if err != nil {
		t.Fatal(err)
	}
	monitoringDoc := string(doc)
	type count struct{ before, after, bytesBefore, bytesAfter int }
	counts := map[string]count{}
	for _, s := range shapes {
		var buf bytes.Buffer
		em := metrics.New(&buf, "SHN/Preview", map[string]string{"Env": "shn-preview"}, func() time.Time { return time.Unix(1_761_350_400, 0) })
		s.write(t, emfRegistry(em, "shn-cloud-acme", "payer"))
		var c count
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			c.after++
			c.bytesAfter += len(line) + 1
			if slices.Contains(emfMetricsBeforeMonitoring, emfMetricName(t, line)) {
				c.before++
				c.bytesBefore += len(line) + 1
			}
		}
		counts[s.Name] = c
		t.Logf("%-80s before %d after %d lines, %d / %d bytes", s.Name, c.before, c.after, c.bytesBefore, c.bytesAfter)
		if want, ok := emfCostShapeLines[s.Name]; !ok || want != [2]int{c.before, c.after} {
			t.Errorf("%s writes %d lines before the additions and %d after, pinned %v", s.Name, c.before, c.after, want)
		}
	}
	if len(counts) != len(emfCostShapeLines) {
		t.Errorf("%d shapes, %d pinned", len(counts), len(emfCostShapeLines))
	}
	// MONITORING.md's "Lines per exchange" table gives each row's shapes'
	// counts: one before, and the after (a range where the shapes differ).
	covered := map[string]bool{}
	for _, r := range emfCostDocRows {
		before, lo, hi := -1, 0, 0
		for n, name := range r.shapes {
			c, ok := counts[name]
			if !ok {
				t.Fatalf("row %q names %q, which is no shape", r.row, name)
			}
			covered[name] = true
			if n == 0 {
				before, lo, hi = c.before, c.after, c.after
				continue
			}
			if c.before != before {
				t.Errorf("row %q: its shapes write %d and %d lines before; split the row", r.row, before, c.before)
			}
			lo, hi = min(lo, c.after), max(hi, c.after)
		}
		after := strconv.Itoa(lo)
		if hi != lo {
			after = fmt.Sprintf("%d to %d", lo, hi)
		}
		if want := fmt.Sprintf("| %s | %d | %s |", r.row, before, after); !strings.Contains(monitoringDoc, want) {
			t.Errorf("docs/MONITORING.md does not publish %q", want)
		}
	}
	for name := range counts {
		if !covered[name] {
			t.Errorf("no row of MONITORING.md's lines per exchange gives the shape %q", name)
		}
	}
	for mix, weights := range emfCostMixes {
		var n int
		var got count
		for name, w := range weights {
			c, ok := counts[name]
			if !ok {
				t.Fatalf("mix %s names %q, which is no shape", mix, name)
			}
			n += w
			got.before += w * c.before
			got.after += w * c.after
			got.bytesBefore += w * c.bytesBefore
			got.bytesAfter += w * c.bytesAfter
		}
		if n != 1000 {
			t.Fatalf("mix %s weighs %d exchanges, not 1,000", mix, n)
		}
		t.Logf("mix %s per 1,000 exchanges: %d lines (%d bytes) before, %d lines (%d bytes) after", mix, got.before, got.bytesBefore, got.after, got.bytesAfter)
		if want := emfCostMixPins[mix]; want != [4]int{got.before, got.bytesBefore, got.after, got.bytesAfter} {
			t.Errorf("mix %s per 1,000 exchanges: %d lines (%d bytes) before, %d lines (%d bytes) after; pinned %v", mix, got.before, got.bytesBefore, got.after, got.bytesAfter, want)
		}
		// MONITORING.md publishes the pinned figures, at the list price it
		// states ($0.50 per GB of 10^9 bytes).
		perMillion := func(bytes int) string { return fmt.Sprintf("$%.2f", float64(bytes)*1000/1e9*0.5) }
		row := fmt.Sprintf("| %s | %s | %s | %.2f MB | $%.4f | %s → %s |", emfCostMixRows[mix], thousands(got.before), thousands(got.after),
			float64(got.bytesAfter)/1e6, float64(got.bytesAfter)/1e9*0.5, perMillion(got.bytesBefore), perMillion(got.bytesAfter))
		if !strings.Contains(monitoringDoc, row) {
			t.Errorf("docs/MONITORING.md does not publish %q, this mix's figures per 1,000 exchanges", row)
		}
	}
}
