package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestIndependentDefaultClientsWaitThenEachQualifyCompleteCorpus(t *testing.T) {
	var rows []struct {
		Path, Profile, SHA256 string
		Outcome               json.RawMessage
	}
	raw, err := os.ReadFile("testdata/qualification-2.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	const clients = 6
	var admitted atomic.Bool
	var mu sync.Mutex
	counts := make([]int, clients)
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
		if r.Method == http.MethodGet && parts[1] == "fhir/metadata" {
			io.WriteString(w, `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`)
			return
		}
		mu.Lock()
		index := counts[id]
		counts[id]++
		mu.Unlock()
		if index >= len(rows) {
			http.Error(w, "repeated corpus", 400)
			return
		}
		row := rows[index]
		body, e := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		if e != nil || r.Method != http.MethodPost || "/"+parts[1] != row.Path || r.URL.Query().Get("profile") != row.Profile || hex.EncodeToString(sum[:]) != row.SHA256 {
			http.Error(w, "wrong exact corpus request", 400)
			return
		}
		w.Write(row.Outcome)
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
	mu.Lock()
	for _, count := range counts {
		if count != 0 {
			t.Error("cold corpus request")
		}
	}
	mu.Unlock()
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
	for id, count := range counts {
		if count != len(rows) {
			t.Fatalf("client %d submitted %d/%d rows", id, count, len(rows))
		}
	}
}
