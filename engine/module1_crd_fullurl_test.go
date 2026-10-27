//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// v5URN is an absolute urn:uuid naming an RFC 4122 version 5 UUID.
var v5URN = regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func collectionEntries(t *testing.T, bundle []byte) []struct {
	FullURL  string          `json:"fullUrl"`
	Resource json.RawMessage `json:"resource"`
} {
	t.Helper()
	var b struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	if b.ResourceType != "Bundle" || b.Type != "collection" {
		t.Fatalf("not a collection Bundle: %s", bundle)
	}
	return b.Entry
}

// The draftOrders and deviceHistory Bundles the provider's gateway writes
// name each entry by an absolute urn:uuid, as FHIR R4 requires (a relative
// Type/id is refused by the validator): the same seed builds the same bytes,
// each entry and each seed has its own, and each resource is carried as sent.
func TestCollectionBundleFullURLsAreAbsoluteAndStable(t *testing.T) {
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-1","status":"draft"}`)
	dr := []byte(` {"resourceType":"DeviceRequest","id":"dr-1"} `)
	const seed = "d1577c69-dfbe-44ad-ba6d-3e05e953b2ea/draftOrders"
	first, err := collectionBundle(seed, sr, dr)
	if err != nil {
		t.Fatal(err)
	}
	again, err := collectionBundle(seed, sr, dr)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(again) {
		t.Fatalf("two builds of the same Bundle differ:\n%s\n%s", first, again)
	}
	entries := collectionEntries(t, first)
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2", len(entries))
	}
	for i, e := range entries {
		if !v5URN.MatchString(e.FullURL) {
			t.Errorf("entry %d: fullUrl %q is not an absolute urn:uuid of a version 5 UUID", i, e.FullURL)
		}
		if strings.Contains(e.FullURL, "/") {
			t.Errorf("entry %d: fullUrl %q is a Type/id", i, e.FullURL)
		}
	}
	if entries[0].FullURL != "urn:uuid:cca38da6-7917-5cdb-b66f-ac0da785b2da" {
		t.Errorf("entry 0: fullUrl %q, want the version 5 UUID of %q/0 in the URL name space", entries[0].FullURL, seed)
	}
	if entries[0].FullURL == entries[1].FullURL {
		t.Errorf("both entries carry fullUrl %q", entries[0].FullURL)
	}
	if string(entries[0].Resource) != string(sr) || string(entries[1].Resource) != strings.TrimSpace(string(dr)) {
		t.Errorf("the resources are not carried as sent: %s", first)
	}
	other, err := collectionBundle("another-request/draftOrders", sr, dr)
	if err != nil {
		t.Fatal(err)
	}
	if o := collectionEntries(t, other); o[0].FullURL == entries[0].FullURL || o[1].FullURL == entries[1].FullURL {
		t.Errorf("another request's Bundle reuses this one's fullUrls: %s", other)
	}
}

// entryFullURL never names an entry by its resource's own id: a HAPI-based
// payer replaces the resource's id with a fullUrl of urn:uuid:<that id>. An
// order whose id is the UUID the entry's name gives gets another one.
func TestEntryFullURLNeverTheResourceID(t *testing.T) {
	taken := nameBasedUUID("s/0")
	got := entryFullURL("s", 0, taken)
	if got == "urn:uuid:"+taken || !v5URN.MatchString(got) {
		t.Fatalf("entryFullURL for a resource whose id is %s = %q", taken, got)
	}
	if entryFullURL("s", 0, "sr-1") != "urn:uuid:"+taken {
		t.Fatalf("a resource with another id does not get the name's own UUID")
	}
}

// nameBasedUUID is RFC 4122's version 5 in the URL name space: the vector
// any version 5 implementation gives for http://www.example.com.
func TestNameBasedUUIDIsVersion5(t *testing.T) {
	if got := nameBasedUUID("http://www.example.com"); got != "2d32a3f1-fac2-52a6-ac52-fe9e87e628ea" {
		t.Fatalf("nameBasedUUID(http://www.example.com) = %s", got)
	}
}

// crdFullURLRecords are a member's records an originated CRD request
// carries: its Patient and a Coverage search result, with no history.
func crdFullURLRecords() crdOriginRecords {
	return crdOriginRecords{
		member: "MBR-1", sorID: "MBR-1",
		patient:  []byte(`{"resourceType":"Patient","id":"MBR-1"}`),
		coverage: []byte(`{"resourceType":"Bundle","type":"searchset","total":0}`),
		history:  map[string][]byte{},
	}
}

// requestFullURLs is every fullUrl of the Bundle at key (context.draftOrders
// or prefetch.deviceHistory) in a built CRD request.
func requestFullURLs(t *testing.T, body []byte, where, key string) []string {
	t.Helper()
	var req map[string]json.RawMessage
	var part map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(req[where], &part); err != nil {
		t.Fatalf("the request's %s: %v", where, err)
	}
	bundle, ok := part[key]
	if !ok {
		t.Fatalf("the request carries no %s.%s: %s", where, key, body)
	}
	var out []string
	for _, e := range collectionEntries(t, bundle) {
		out = append(out, e.FullURL)
	}
	return out
}

// The same exchange builds the same request bytes, so the same fullUrls,
// and another exchange its own: the draftOrders Bundle of an order-sign
// request, and the deviceHistory Bundle an order-dispatch request carries
// when the provider's search returned none.
func TestOriginatedCRDRequestFullURLsFollowTheExchange(t *testing.T) {
	g := &Gateway{cfg: Config{NPI: "1234567893"}}
	order := []byte(`{"resourceType":"ServiceRequest","id":"sr-1","status":"draft","intent":"order","subject":{"reference":"Patient/MBR-1"}}`)
	sign := func(cid string) []byte {
		t.Helper()
		b, err := g.originatedOrderingRequest(context.Background(), hookOrderSign, cid, crdFullURLRecords(), order)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	device := []byte(`{"resourceType":"DeviceRequest","id":"dr-1","status":"active","intent":"order","subject":{"reference":"Patient/MBR-1"}}`)
	dispatch := func(cid string) []byte {
		t.Helper()
		b, err := g.originatedDispatchRequest(cid, crdFullURLRecords(), device, "Organization/dme-1")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, c := range []struct {
		name, where, key string
		build            func(string) []byte
	}{
		{"order-sign draftOrders", "context", "draftOrders", sign},
		{"order-dispatch deviceHistory", "prefetch", "deviceHistory", dispatch},
	} {
		first, again := c.build("exchange-a"), c.build("exchange-a")
		if string(first) != string(again) {
			t.Errorf("%s: the same exchange built two requests:\n%s\n%s", c.name, first, again)
		}
		urls := requestFullURLs(t, first, c.where, c.key)
		other := requestFullURLs(t, c.build("exchange-b"), c.where, c.key)
		if len(urls) != 1 || !v5URN.MatchString(urls[0]) {
			t.Fatalf("%s: fullUrls %v, want one version 5 urn:uuid", c.name, urls)
		}
		if other[0] == urls[0] {
			t.Errorf("%s: another exchange reuses fullUrl %s", c.name, urls[0])
		}
	}
}
