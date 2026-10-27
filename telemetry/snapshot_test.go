package telemetry

import (
	"bytes"
	"encoding/json"
	"log"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	metrics "github.com/SmartHealthNetwork/shn-sdk/metrics"
)

// snapshots keeps every snapshot it is handed, whole.
type snapshots struct {
	recorder
	sets [][]Measurement
}

func (s *snapshots) RecordSnapshot(_ *Instrument, series []Measurement) {
	s.sets = append(s.sets, series)
}

func attrsOf(series []Measurement) []string {
	var out []string
	for _, m := range series {
		out = append(out, strings.Join(m.Attributes, ","))
	}
	return out
}

// MON-R15, MON-R18: a snapshot's points fold into their closed lists, points
// that fold together keep the least value, and the series come out in one
// order whatever order the points came in, so an over-the-list snapshot
// reports its overflow series rather than whichever point a map yielded.
func TestRegistry_SnapshotFoldsMergesAndSorts_MON_R15_R18(t *testing.T) {
	points := []Point{
		{Value: 30, Attributes: []string{"hub", "server", NoIGLine}},
		{Value: 50, Attributes: []string{"not-a-kind", "server", NoIGLine}},
		{Value: 20, Attributes: []string{"validator", "server", "2.1"}},
		{Value: 40, Attributes: []string{"also-not-a-kind", "server", NoIGLine}},
		{Value: 10, Attributes: []string{"hub", "server", NoIGLine}},
	}
	var first []string
	for n := range len(points) {
		rotated := append(slices.Clone(points[n:]), points[:n]...)
		snap := &snapshots{}
		New(snap).RecordSnapshot(CertificateExpiry, rotated)
		if len(snap.sets) != 1 || len(snap.got) != 0 {
			t.Fatalf("a SnapshotSink took %d snapshots and %d single records, want 1 and 0", len(snap.sets), len(snap.got))
		}
		got := attrsOf(snap.sets[0])
		if first == nil {
			first = got
		} else if !slices.Equal(got, first) {
			t.Fatalf("rotation %d: series %v, want the same order as %v", n, got, first)
		}
		want := map[string]float64{"hub,server,none": 10, "other,server,none": 40, "validator,server,2.1": 20}
		if len(snap.sets[0]) != len(want) {
			t.Fatalf("series %v, want %v", got, want)
		}
		for _, m := range snap.sets[0] {
			if v, ok := want[strings.Join(m.Attributes, ",")]; !ok || m.Value != v || m.Instrument != CertificateExpiry {
				t.Errorf("series %v = %v, want %v (the least of the points folded into it)", m.Attributes, m.Value, v)
			}
		}
	}
	if !slices.IsSorted(first) {
		t.Fatalf("series %v are not sorted by their attribute values", first)
	}
}

// A sink that is not a SnapshotSink gets each series through Record, each
// SnapshotSink gets a slice of its own, and an empty snapshot is handed on
// (it is how a value stops being known).
func TestRegistry_SnapshotReachesEverySink(t *testing.T) {
	plain, a, b := &recorder{}, &snapshots{}, &snapshots{}
	reg := New(plain, a, b)
	reg.RecordSnapshot(DependencyAge, []Point{{Value: 1, Attributes: []string{"registrar-feed"}}})
	if len(plain.got) != 1 || plain.got[0].Instrument != DependencyAge || plain.got[0].Value != 1 {
		t.Fatalf("plain sink got %+v", plain.got)
	}
	a.sets[0][0].Value = 99
	if b.sets[0][0].Value != 1 {
		t.Fatal("two SnapshotSinks share one slice")
	}
	reg.RecordSnapshot(DependencyAge, nil)
	if len(a.sets) != 2 || len(a.sets[1]) != 0 {
		t.Fatalf("an empty snapshot did not replace the last: %v", a.sets)
	}
}

// What does not fit a snapshot is dropped, never a guess (MON-R12).
func TestRegistry_SnapshotDropsWhatDoesNotFit(t *testing.T) {
	snap := &snapshots{}
	reg := New(snap)
	reg.RecordSnapshot(CertificateExpiry, []Point{
		{Value: 1, Attributes: []string{"hub", "server"}},                  // too few values
		{Value: math.NaN(), Attributes: []string{"hub", "server", "none"}}, // not finite
		{Value: math.Inf(1), Attributes: []string{"cds", "server", "none"}},
		{Value: 5, Attributes: []string{"audit", "server", "none"}},
	})
	if got := attrsOf(snap.sets[0]); !slices.Equal(got, []string{"audit,server,none"}) {
		t.Fatalf("series %v, want only the point that fits", got)
	}
	reg.RecordSnapshot(AuditQueueDepth, []Point{{Value: 1}}) // a Gauge is recorded
	reg.RecordSnapshot(nil, nil)
	reg.Record(PeersKnown, 3) // a Snapshot is never recorded one value at a time
	reg.Add(PeersKnown, 3)
	reg.RecordDuration(CertificateExpiry, time.Second, "hub", "server", NoIGLine)
	if len(snap.sets) != 1 || len(snap.got) != 0 {
		t.Fatalf("recorded misfits: %d snapshots, %d records", len(snap.sets), len(snap.got))
	}
	var nilReg *Registry
	nilReg.RecordSnapshot(PeersKnown, []Point{{Value: 1}})
	// Nor does a single value reach the EMF sink as a stray line.
	var buf bytes.Buffer
	em := metrics.New(&buf, "SHN/Test", nil, nil)
	emf := New(NewEMF(em, "gw", ""))
	emf.Record(CertificateExpiry, 1, "hub", "server", NoIGLine)
	emf.Record(PeersKnown, 1)
	if buf.Len() != 0 {
		t.Fatalf("a Snapshot recorded one value at a time wrote %q", buf.String())
	}
}

