// process-child is a controlled test server for Linux image lifecycle verification.
// It is built separately and mounted only by verify-process.sh; never copied to runtime paths.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type observation struct {
	Args     []string        `json:"args"`
	Env      []string        `json:"env"`
	Cwd      string          `json:"cwd"`
	PID      int             `json:"pid"`
	Parent   int             `json:"parent"`
	UID      int             `json:"uid"`
	Initial  json.RawMessage `json:"initial"`
	Posts    []string        `json:"posts"`
	Profiles []string        `json:"profiles"`
	Active   int             `json:"active"`
	Maximum  int             `json:"maximum"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "resolve-java" {
		path, err := exec.LookPath("java")
		if err == nil {
			path, err = filepath.EvalSymlinks(path)
		}
		if err != nil {
			panic(err)
		}
		fmt.Println(path)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "observe-runtime" {
		if err := observeRuntime(); err != nil {
			panic(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "sentinel" {
		if err := os.WriteFile("/tmp/second-child-launched", []byte("launched"), 0600); err != nil {
			panic(err)
		}
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "request" || os.Args[1] == "request-public") {
		c := http.Client{Timeout: 2 * time.Second}
		base := "http://127.0.0.1:18080/"
		if os.Args[1] == "request-public" {
			base = "http://localhost:8080/"
		}
		r, e := c.Get(base + os.Args[2])
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		defer r.Body.Close()
		_, e = io.Copy(os.Stdout, r.Body)
		if e != nil || r.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	address, err := controlledAddress(os.Args[1:])
	if err != nil {
		panic(err)
	}
	cwd, _ := os.Getwd()
	initial, e := os.ReadFile("/tmp/shn-validator-warm")
	if e != nil {
		panic(e)
	}
	state := observation{Args: os.Args[1:], Env: os.Environ(), Cwd: cwd, PID: os.Getpid(), Parent: os.Getppid(), UID: os.Getuid(), Initial: initial, Posts: []string{}, Profiles: []string{}}
	var mu sync.Mutex
	metadata := false
	release := make(chan struct{})
	var once sync.Once
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sig {
			fmt.Printf("child received %s\n", s)
			if os.Getenv("PROCESS_IGNORE_SIGNAL") != "1" {
				os.Exit(0)
			}
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(state)
	})
	mux.HandleFunc("/metadata-on", func(w http.ResponseWriter, r *http.Request) { mu.Lock(); metadata = true; mu.Unlock() })
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(release) }) })
	mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) { os.Exit(7) })
	mux.HandleFunc("/marker", func(w http.ResponseWriter, r *http.Request) {
		b, e := os.ReadFile("/tmp/shn-validator-warm")
		if e != nil {
			http.Error(w, "missing", 500)
			return
		}
		w.Write(b)
	})
	mux.HandleFunc("/fhir/metadata", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		on := metadata
		mu.Unlock()
		if !on {
			http.Error(w, "held", 503)
			return
		}
		io.WriteString(w, `{"resourceType":"CapabilityStatement"}`)
	})
	mux.HandleFunc("/fhir/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/$validate") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		mu.Lock()
		state.Posts = append(state.Posts, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/fhir/"), "/$validate"))
		state.Profiles = append(state.Profiles, r.URL.Query().Get("profile"))
		state.Active++
		if state.Active > state.Maximum {
			state.Maximum = state.Active
		}
		mu.Unlock()
		defer func() { mu.Lock(); state.Active--; mu.Unlock() }()
		<-release
		if os.Getenv("PROCESS_DROP_RESPONSE") == "1" {
			c, _, e := w.(http.Hijacker).Hijack()
			if e == nil {
				c.Close()
			}
			return
		}
		serveValidation(w, r, body)
	})
	fmt.Println("controlled child started")
	if e := http.ListenAndServe(address, mux); e != nil {
		panic(e)
	}
}

// serveValidation contains only the controlled response selection. Request
// accounting, release, and uncertain transport stay in the process handler.
func serveValidation(w http.ResponseWriter, r *http.Request, body []byte) {
	profile := r.URL.Query().Get("profile")
	if profile == "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|9.9.9" || profile == "https://example.org/fhir/StructureDefinition/unavailable-profile" {
		var resource struct {
			ResourceType string `json:"resourceType"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/fhir/ClaimResponse/$validate" || json.Unmarshal(body, &resource) != nil || resource.ResourceType != "ClaimResponse" {
			http.Error(w, "invalid controlled profile request", http.StatusBadRequest)
			return
		}
		diagnostic, _ := json.Marshal("Invalid profile. Failed to retrieve explicitly requested profile with url=" + profile)
		w.Header().Set("Content-Type", "application/fhir+json")
		fmt.Fprintf(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"Validation_VAL_Profile_Unknown"}]},"diagnostics":%s}]}`, diagnostic)
		return
	}
	var resource struct {
		ResourceType string `json:"resourceType"`
	}
	if json.Unmarshal(body, &resource) == nil && resource.ResourceType == "Bundle" {
		// Lifecycle instrumentation returns the committed rejection fixtures;
		// real HAPI verdicts are certified separately by verify.sh.
		for token, mutation := range map[string]string{`"L9999"`: "hcpcs", `"98"`: "pos", `"invalid-reference-type"`: "encounter"} {
			if !strings.Contains(string(body), token) {
				continue
			}
			dir := "/process-fixtures"
			if line := os.Getenv("SHN_IG_LINE"); line == "2.1" || line == "2.2" {
				dir = filepath.Join(dir, line)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "pas-response-"+mutation+"-errors.json"))
			if err != nil {
				http.Error(w, "controlled fixture unavailable", 500)
				return
			}
			w.Write(raw)
			return
		}
	} else if resource.ResourceType == "Claim" && strings.Contains(string(body), `"resourceType":"Patient"`) && strings.Contains(string(body), `"id":"probe-encounter"`) {
		// The encounter target-type control: the contained Encounter was
		// swapped for a Patient; answer with the committed pinned outcome.
		dir := "/process-fixtures"
		if line := os.Getenv("SHN_IG_LINE"); line == "2.1" || line == "2.2" {
			dir = filepath.Join(dir, line)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "claim-encounter-target-errors.json"))
		if err != nil {
			http.Error(w, "controlled fixture unavailable", 500)
			return
		}
		w.Write(raw)
		return
	} else if strings.Contains(string(body), `"valueBoolean":true`) {
		io.WriteString(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"Extension_EXT_Type"}]},"diagnostics":"The Extension 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode' definition allows for the types [CodeableConcept] but found type boolean","expression":["ClaimResponse.item[0].adjudication[0].extension[0].extension[0]"]}]}`)
		return
	}
	io.WriteString(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"Validation successful"}]}`)
}
