package telemetry

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// recorder keeps every measurement it is handed.
type recorder struct{ got []Measurement }

func (r *recorder) Record(m Measurement) { r.got = append(r.got, m) }

// MON-R18: every attribute value comes from its closed list; anything else
// records as Other.
func TestRegistry_FoldsToClosedLists_MON_R18(t *testing.T) {
	rec := &recorder{}
	reg := New(rec)
	kind := engine.ExchangeKinds[0] // a compiled-in leg, whichever workstreams the build carries
	reg.Add(Exchanges, 1, "inbound", kind, "answered")
	reg.Add(Exchanges, 1, "sideways", "holder-a", "Patient/123")
	reg.Record(ValidatorLaneQualified, 1, "9.9", "routing")
	if len(rec.got) != 3 {
		t.Fatalf("%d measurements, want 3", len(rec.got))
	}
	for i, want := range [][]string{{"inbound", kind, "answered"}, {Other, Other, Other}, {Other, "routing"}} {
		if strings.Join(rec.got[i].Attributes, ",") != strings.Join(want, ",") {
			t.Errorf("measurement %d attributes %v, want %v", i, rec.got[i].Attributes, want)
		}
	}
	// An attribute with its own fold value folds to it; on an Optional one,
	// "" stays "", the attribute absent, and anything else outside the list
	// still folds.
	rec.got = nil
	reg.RecordDuration(HTTPServerRequestDuration, time.Millisecond, "BREW", "", "", "http", "1.1", "")
	reg.RecordDuration(HTTPServerRequestDuration, time.Millisecond, "GET", "200", "/health", "gopher", "0.9", "boom")
	for i, want := range [][]string{
		{SemconvOther, "", "", "http", "1.1", ""},
		{"GET", "200", "/health", SemconvOther, SemconvOther, SemconvOther},
	} {
		if strings.Join(rec.got[i].Attributes, ",") != strings.Join(want, ",") {
			t.Errorf("semconv measurement %d attributes %q, want %q", i, rec.got[i].Attributes, want)
		}
	}
	// "" on an attribute that is not Optional is outside its list.
	rec.got = nil
	reg.Record(ValidatorLaneQualified, 1, "", "routing")
	if rec.got[0].Attributes[0] != Other {
		t.Errorf(`"" on a required attribute recorded as %q, want %q`, rec.got[0].Attributes[0], Other)
	}
}

// MON-R18: an attribute's cardinality counts its fold value once, and the
// absence of an Optional one; an instrument's Bound replaces the product.
func TestAttributeCardinality_MON_R18(t *testing.T) {
	for _, row := range []struct {
		a    Attribute
		want int
	}{
		{Attribute{Values: []string{"a", "b"}}, 3},
		{Attribute{Values: []string{"a", Other}}, 2},
		{Attribute{Values: []string{"a"}, OtherValue: SemconvOther}, 2},
		{Attribute{Values: []string{"a", SemconvOther}, OtherValue: SemconvOther}, 2},
		{Attribute{Values: []string{"a"}, OtherValue: SemconvOther, Optional: true}, 3},
	} {
		if got := row.a.cardinality(); got != row.want {
			t.Errorf("%+v: cardinality %d, want %d", row.a, got, row.want)
		}
	}
	i := &Instrument{Attributes: []Attribute{{Values: []string{"a"}}}, Bound: 7}
	if i.SeriesBound() != 7 {
		t.Errorf("bound %d, want the instrument's own 7", i.SeriesBound())
	}
}

