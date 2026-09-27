package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Each default client waits at a cold boundary, then qualifies against its own
// lane the whole corpus once. Each client's lane is a real 2.1 lane replayed
// strictly (lane-2.1-metadata and lane-2.1-warm beside the validator code). The
// cold 503 is authored: a lane still booting refuses the connection, which is
// no HTTP answer to record; the 503 stands for a public boundary not yet open.
func TestIndependentDefaultClientsWaitThenEachQualifyCompleteCorpus(t *testing.T) {
	const clients = 6
	lanes := make([]*recordedLane, clients)
	for id := range lanes {
		lanes[id] = newRecordedLane(t, nil, "lane-2.1-metadata", "lane-2.1-warm")
	}
	var admitted atomic.Bool
	var mu sync.Mutex
	entered := make([]bool, clients)
	cold := make(chan int, clients)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		id, e := strconv.Atoi(parts[0])
		if e != nil || id < 0 || id >= clients || len(parts) != 2 {
			http.Error(w, "unknown client", 400)
			return
		}
		if !admitted.Load() {
			mu.Lock()
			if !entered[id] {
				entered[id] = true
				cold <- id
			}
			if r.Method != http.MethodGet || parts[1] != "fhir/metadata" {
				t.Error("validation before public admission")
			}
			mu.Unlock()
			http.Error(w, "cold public boundary", 503)
			return
		}
		lane := r.Clone(r.Context())
		lane.URL.Path = "/" + parts[1]
		lane.URL.RawPath = ""
		lanes[id].Config.Handler.ServeHTTP(w, lane)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	results := make(chan error, clients)
	for i := 0; i < clients; i++ {
		workers.Add(1)
		go func(id int) {
			defer workers.Done()
			_, manager, e := discoverValidatorLanes(ctx, func(string) string { return "" }, []string{"pa.dtr@2.1"}, shnsdk.NewFakeValidator(), config{FHIRValidateURL22: "http://explicit.invalid/fhir"}, func(string) string { return fmt.Sprintf("%s/%d/fhir", server.URL, id) }, qualifyDefaultLane)
			if manager != nil {
				defer manager.Close()
			}
			if e == nil && !manager.defaults["2.1"].Ready() {
				e = fmt.Errorf("client %d returned before qualification", id)
			}
			results <- e
		}(i)
	}
	for i := 0; i < clients; i++ {
		select {
		case <-cold:
		case <-time.After(3 * time.Second):
			t.Fatal("client did not reach cold admission boundary")
		}
	}
	for id, lane := range lanes {
		if lane.gets.Load()+lane.posts.Load() != 0 {
			t.Errorf("client %d reached its lane while cold", id)
		}
	}
	select {
	case e := <-results:
		t.Fatalf("declared client returned while cold: %v", e)
	default:
	}
	admitted.Store(true)
	for i := 0; i < clients; i++ {
		select {
		case e := <-results:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("client qualification did not finish")
		}
	}
	workers.Wait()
	for id, lane := range lanes {
		if int(lane.posts.Load()) != lanequalify.RowCount("2.1") || lane.gets.Load() != 1 {
			t.Fatalf("client %d asked metadata %d times and submitted %d/%d rows", id, lane.gets.Load(), lane.posts.Load(), lanequalify.RowCount("2.1"))
		}
	}
}