type panickySnapshot struct{ panicky }

func (panickySnapshot) RecordSnapshot(*Instrument, []Measurement) { panic("snapshot bug") }

// MON-R12: a SnapshotSink that panics affects neither the caller nor the
// sinks after it.
func TestRegistry_SnapshotSinkPanicIsContained_MON_R12(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	snap := &snapshots{}
	New(panickySnapshot{}, snap).RecordSnapshot(PeersKnown, []Point{{Value: 2}})
	if len(snap.sets) != 1 {
		t.Fatal("the sink after a panicking one was skipped")
	}
	if !strings.Contains(buf.String(), `a metrics sink panicked recording shn.gateway.peers.known: "snapshot bug"`) {
		t.Fatalf("log: %q", buf.String())
	}
}

// emfLinesOf records a snapshot of points on an EMF sink and returns each
// line it wrote, in order, as name{dimensions}=value.
func emfLinesOf(t *testing.T, i *Instrument, points []Point) []string {
	t.Helper()
	var buf bytes.Buffer
	em := metrics.New(&buf, "SHN/Test", map[string]string{"Env": "test"}, func() time.Time { return time.Unix(0, 0) })
	New(NewEMF(em, "gw", "")).RecordSnapshot(i, points)
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatal(err)
		}
		aws := doc["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)
		name := aws["Metrics"].([]any)[0].(map[string]any)["Name"].(string)
		dims := aws["Dimensions"].([]any)[0].([]any)
		var kv []string
		for _, d := range dims {
			kv = append(kv, d.(string)+"="+doc[d.(string)].(string))
		}
		out = append(out, name+"{"+strings.Join(kv, ",")+"}="+strconv.FormatFloat(doc[name].(float64), 'f', -1, 64))
	}
	return out
}

// MON-R34: each snapshot writes its series' lines and then the one rollup
// line the alarm watches; an empty snapshot writes nothing.
func TestEMF_SnapshotRollups_MON_R34(t *testing.T) {
	got := emfLinesOf(t, CertificateExpiry, []Point{
		{Value: 864000, Attributes: []string{"hub", "server", NoIGLine}},
		{Value: 86400, Attributes: []string{"validator", "server", "2.1"}},
		{Value: 600, Attributes: []string{"trust-root", "trust-root", NoIGLine}},
		{Value: 7200, Attributes: []string{"ingress", "self", NoIGLine}},
	})
	want := []string{
		"CertificateExpiry{Env=test,Service=gw,kind=hub,line=none,role=server}=864000",
		"CertificateExpiry{Env=test,Service=gw,kind=ingress,line=none,role=self}=7200",
		"CertificateExpiry{Env=test,Service=gw,kind=trust-root,line=none,role=trust-root}=600",
		"CertificateExpiry{Env=test,Service=gw,kind=validator,line=2.1,role=server}=86400",
		"CertificateExpiryMin{Env=test,Service=gw}=7200",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("lines\n%v\nwant\n%v", got, want)
	}
	got = emfLinesOf(t, DependencyAge, []Point{{Value: 31, Attributes: []string{"registrar-feed"}}})
	want = []string{"DependencyAge{Env=test,Service=gw,dependency=registrar-feed}=31", "DependencyAgeMax{Env=test,Service=gw,dependency=registrar-feed}=31"}
	if !slices.Equal(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
	for _, i := range []*Instrument{CertificateExpiry, DependencyAge} {
		if got := emfLinesOf(t, i, nil); len(got) != 0 {
			t.Errorf("%s: an empty snapshot wrote %v", i.Name, got)
		}
	}
	// A trust-root alone is a line of its own and no Min line.
	got = emfLinesOf(t, CertificateExpiry, []Point{{Value: 600, Attributes: []string{"trust-root", "trust-root", NoIGLine}}})
	if want := []string{"CertificateExpiry{Env=test,Service=gw,kind=trust-root,line=none,role=trust-root}=600"}; !slices.Equal(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
}

// The closed lists the EMF lines pick from are literals, so the stability
// fence can read them; each is its source's list exactly.
func TestClosedListsMatchTheirSources(t *testing.T) {
	levels := []string{engine.EnforcementNone.String(), engine.EnforcementObserve.String(), engine.EnforcementStructural.String(), engine.EnforcementStrict.String()}
	// The wire protocol list covers every version the SDK speaks, whichever
	// SDK the build links: the workspace's, or the published one a release
	// pins, which can speak fewer.
	for _, v := range shnsdk.WireProtocolVersions() {
		if !slices.Contains(WireProtocols, v) {
			t.Errorf("WireProtocols %v lacks %s, which the SDK speaks", WireProtocols, v)
		}
	}
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"ConformanceLevels", ConformanceLevels, levels},
		{"EndpointKinds", EndpointKinds, endpointcert.KindNames()},
		{"CertRoles", CertRoles, endpointcert.RoleNames()},
		{"CertIGLines", CertIGLines, append(slices.Clone(IGLines), NoIGLine)},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s = %v, its source has %v", c.name, c.got, c.want)
		}
	}
}