// MON-R18, MON-R29: the HTTP server metric's bound is the most it can record
// (a cap), from the closed lists: per scheme and version (and _OTHER for
// another HTTP/1 version), each route's own methods (any method with no route)
// times the status and error pairs a request can record (counted by
// TestHTTPStatusErrorPairs_MON_R18). Pinned: a change to a route or a list is a change to what every backend
// stores.
func TestHTTPServerBound_MON_R18(t *testing.T) {
	routes := 10 // no route matched: the nine methods and _OTHER
	for _, p := range HTTPRoutePatterns {
		switch {
		case !strings.Contains(p, " "):
			routes += 10
		case strings.HasPrefix(p, "GET "):
			routes += 2
		default:
			routes++
		}
	}
	// 200 to 599 twice, 600 to 999 once, and a panic before any status.
	const pairs = 2*400 + 400 + 1
	want := 2 * (3 + 1) * routes * pairs
	if got := HTTPServerRequestDuration.SeriesBound(); got != want || HTTPStatusErrorPairs() != pairs {
		t.Errorf("bound %d with %d status and error pairs, want %d with %d", got, HTTPStatusErrorPairs(), want, pairs)
	}
	if routes != 71 || len(HTTPStatusCodes) != 900 || want != 682168 {
		t.Errorf("%d method-route pairs, %d status codes, bound %d: pinned 71, 900 and 682168", routes, len(HTTPStatusCodes), want)
	}
}

// A measurement that does not fit its instrument is dropped, never a panic
// and never a guess (MON-R12).
func TestRegistry_DropsWhatDoesNotFit(t *testing.T) {
	rec := &recorder{}
	reg := New(nil, rec)
	reg.Add(Exchanges, 1, "inbound", "pas-claim")                                        // too few values
	reg.Add(Exchanges, 1, "inbound", "pas-claim", "answered", "extra")                   // too many
	reg.Add(Exchanges, -1, "inbound", "pas-claim", "answered")                           // a counter never goes down
	reg.Record(Exchanges, 1, "inbound", "pas-claim", "answered")                         // a counter is added to
	reg.Add(AuditQueueDepth, 1)                                                          // a gauge is recorded
	reg.RecordDuration(AuditQueueDepth, time.Second)                                     // not a histogram
	reg.RecordDuration(&Instrument{Name: "x", Kind: Histogram, Unit: "By"}, time.Second) // not in seconds
	reg.Add(nil, 1)
	if len(rec.got) != 0 {
		t.Fatalf("recorded %d misfits: %+v", len(rec.got), rec.got)
	}
	var nilReg *Registry
	nilReg.Add(Exchanges, 1, "inbound", "pas-claim", "answered")
	New().Add(Exchanges, 1, "inbound", "pas-claim", "answered")

	reg.RecordDuration(ExchangeDuration, 1500*time.Millisecond, "inbound", "pas-claim")
	if m := rec.got[0]; m.Value != 1.5 || !m.IsDuration || m.Duration != 1500*time.Millisecond {
		t.Fatalf("duration measurement %+v", m)
	}
}

type panicky struct{}

func (panicky) Record(Measurement) { panic("sink bug\ngateway: access: {}") }

// MON-R12: a sink that panics affects neither the caller nor the sinks after
// it, and its panic cannot start a log line of its own.
func TestRegistry_SinkPanicIsContained_MON_R12(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	rec := &recorder{}
	New(panicky{}, rec).Add(Legs, 1, "failed")
	if len(rec.got) != 1 {
		t.Fatal("the sink after a panicking one was skipped")
	}
	if !strings.Contains(buf.String(), `a metrics sink panicked recording shn.gateway.legs: "sink bug\ngateway: access: {}"`) {
		t.Fatalf("log: %q", buf.String())
	}
}

// allowedKeys are the attribute keys of the catalogue. Each one names a
// closed list; adding one here is adding a dimension every backend stores,
// so it never names a peer, an id, a patient, a payload value, a URL or free
// text (MON-R19).
var allowedKeys = map[string]bool{
	KeyDirection: true, KeyExchange: true, KeyOutcome: true, KeyBackendClass: true, KeyStore: true,
	KeyReason: true, KeyPoolState: true, KeyIGLine: true, KeyLane: true, KeyWireProtocol: true, KeyLevel: true,
	KeyEndpointKind: true, KeyCertRole: true, KeyDependency: true,
	KeyRefusalRule: true, KeyRefusalBy: true, KeyStage: true, KeyFindingKind: true,
	KeyAction: true, KeyTolerated: true,
}

