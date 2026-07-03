package scenariodriver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// pkgFixture is a br-payer-shaped Parameters{return: Bundle{Questionnaire}} package.
const pkgFixture = `{"resourceType":"Parameters","parameter":[{"name":"return","resource":{
  "resourceType":"Bundle","type":"collection","entry":[
    {"resource":{"resourceType":"Questionnaire","id":"q1","url":"http://example.org/Questionnaire/home-oxygen"}}]}}]}`

func TestOriginateThroughBRProvider(t *testing.T) {
	var bffPath, bffServer, bypass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/fhir/metadata":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/cds-services/order-select-crd":
			bffPath, bffServer, bypass = r.URL.Path, r.URL.Query().Get("server"), r.Header.Get("X-Bypass-Auth")
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if req["hook"] != "order-sign" {
				t.Errorf("hook = %v", req["hook"])
			}
			w.Write([]byte(cardsFixture))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	d := New(Config{BFFURL: srv.URL, IngressBase: "http://provider-gw:8080"})
	if err := d.BRProviderReady(); err != nil {
		t.Fatalf("BRProviderReady: %v", err)
	}
	r, err := d.OriginateThroughBRProvider("approve", "MBR-COVERED")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != http.StatusOK || r.Covered() != "covered" || r.Member != "MBR-COVERED" {
		t.Fatalf("result = %+v", r)
	}
	if len(r.Questionnaires()) == 0 {
		t.Fatal("approve card must carry questionnaire canonicals")
	}
	if bffPath == "" || bffServer != "http://provider-gw:8080/cds-services" || bypass != "true" {
		t.Fatalf("BFF call: path=%q server=%q bypass=%q", bffPath, bffServer, bypass)
	}
	if _, err := d.OriginateThroughBRProvider("nonsense", "M"); err == nil {
		t.Fatal("want error for unknown scenario")
	}
}

func TestPopulateViaBRProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/dtr/populate" {
			t.Errorf("path %s", r.URL.Path)
		}
		var params struct {
			Parameter []struct {
				Name     string          `json:"name"`
				Resource json.RawMessage `json:"resource"`
			} `json:"parameter"`
		}
		json.NewDecoder(r.Body).Decode(&params)
		var names []string
		sawBundlePackage := false
		for _, p := range params.Parameter {
			names = append(names, p.Name)
			if p.Name == "packagebundle" && bytes.Contains(p.Resource, []byte(`"Bundle"`)) &&
				!bytes.Contains(p.Resource, []byte(`"Parameters"`)) {
				sawBundlePackage = true
			}
		}
		if len(names) != 3 || !sawBundlePackage {
			t.Errorf("populate params = %v (packagebundle must be the UNWRAPPED Bundle)", names)
		}
		w.Write([]byte(`{"resourceType":"QuestionnaireResponse","id":"qr1"}`))
	}))
	defer srv.Close()
	d := New(Config{BFFURL: srv.URL})
	qr, err := d.PopulateViaBRProvider(DTRPackage{
		Body: []byte(pkgFixture), Canonical: "http://example.org/Questionnaire/home-oxygen", Member: "MBR-COVERED",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(qr, []byte("QuestionnaireResponse")) {
		t.Fatalf("populate did not return a QR: %s", qr)
	}
}

func TestBuildGoldenPASBundleWithQR(t *testing.T) {
	out, err := BuildGoldenPASBundleWithQR("MBR-COVERED", []byte(`{"resourceType":"QuestionnaireResponse","id":"qr-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"supportingInfo"`, `QuestionnaireResponse/qr-x`, `Patient/MBR-COVERED`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("golden+QR bundle missing %s", want)
		}
	}
	// QR-less fallback (R-5): returns the plain rebound golden.
	plain, err := BuildGoldenPASBundleWithQR("MBR-COVERED", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain, []byte(`"supportingInfo"`)) {
		t.Fatal("QR-less bundle must not carry supportingInfo")
	}
}

func TestFetchDTRPackage_RequiresCanonical(t *testing.T) {
	d := New(Config{})
	if _, err := d.FetchDTRPackage(BRPResult{Member: "M"}); err == nil {
		t.Fatal("want error when the CRD card carried no questionnaire canonical")
	}
}
