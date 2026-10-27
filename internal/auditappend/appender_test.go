package auditappend

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// counts is a Metrics that records what it was told.
type counts struct {
	mu       sync.Mutex
	sent     int
	retried  map[string]int
	dropped  map[string]int
	depth    []int
	unknown  []int
	reported chan struct{}
}

func newCounts() *counts {
	return &counts{retried: map[string]int{}, dropped: map[string]int{}, reported: make(chan struct{}, 64)}
}

func (c *counts) Sent()                   { c.mu.Lock(); c.sent++; c.mu.Unlock() }
func (c *counts) Retried(r string)        { c.mu.Lock(); c.retried[r]++; c.mu.Unlock() }
func (c *counts) Dropped(r string, n int) { c.mu.Lock(); c.dropped[r] += n; c.mu.Unlock() }
func (c *counts) Depth(n int) {
	c.mu.Lock()
	c.depth = append(c.depth, n)
	c.mu.Unlock()
	select {
	case c.reported <- struct{}{}:
	default:
	}
}
func (c *counts) EndpointUnknown(n int) { c.mu.Lock(); c.unknown = append(c.unknown, n); c.mu.Unlock() }

func (c *counts) snapshot() (sent int, retried, dropped map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, d := map[string]int{}, map[string]int{}
	for k, v := range c.retried {
		r[k] = v
	}
	for k, v := range c.dropped {
		d[k] = v
	}
	return c.sent, r, d
}

var testKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

func newAppender(t *testing.T, endpoint string, store Store, m Metrics) *Appender {
	t.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	a, err := New(Config{
		Holder: holder, SignPriv: testKey, Endpoint: endpoint, Store: store, Metrics: m,
		Now:    func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) },
		Jitter: rng.Float64,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// fresh is a module-details record with no recordId: Append mints it.
func fresh() shnsdk.AuditRecord {
	r := authored()
	r.RecordID = ""
	return r
}

// TestAppendStoresTheCheckedRecordUnsigned: the stored body is the record
// as the guard passed it, with the minted recordId, and no signature: the
// send signs it.
func TestAppendStoresTheCheckedRecordUnsigned(t *testing.T) {
	s := NewMemStore()
	a := newAppender(t, "https://audit.example", s, newCounts())
	if got := a.Append(fresh()); got != Queued {
		t.Fatalf("Append = %s, want %s", got, Queued)
	}
	e, ok, _ := s.Claim(context.Background(), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), lease)
	if !ok {
		t.Fatal("nothing queued")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(e.Body, &members); err != nil {
		t.Fatal(err)
	}
	if _, signed := members["signatures"]; signed {
		t.Fatalf("the outbox holds a signature: %s", e.Body)
	}
	var r shnsdk.AuditRecord
	if err := json.Unmarshal(e.Body, &r); err != nil {
		t.Fatal(err)
	}
	if r.RecordID == "" || r.RecordID != e.RecordID || !recordIDShape.MatchString(r.RecordID) {
		t.Fatalf("recordId %q (entry %q): want a minted ULID on both", r.RecordID, e.RecordID)
	}
	if err := Check(r, holder); err != nil {
		t.Fatalf("the stored record is not the one the guard passed: %v", err)
	}
}

// keyPlane is a fake Audit Plane that accepts a record only under the one key
// it holds for the holder, as /append does, and answers 401 otherwise.
type keyPlane struct {
	mu     sync.Mutex
	key    ed25519.PublicKey
	bodies [][]byte
}

func (p *keyPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var req shnsdk.AuditAppendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Signatures) != 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	sig, err := base64.StdEncoding.DecodeString(req.Signatures[0])
	if err != nil || !ed25519.Verify(p.key, shnsdk.SignableContent(req.AuditRecord), sig) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthenticated audit record"}`))
		return
	}
	raw, _ := json.Marshal(req)
	p.bodies = append(p.bodies, raw)
	w.WriteHeader(http.StatusOK)
}

