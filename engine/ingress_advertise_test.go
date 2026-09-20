package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// TestParseAdvertisedCDSHooks: the override names hooks the network carries,
// each once, at least one; anything else refuses so a typo never advertises
// nothing or everything by accident.
func TestParseAdvertisedCDSHooks(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want []string
		err  string
	}{
		{"order-sign,order-select", []string{"order-sign", "order-select"}, ""},
		{" order-sign , order-select , order-dispatch ", []string{"order-sign", "order-select", "order-dispatch"}, ""},
		{"order-dispatch", []string{"order-dispatch"}, ""},
		{"order-sign,order-signn", nil, `unknown hook "order-signn"`},
		{"appointment-book", nil, `unknown hook "appointment-book"`},
		{"order-sign,order-sign", nil, `listed twice`},
		{"", nil, "no hooks named"},
		{" , ", nil, "no hooks named"},
	} {
		got, err := ParseAdvertisedCDSHooks(c.raw)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%q: unexpected error %v", c.raw, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%q: error = %v, want one containing %q", c.raw, err, c.err)
		case !slices.Equal(got, c.want):
			t.Errorf("%q: hooks = %q, want %q", c.raw, got, c.want)
		}
	}
}

func listedIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var listing struct {
		Services []struct {
			ID string `json:"id"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range listing.Services {
		ids = append(ids, s.ID)
	}
	return ids
}

// TestCDSDiscovery_NarrowsToAdvertisedHooks: with no override the listing is
// every service the network carries; with one it is exactly the named hooks'
// services in discovery order, and only those dispatch — an unadvertised id
// is unknown, and the refusal names what is offered.
func TestCDSDiscovery_NarrowsToAdvertisedHooks(t *testing.T) {
	all := &Gateway{cfg: Config{}}
	body, err := all.cdsDiscoveryJSON()
	if err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, body); !slices.Equal(got, []string{"shn-order-sign", "shn-order-select", "shn-order-dispatch"}) {
		t.Fatalf("unnarrowed listing = %q", got)
	}
	if _, offered, ok := all.advertisedCDSServiceByID("shn-order-dispatch"); !ok || len(offered) != 3 {
		t.Fatalf("unnarrowed dispatch: ok=%v offered=%q", ok, offered)
	}

	narrowed := &Gateway{cfg: Config{AdvertisedCDSHooks: []string{"order-select", "order-sign"}}}
	body, err = narrowed.cdsDiscoveryJSON()
	if err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, body); !slices.Equal(got, []string{"shn-order-sign", "shn-order-select"}) {
		t.Fatalf("narrowed listing = %q (discovery order, not configuration order)", got)
	}
	svc, offered, ok := narrowed.advertisedCDSServiceByID("shn-order-dispatch")
	if ok || svc.ID != "" {
		t.Fatalf("unadvertised service dispatched: %+v", svc)
	}
	if !slices.Equal(offered, []string{"shn-order-sign", "shn-order-select"}) {
		t.Fatalf("refusal offers %q", offered)
	}
	if _, _, ok := narrowed.advertisedCDSServiceByID("shn-order-sign"); !ok {
		t.Fatal("advertised service not dispatched")
	}
}