// semconvKeys are http.server.request.duration's attribute keys, the
// semantic convention's (MON-R29). The route is the matched pattern's
// template, never the request's path.
var semconvKeys = map[string]bool{
	KeyHTTPMethod: true, KeyHTTPStatusCode: true, KeyHTTPRoute: true, KeyURLScheme: true,
	KeyProtocolVersion: true, KeyErrorType: true,
}

// MON-R19, MON-R20: every instrument is a shn.gateway.* name with a unit and
// a description, and every attribute is an allowed shn.* key with a closed,
// non-empty list.
func TestCatalogue_ClosedAndFreeOfIdentifiers_MON_R19_R20(t *testing.T) {
	seen := map[string]bool{}
	for _, inst := range Instruments() {
		if (!strings.HasPrefix(inst.Name, "shn.gateway.") && inst != HTTPServerRequestDuration) || seen[inst.Name] {
			t.Errorf("%q: not a shn.gateway.* name, or a duplicate", inst.Name)
		}
		seen[inst.Name] = true
		if inst.Description == "" || inst.Kind.String() == "unknown" {
			t.Errorf("%s: description %q kind %v", inst.Name, inst.Description, inst.Kind)
		}
		if inst.Kind == Histogram && (inst.Unit != "s" && inst.Unit != "By" || len(inst.Buckets) == 0) {
			t.Errorf("%s: a histogram is in s or By with buckets", inst.Name)
		}
		for _, a := range inst.Attributes {
			// The semantic convention's instrument carries the convention's
			// own keys; every other key is shn.*.
			keys, prefix := allowedKeys, "shn."
			if inst == HTTPServerRequestDuration {
				keys, prefix = semconvKeys, ""
			}
			if !keys[a.Key] || !strings.HasPrefix(a.Key, prefix) || len(a.Values) == 0 {
				t.Errorf("%s: attribute %q is not an allowed closed key", inst.Name, a.Key)
			}
		}
		if keys := inst.AttributeKeys(); len(keys) != len(inst.Attributes) || (len(keys) > 0 && keys[0] != inst.Attributes[0].Key) {
			t.Errorf("%s: attribute keys %v", inst.Name, keys)
		}
		if inst.SeriesBound() < 1 {
			t.Errorf("%s: bound %d", inst.Name, inst.SeriesBound())
		}
	}
	if len(seen) != 34 {
		t.Errorf("%d instruments; the catalogue's count is part of MONITORING.md's and STABILITY's", len(seen))
	}
}

// Every instrument but the OpenTelemetry-only ones (the export count and the
// HTTP server metric) writes EMF lines.
func TestCatalogue_EMFCoverage_MON_R17(t *testing.T) {
	for _, inst := range Instruments() {
		if _, ok := emfLines[inst]; ok == (inst == TelemetryExports || inst == HTTPServerRequestDuration) {
			t.Errorf("%s: EMF line present=%t", inst.Name, ok)
		}
	}
}

// Only package otelsetup imports OpenTelemetry, gRPC or protobuf (and the
// collector the tests run): the registry, the EMF sink and every caller stay
// free of them.
func TestOnlyOtelsetupImportsOpenTelemetry(t *testing.T) {
	root := ".."
	allowed := []string{"telemetry/otelsetup/", "internal/otlptest/"}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(p, "go.opentelemetry.io/") && !strings.HasPrefix(p, "google.golang.org/grpc") && !strings.HasPrefix(p, "google.golang.org/protobuf") {
				continue
			}
			ok := false
			for _, a := range allowed {
				ok = ok || strings.HasPrefix(rel, a)
			}
			if !ok {
				t.Errorf("%s imports %s; only package otelsetup may", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