// TestSendSignsWithTheKeyTheGatewayRunsWith: a record queued under one key
// and sent by a gateway started again under a rotated key is signed with the
// new key and accepted; signing at queue time would have sent old-key bytes,
// which the Plane refuses once it holds the new key (stop, rotate, start).
func TestSendSignsWithTheKeyTheGatewayRunsWith(t *testing.T) {
	rotated := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	p := &keyPlane{key: rotated.Public().(ed25519.PublicKey)}
	srv := httptest.NewServer(p)
	defer srv.Close()
	outbox := &durableStore{} // survives the restart
	if got := newAppender(t, srv.URL, outbox, newCounts()).Append(fresh()); got != Queued {
		t.Fatalf("Append = %s, want %s", got, Queued)
	}
	m := newCounts()
	restarted, err := New(Config{Holder: holder, SignPriv: rotated, Endpoint: srv.URL, Store: outbox, Metrics: m,
		Now: func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	restarted.sendOne(context.Background(), 1)
	if sent, _, dropped := m.snapshot(); sent != 1 || len(dropped) != 0 {
		t.Fatalf("sent %d, dropped %v: want the queued record accepted under the rotated key", sent, dropped)
	}
	if n, _ := outbox.Depth(context.Background()); n != 0 {
		t.Fatalf("Depth = %d after delivery, want 0", n)
	}
}

// TestSendDropsAQueuedRowItWouldNotSend: a row that is not what Append
// stores (a value the guard refuses, an already signed request, more than one
// value or anything after it, a record under another row's recordId, not a
// record) leaves the outbox counted invalid and is never sent.
func TestSendDropsAQueuedRowItWouldNotSend(t *testing.T) {
	good, err := json.Marshal(authored())
	if err != nil {
		t.Fatal(err)
	}
	other := authored()
	other.Sender = "someone-else"
	foreign, _ := json.Marshal(other)
	signed, _ := json.Marshal(shnsdk.AuditAppendRequest{AuditRecord: authored(), Signatures: []string{"c2ln"}})
	for name, body := range map[string][]byte{
		"another holder's record": foreign,
		"a signed request":        signed,
		"two values":              append(append([]byte{}, good...), good...),
		"a trailing brace":        append(append([]byte{}, good...), '}'),
		"another row's recordId":  mustMarshal(t, withRecordID(authored(), "01JZX5A7B8C9D0E1F2G3H4JA99")),
		"not a record":            []byte(`[1,2]`),
	} {
		p := &plane{}
		srv := httptest.NewServer(p)
		s := NewMemStore()
		if _, err := s.Enqueue(context.Background(), Entry{RecordID: authored().RecordID, Body: body}, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Bound); err != nil {
			t.Fatal(err)
		}
		m := newCounts()
		newAppender(t, srv.URL, s, m).sendOne(context.Background(), 1)
		srv.Close()
		if _, _, d := m.snapshot(); d[DropInvalid] != 1 {
			t.Errorf("%s: dropped = %v, want invalid once", name, d)
		}
		if len(p.sent()) != 0 {
			t.Errorf("%s: sent %d times, want never", name, len(p.sent()))
		}
		if n, _ := s.Depth(context.Background()); n != 0 {
			t.Errorf("%s: Depth = %d, want the row gone", name, n)
		}
	}
	// The control: the record itself is sent.
	p := &plane{}
	srv := httptest.NewServer(p)
	defer srv.Close()
	s := NewMemStore()
	if _, err := s.Enqueue(context.Background(), Entry{RecordID: authored().RecordID, Body: good}, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Bound); err != nil {
		t.Fatal(err)
	}
	m := newCounts()
	newAppender(t, srv.URL, s, m).sendOne(context.Background(), 1)
	if sent, _, _ := m.snapshot(); sent != 1 || len(p.sent()) != 1 {
		t.Fatalf("control: sent %d (%d posts), want the record delivered", sent, len(p.sent()))
	}
}

// TestAppendRefusesBeforeAnyStore: a record the guard refuses is counted as
// invalid and never reaches the store, whatever is wrong with it.
func TestAppendRefusesBeforeAnyStore(t *testing.T) {
	for _, c := range cases() {
		// A record with no recordId is one Append mints for, so it is not a
		// refusal here.
		if c.Accept || c.Name == "no recordId" {
			continue
		}
		s := &recordingStore{}
		m := newCounts()
		a := newAppender(t, "https://audit.example", s, m)
		if got := a.Append(c.Record); got != DropInvalid {
			t.Errorf("%s: Append = %s, want %s", c.Name, got, DropInvalid)
		}
		if s.enqueued != 0 {
			t.Errorf("%s: a refused record reached the store", c.Name)
		}
		if _, _, d := m.snapshot(); d[DropInvalid] != 1 {
			t.Errorf("%s: dropped = %v, want invalid counted once", c.Name, d)
		}
	}
}

// recordingStore is a Store whose Enqueue can fail or block.
type recordingStore struct {
	MemStore
	enqueued int
	fail     bool
	block    bool
}

func (s *recordingStore) Enqueue(ctx context.Context, e Entry, now time.Time, bound int) (int, error) {
	s.enqueued++
	if s.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if s.fail {
		return 0, errors.New("store down")
	}
	return s.MemStore.Enqueue(ctx, e, now, bound)
}

// TestAppendNeverFailsOrHoldsTheLeg: a store error is counted as
// dropped{store} and Append returns; a store that hangs is cut at
// EnqueueTimeout. The leg calling Append goes on as before: a failed append
// leaves the leg unchanged.
func TestAppendNeverFailsOrHoldsTheLeg(t *testing.T) {
	for name, s := range map[string]*recordingStore{"failing": {fail: true}, "hanging": {block: true}} {
		m := newCounts()
		a := newAppender(t, "https://audit.example", s, m)
		start := time.Now()
		got := a.Append(fresh())
		took := time.Since(start)
		if got != DropStore {
			t.Errorf("%s store: Append = %s, want %s", name, got, DropStore)
		}
		if took > EnqueueTimeout+250*time.Millisecond {
			t.Errorf("%s store: Append took %v, want at most about EnqueueTimeout (%v)", name, took, EnqueueTimeout)
		}
		if _, _, d := m.snapshot(); d[DropStore] != 1 {
			t.Errorf("%s store: dropped = %v, want store counted once", name, d)
		}
	}
}

// TestAppendOverflowDropsTheOldestCounted: past Bound the oldest record goes
// and dropped{overflow} counts it.
func TestAppendOverflowDropsTheOldestCounted(t *testing.T) {
	s := NewMemStore()
	m := newCounts()
	a := newAppender(t, "https://audit.example", s, m)
	for i := 0; i < Bound+2; i++ {
		if got := a.Append(fresh()); got != Queued {
			t.Fatalf("record %d: Append = %s", i, got)
		}
	}
	if n, _ := s.Depth(context.Background()); n != Bound {
		t.Fatalf("Depth = %d, want Bound (%d)", n, Bound)
	}
	if _, _, d := m.snapshot(); d[DropOverflow] != 2 {
		t.Fatalf("dropped = %v, want overflow 2", d)
	}
	if s.entries[0].Seq != 3 {
		t.Fatalf("oldest left is seq %d, want 3: the oldest records drop", s.entries[0].Seq)
	}
}

// TestClassify is the answer table: what the outbox does with each of the
// Plane's answers to /append.
func TestClassify(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   verdict
		reason string
	}{
		{200, `{}`, verdictAccepted, ""},
		{202, `{"accepted":true}`, verdictAccepted, ""},
		{401, `{"error":"unknown sender"}`, verdictRetry, RetryUnknownSender},
		{401, `{"error":"unauthenticated audit record"}`, verdictDropped, DropRefused},
		{401, `unknown sender`, verdictDropped, DropRefused},
		{400, `{"error":"invalid record members: x"}`, verdictDropped, DropRefused},
		{403, `{}`, verdictDropped, DropRefused},
		{413, `{}`, verdictDropped, DropRefused},
		{409, `{}`, verdictDropped, DropConflict},
		{408, ``, verdictRetry, RetryUnavailable},
		{429, ``, verdictRetry, RetryUnavailable},
		{500, `{"error":"append failed"}`, verdictRetry, RetryUnavailable},
		{503, `{"error":"audit buffer unavailable"}`, verdictRetry, RetryUnavailable},
		{404, ``, verdictRetry, RetryUnavailable},
	} {
		v, r := classify(c.status, []byte(c.body))
		if v != c.want || r != c.reason {
			t.Errorf("%d %s: (%d, %q), want (%d, %q)", c.status, c.body, v, r, c.want, c.reason)
		}
	}
}

// plane is a fake Audit Plane answering each /append from a script.
type plane struct {
	mu      sync.Mutex
	answers []int
	bodies  [][]byte
}

func (p *plane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.URL.Path != "/append" || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var b bytes.Buffer
	_, _ = b.ReadFrom(r.Body)
	p.bodies = append(p.bodies, b.Bytes())
	status := http.StatusOK
	if len(p.answers) > 0 {
		status, p.answers = p.answers[0], p.answers[1:]
	}
	w.WriteHeader(status)
	if status == http.StatusUnauthorized {
		_, _ = w.Write([]byte(`{"error":"unknown sender"}`))
	}
}

func (p *plane) sent() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.bodies...)
}

// TestSendRetriesThenDeliversTheSameBytes: an unknown sender and an outage
// are retried; the record the Plane finally accepts is byte-identical to the
// first try, and leaves the outbox.
func TestSendRetriesThenDeliversTheSameBytes(t *testing.T) {
	p := &plane{answers: []int{401, 503, 200}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	s := NewMemStore()
	m := newCounts()
	a := newAppender(t, srv.URL+"/", s, m)
	a.Append(fresh())
	clock := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		a.cfg.Now = func() time.Time { return clock }
		a.sendOne(context.Background(), 1)
		clock = clock.Add(10 * time.Minute)
	}
	sent, retried, _ := m.snapshot()
	if sent != 1 || retried[RetryUnknownSender] != 1 || retried[RetryUnavailable] != 1 {
		t.Fatalf("sent %d, retried %v: want two retries then one delivery", sent, retried)
	}
	bodies := p.sent()
	if len(bodies) != 3 || !bytes.Equal(bodies[0], bodies[2]) {
		t.Fatalf("%d sends; the delivered bytes must equal the first try's", len(bodies))
	}
	if n, _ := s.Depth(context.Background()); n != 0 {
		t.Fatalf("Depth = %d after delivery, want 0", n)
	}
}

// TestSendDropsWhatThePlaneRefuses: a refusal leaves the outbox, counted,
// and is not tried again.
func TestSendDropsWhatThePlaneRefuses(t *testing.T) {
	for status, reason := range map[int]string{400: DropRefused, 403: DropRefused, 409: DropConflict} {
		p := &plane{answers: []int{status}}
		srv := httptest.NewServer(p)
		s := NewMemStore()
		m := newCounts()
		a := newAppender(t, srv.URL, s, m)
		a.Append(fresh())
		a.sendOne(context.Background(), 1)
		a.sendOne(context.Background(), 1)
		srv.Close()
		if _, _, d := m.snapshot(); d[reason] != 1 {
			t.Errorf("%d: dropped = %v, want %s once", status, d, reason)
		}
		if len(p.sent()) != 1 {
			t.Errorf("%d: sent %d times, want once", status, len(p.sent()))
		}
	}
}

// TestUnknownEndpointQueuesAndReports: with no endpoint known nothing is
// sent, records stay queued, and the worker reports the unknown endpoint
// with the depth.
func TestUnknownEndpointQueuesAndReports(t *testing.T) {
	s := NewMemStore()
	m := newCounts()
	a := newAppender(t, "", s, m)
	if a.EndpointKnown() {
		t.Fatal("EndpointKnown with no endpoint")
	}
	a.Append(fresh())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	select {
	case <-m.reported:
	case <-time.After(5 * time.Second):
		t.Fatal("no report")
	}
	cancel()
	<-done
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.unknown) == 0 || m.unknown[0] != 1 {
		t.Fatalf("EndpointUnknown reports %v, want the depth 1", m.unknown)
	}
	if n, _ := s.Depth(context.Background()); n != 1 {
		t.Fatalf("Depth = %d, want the record still queued", n)
	}
}

// TestRunDeliversAQueuedRecord: the worker wakes on a queued record and
// sends it.
func TestRunDeliversAQueuedRecord(t *testing.T) {
	p := &plane{}
	srv := httptest.NewServer(p)
	defer srv.Close()
	s := NewMemStore()
	m := newCounts()
	a := newAppender(t, srv.URL, s, m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	a.Append(fresh())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sent, _, _ := m.snapshot(); sent == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if sent, _, _ := m.snapshot(); sent != 1 {
		t.Fatalf("sent %d, want the queued record delivered", sent)
	}
}

// TestCloseDrainsTheMemoryOutboxAndCountsTheRest: at stop an in-memory
// outbox sends what the Plane takes and counts what is left as stopped; a
// durable one keeps its records.
func TestCloseDrainsTheMemoryOutboxAndCountsTheRest(t *testing.T) {
	p := &plane{answers: []int{200, 503}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	s := NewMemStore()
	m := newCounts()
	a := newAppender(t, srv.URL, s, m)
	l := logTo(a)
	a.Append(fresh())
	a.Append(fresh())
	a.Close()
	sent, _, d := m.snapshot()
	if sent != 1 || d[DropStopped] != 1 {
		t.Fatalf("sent %d, dropped %v: want one delivered and one counted stopped", sent, d)
	}
	if want := "gateway: audit: stopped: 1 of 2 records sent or refused (each counted); 1 left unsent, counted dropped{stopped}"; !slices.Contains(l.all(), want) {
		t.Fatalf("lines %q, want %q", l.all(), want)
	}

	durable := &durableStore{}
	m2 := newCounts()
	b := newAppender(t, srv.URL, durable, m2)
	b.Append(fresh())
	b.Close()
	if _, _, d := m2.snapshot(); d[DropStopped] != 0 {
		t.Fatalf("a durable outbox counted %v at stop; its records wait for the next start", d)
	}
}

// lines collects an appender's log lines.
type lines struct {
	mu  sync.Mutex
	got []string
	ch  chan string
}

func logTo(a *Appender) *lines {
	l := &lines{ch: make(chan string, 16)}
	a.cfg.Logf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		l.mu.Lock()
		l.got = append(l.got, line)
		l.mu.Unlock()
		l.ch <- line
	}
	return l
}

func (l *lines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.got)
}

// TestCloseReportsTheQueueBeforeItsDrain: before the drain sends anything,
// Close reports how many records are queued, as the depth and in the log, and
// counts nothing yet; a gateway killed at that point leaves that count and no
// result. When the drain ends, it logs what was sent or refused, and what was
// left unsent.
func TestCloseReportsTheQueueBeforeItsDrain(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	// A failure before the release below must not leave srv.Close waiting
	// on the blocked handler.
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	m := newCounts()
	a := newAppender(t, srv.URL, NewMemStore(), m)
	l := logTo(a)
	for range 3 {
		a.Append(fresh())
	}
	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	select {
	case line := <-l.ch:
		if want := "gateway: audit: stopping with 3 records in the in-memory outbox; sending them for up to 10s"; line != want {
			t.Fatalf("first line %q, want %q", line, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close logged nothing before its drain")
	}
	m.mu.Lock()
	depth := slices.Clone(m.depth)
	m.mu.Unlock()
	if !slices.Equal(depth, []int{3}) {
		t.Fatalf("depth reported %v before the drain, want [3]", depth)
	}
	if sent, _, d := m.snapshot(); sent != 0 || len(d) != 0 {
		t.Fatalf("sent %d, dropped %v while the first send waits: want nothing counted yet", sent, d)
	}
	close(release)
	released = true
	<-closed
	got := l.all()
	if want := "gateway: audit: stopped: 3 of 3 records sent or refused (each counted); 0 left unsent, counted dropped{stopped}"; len(got) != 2 || got[1] != want {
		t.Fatalf("lines %q, want the drain's end as %q", got, want)
	}
	if sent, _, d := m.snapshot(); sent != 3 || d[DropStopped] != 0 {
		t.Fatalf("sent %d, dropped %v: want all three delivered and none stopped", sent, d)
	}
}

// TestCloseWithNothingQueuedSaysNothing: an empty outbox reports no depth and
// logs no line at stop.
func TestCloseWithNothingQueuedSaysNothing(t *testing.T) {
	srv := httptest.NewServer(&plane{})
	defer srv.Close()
	m := newCounts()
	a := newAppender(t, srv.URL, NewMemStore(), m)
	l := logTo(a)
	a.Close()
	if got := l.all(); len(got) != 0 || len(m.depth) != 0 {
		t.Fatalf("lines %q, depth %v: want neither for an empty outbox", got, m.depth)
	}
}

// TestCloseWithNoEndpointCountsTheQueueStopped: with no audit endpoint known
// nothing can be sent, so there is no drain to announce: the depth is
// reported and every record is counted stopped at once.
func TestCloseWithNoEndpointCountsTheQueueStopped(t *testing.T) {
	m := newCounts()
	a := newAppender(t, "", NewMemStore(), m)
	l := logTo(a)
	a.Append(fresh())
	a.Append(fresh())
	a.Close()
	if want := []string{"gateway: audit: stopped: 0 of 2 records sent or refused (each counted); 2 left unsent, counted dropped{stopped}"}; !slices.Equal(l.all(), want) {
		t.Fatalf("lines %q, want %q", l.all(), want)
	}
	if _, _, d := m.snapshot(); d[DropStopped] != 2 || !slices.Equal(m.depth, []int{2}) {
		t.Fatalf("dropped %v, depth %v: want 2 stopped and the depth reported", d, m.depth)
	}
}

// depthFailsAfter is a MemStore whose Depth fails from its nth call on.
type depthFailsAfter struct {
	*MemStore
	calls, n int
}

func (s *depthFailsAfter) Depth(ctx context.Context) (int, error) {
	s.calls++
	if s.calls >= s.n {
		return 0, errors.New("depth unavailable")
	}
	return s.MemStore.Depth(ctx)
}

// TestCloseAfterAnUnreadableDrainSaysUnknown: when the outbox cannot be read
// after the drain, what was lost is logged as unknown, never as none, and
// nothing is counted.
func TestCloseAfterAnUnreadableDrainSaysUnknown(t *testing.T) {
	m := newCounts()
	s := &depthFailsAfter{MemStore: NewMemStore(), n: 2}
	a := newAppender(t, "", s, m)
	l := logTo(a)
	a.Append(fresh())
	a.Close()
	want := []string{"gateway: audit: stopped: 1 records were queued; the outbox could not be read after its drain, so what was lost is unknown"}
	if !slices.Equal(l.all(), want) {
		t.Fatalf("lines %q, want %q", l.all(), want)
	}
	if _, _, d := m.snapshot(); len(d) != 0 {
		t.Fatalf("dropped %v, want nothing counted when the depth is unknown", d)
	}
}

type durableStore struct{ MemStore }

func (*durableStore) Durable() bool { return true }

// TestNewNeedsItsWiring: no holder, key, store or metrics is an error.
func TestNewNeedsItsWiring(t *testing.T) {
	ok := Config{Holder: holder, SignPriv: testKey, Store: NewMemStore(), Metrics: newCounts()}
	for name, c := range map[string]Config{
		"no holder":  {SignPriv: ok.SignPriv, Store: ok.Store, Metrics: ok.Metrics},
		"no key":     {Holder: holder, Store: ok.Store, Metrics: ok.Metrics},
		"no store":   {Holder: holder, SignPriv: ok.SignPriv, Metrics: ok.Metrics},
		"no metrics": {Holder: holder, SignPriv: ok.SignPriv, Store: ok.Store},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
}

// timers is an injected After: each request is recorded with its channel,
// which the test fires.
type timers struct{ reqs chan timerReq }

type timerReq struct {
	d time.Duration
	c chan time.Time
}

func newTimers() *timers { return &timers{reqs: make(chan timerReq, 64)} }

func (tm *timers) After(d time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	tm.reqs <- timerReq{d, c}
	return c
}

// next is the next wait the worker asks for, skipping the report timers.
func (tm *timers) next(t *testing.T) timerReq {
	t.Helper()
	for {
		select {
		case r := <-tm.reqs:
			if r.d == 0 || r.d == reportEvery {
				continue
			}
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("the worker asked for no wait")
		}
	}
}

// raw is the next timer the worker asks for, report timers included.
func (tm *timers) raw(t *testing.T) timerReq {
	t.Helper()
	select {
	case r := <-tm.reqs:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the worker asked for no timer")
	}
	return timerReq{}
}

// clock is a settable Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// TestRunBacksOffAndANewRecordDoesNotCutTheWait: against a Plane that is
// down, each failure waits the schedule's wait, the failed record is due
// again at the end of that same wait (one draw for both: a second draw, as
// the alternating jitter here makes plain, would leave the worker waking to
// nothing due), and records queued during a wait send nothing until it ends.
func TestRunBacksOffAndANewRecordDoesNotCutTheWait(t *testing.T) {
	p := &plane{answers: []int{503, 503, 503, 503, 503, 503}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	clk := &clock{t: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	tm := newTimers()
	s := NewMemStore()
	m := newCounts()
	var mu sync.Mutex
	k := 0
	jitter := func() float64 {
		mu.Lock()
		defer mu.Unlock()
		v := []float64{0.9, 0.1}[k%2]
		k++
		return v
	}
	a, err := New(Config{Holder: holder, SignPriv: testKey, Endpoint: srv.URL, Store: s, Metrics: m,
		Now: clk.Now, After: tm.After, Jitter: jitter})
	if err != nil {
		t.Fatal(err)
	}
	a.Append(fresh())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	w1 := tm.next(t)
	if w1.d < 500*time.Millisecond || w1.d > time.Second {
		t.Fatalf("after the first failure the worker waits %v, want 0.5 s to 1 s", w1.d)
	}
	// The one record is due exactly when the wait ends: firing the timer
	// there sends it again, and the next wait doubles.
	clk.add(w1.d)
	w1.c <- clk.Now()
	w2 := tm.next(t)
	if w2.d < time.Second || w2.d > 2*time.Second {
		t.Fatalf("after the second failure the worker waits %v, want 1 s to 2 s (the record was not due when the first wait ended)", w2.d)
	}
	if bodies := p.sent(); len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("%d sends; the second must be the first record again", len(bodies))
	}
	// Queued during the wait: no send until the wait ends.
	a.Append(fresh())
	a.Append(fresh())
	time.Sleep(100 * time.Millisecond)
	if n := len(p.sent()); n != 2 {
		t.Fatalf("%d sends during the second wait, want 2: a queued record cut the backoff short", n)
	}
	clk.add(w2.d)
	w2.c <- clk.Now()
	tm.next(t)
	if n := len(p.sent()); n != 3 {
		t.Fatalf("%d sends after the second wait, want 3", n)
	}
}

// claimFails is a Store whose Claim errors.
type claimFails struct{ MemStore }

func (*claimFails) Claim(context.Context, time.Time, time.Duration) (Entry, bool, error) {
	return Entry{}, false, errors.New("store down")
}

// TestRunBacksOffWhenTheOutboxCannotBeRead: a failing read of the outbox is
// counted as a store retry and waited out on the schedule.
func TestRunBacksOffWhenTheOutboxCannotBeRead(t *testing.T) {
	tm := newTimers()
	m := newCounts()
	a, err := New(Config{Holder: holder, SignPriv: testKey, Endpoint: "https://audit.example", Store: &claimFails{}, Metrics: m,
		After: tm.After})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	w := tm.next(t)
	cancel()
	<-done
	if w.d != time.Second {
		t.Fatalf("after a failed read the worker waits %v, want the schedule's first step (1 s, no jitter)", w.d)
	}
	if _, r, _ := m.snapshot(); r[RetryStore] != 1 {
		t.Fatalf("retried = %v, want store once", r)
	}
}

// TestSendCutAtStopIsNotARetry: a send cut off because the worker is stopping
// is not counted as the Plane's failure, and the record stays queued.
func TestSendCutAtStopIsNotARetry(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	s := NewMemStore()
	m := newCounts()
	a := newAppender(t, srv.URL, s, m)
	a.Append(fresh())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if out, _ := a.sendOne(ctx, 1); out != outcomeStopped {
		t.Fatalf("outcome %d, want stopped", out)
	}
	if _, r, d := m.snapshot(); len(r) != 0 || len(d) != 0 {
		t.Fatalf("retried %v dropped %v, want nothing counted", r, d)
	}
	if n, _ := s.Depth(context.Background()); n != 1 {
		t.Fatalf("Depth = %d, want the record still queued", n)
	}
}

// TestOverflowDuringASendCountsTheRecordOnce: a record overflow takes while
// it is being sent is counted under overflow only, not also as sent.
func TestOverflowDuringASendCountsTheRecordOnce(t *testing.T) {
	var a *Appender
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < Bound; i++ {
			a.Append(fresh())
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	m := newCounts()
	a = newAppender(t, srv.URL, NewMemStore(), m)
	a.Append(fresh())
	a.sendOne(context.Background(), 1)
	sent, _, d := m.snapshot()
	if sent != 0 || d[DropOverflow] != 1 {
		t.Fatalf("sent %d, dropped %v: want the record counted once, as overflow", sent, d)
	}
}

// TestCloseTriesEveryDueRecord: at stop the drain tries each due record, not
// only those before the first failure.
func TestCloseTriesEveryDueRecord(t *testing.T) {
	p := &plane{answers: []int{503, 200}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	m := newCounts()
	a := newAppender(t, srv.URL, NewMemStore(), m)
	a.Append(fresh())
	a.Append(fresh())
	a.Close()
	sent, _, d := m.snapshot()
	if sent != 1 || d[DropStopped] != 1 || len(p.sent()) != 2 {
		t.Fatalf("sent %d, dropped %v, %d tries: want both tried, one delivered, one stopped", sent, d, len(p.sent()))
	}
}

// TestRunReportDuringABackoffKeepsTheSchedule: the minute's report, firing
// during a retry wait, reports and nothing else: no send, no reset of the
// schedule, no wake for a newly queued record; the next failure's wait keeps
// climbing.
func TestRunReportDuringABackoffKeepsTheSchedule(t *testing.T) {
	p := &plane{answers: []int{503, 503, 503, 503}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	clk := &clock{t: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	tm := newTimers()
	m := newCounts()
	a, err := New(Config{Holder: holder, SignPriv: testKey, Endpoint: srv.URL, Store: NewMemStore(), Metrics: m,
		Now: clk.Now, After: tm.After}) // no jitter: the waits are the whole steps
	if err != nil {
		t.Fatal(err)
	}
	a.Append(fresh())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	first := tm.raw(t)
	if first.d != 0 {
		t.Fatalf("first timer %v, want the immediate report", first.d)
	}
	w1 := tm.raw(t)
	if w1.d != time.Second {
		t.Fatalf("first retry wait %v, want 1 s", w1.d)
	}
	clk.add(w1.d)
	w1.c <- clk.Now()
	w2 := tm.raw(t)
	if w2.d != 2*time.Second {
		t.Fatalf("second retry wait %v, want 2 s", w2.d)
	}
	// The report fires during the wait: it asks for the next report and
	// nothing else.
	first.c <- clk.Now()
	if r := tm.raw(t); r.d != reportEvery {
		t.Fatalf("after a report the worker asked for %v, want the next report (%v)", r.d, reportEvery)
	}
	a.Append(fresh())
	time.Sleep(100 * time.Millisecond)
	if n := len(p.sent()); n != 2 {
		t.Fatalf("%d sends after a report and a queued record during the wait, want 2", n)
	}
	clk.add(w2.d)
	w2.c <- clk.Now()
	if w3 := tm.raw(t); w3.d != 4*time.Second {
		t.Fatalf("third retry wait %v, want 4 s: the schedule was reset", w3.d)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func withRecordID(r shnsdk.AuditRecord, id string) shnsdk.AuditRecord {
	r.RecordID = id
	return r
}
