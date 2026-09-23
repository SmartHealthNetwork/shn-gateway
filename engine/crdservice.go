// crdservice.go — choosing the payer's CDS service for a CDS Hooks request.
//
// The request's hook is the participant's own clinical event and is never
// changed. The payer gateway reads the payer's CDS service listing
// ({CDS base}/cds-services) and sends the request to the one service whose
// hook is the request's hook. PAYER_DAVINCI_CRD_SERVICE_ID and
// PAYER_DAVINCI_DISPATCH_SERVICE_ID optionally name the service instead; the
// named service must still be listed for the request's hook.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// cdsServiceListingTTL is how long a payer's CDS service listing is reused
// before it is read again.
const cdsServiceListingTTL = 5 * time.Minute

// cdsServiceListingTimeout bounds one read of the listing.
const cdsServiceListingTimeout = 10 * time.Second

// cdsServiceListingRetryAfter is how long a failed read of the listing is
// remembered: requests in that window are not answered by another read (they
// use the last listing read, or are refused when there is none), and the
// first request after it reads the listing again.
const cdsServiceListingRetryAfter = 5 * time.Second

// cdsServiceListingMaxAge bounds how old a listing may be and still be used
// when it cannot be read again. Past it, requests are refused until the
// listing is read.
const cdsServiceListingMaxAge = time.Hour

// CDSService is one entry of a CDS service listing.
type CDSService struct {
	ID   string `json:"id"`
	Hook string `json:"hook"`
}

// crdLegHooks are the hooks each CRD leg carries: an ordering hook on the
// order-select leg, the dispatch hook on the order-dispatch leg.
var crdLegHooks = map[string][]string{
	"crd-order-select":   {"order-select", "order-sign"},
	"crd-order-dispatch": {"order-dispatch"},
}

// DiscoverCDSServices reads a CDS service listing from base + "/cds-services".
// A non-2xx answer, an unreadable body or a listing without a services array
// is an error; an entry without an id or a hook is an error too. headers are
// the partner's fixed request headers (WithBackendHeaders), nil for none.
func DiscoverCDSServices(ctx context.Context, client *http.Client, base string, headers http.Header) ([]CDSService, error) {
	ctx, cancel := context.WithTimeout(ctx, cdsServiceListingTimeout)
	defer cancel()
	url := base + "/cds-services"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("engine: build GET %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header[k] = append([]string(nil), v...)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("engine: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	if err != nil {
		return nil, fmt.Errorf("engine: read %s: %w", url, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("engine: GET %s returned %s", url, resp.Status)
	}
	var listing struct {
		Services *[]CDSService `json:"services"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("engine: parse %s: %w", url, err)
	}
	if listing.Services == nil {
		return nil, fmt.Errorf("engine: %s lists no services array", url)
	}
	for i, s := range *listing.Services {
		if s.ID == "" || s.Hook == "" {
			return nil, fmt.Errorf("engine: %s: service %d has no id or no hook", url, i)
		}
	}
	return *listing.Services, nil
}

// cdsServiceListing caches the payer's listing. A listing younger than
// cdsServiceListingTTL is used as is; an older one is read again. One read
// runs at a time and nobody holds the lock while it runs: requests that still
// have a listing younger than cdsServiceListingMaxAge use it meanwhile, and
// requests without one wait for the read. A failed read keeps the last listing
// (up to cdsServiceListingMaxAge) and is remembered for
// cdsServiceListingRetryAfter.
type cdsServiceListing struct {
	mu       sync.Mutex
	services []CDSService
	read     time.Time
	failed   time.Time
	err      error
	// reading is closed when the read in progress ends; nil when none runs.
	reading chan struct{}
	// joined, when set, is called each time a request starts waiting for the
	// read in progress.
	joined func()
}

// PrimeCDSServices stores a listing read elsewhere (at boot) so the first
// request does not read it again.
func (n *nativeResponder) PrimeCDSServices(services []CDSService) {
	n.cds.mu.Lock()
	defer n.cds.mu.Unlock()
	n.cds.services = slices.Clone(services)
	n.cds.read = n.clock()
	n.cds.failed, n.cds.err = time.Time{}, nil
}

func (n *nativeResponder) cdsServices(ctx context.Context) ([]CDSService, error) {
	for {
		n.cds.mu.Lock()
		now := n.clock()
		held := !n.cds.read.IsZero()
		if held && now.Sub(n.cds.read) < cdsServiceListingTTL {
			services := n.cds.services
			n.cds.mu.Unlock()
			return services, nil
		}
		usable := held && now.Sub(n.cds.read) < cdsServiceListingMaxAge
		services := n.cds.services
		if n.cds.err != nil && now.Sub(n.cds.failed) < cdsServiceListingRetryAfter {
			err := n.cds.err
			n.cds.mu.Unlock()
			if usable {
				return services, nil
			}
			return nil, err
		}
		if reading := n.cds.reading; reading != nil {
			joined := n.cds.joined
			n.cds.mu.Unlock()
			if usable {
				return services, nil
			}
			if joined != nil {
				joined()
			}
			select {
			case <-reading:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		reading := make(chan struct{})
		n.cds.reading = reading
		n.cds.mu.Unlock()
		return n.readCDSServices(ctx, reading)
	}
}

// readCDSServices performs the one read in progress and publishes its result.
// The read is not tied to the lifetime of the request that started it: other
// requests may be waiting for it (it is bounded by cdsServiceListingTimeout).
func (n *nativeResponder) readCDSServices(ctx context.Context, reading chan struct{}) ([]CDSService, error) {
	services, err := DiscoverCDSServices(context.WithoutCancel(ctx), n.client, n.cdsBaseURL, n.backendHeaders)
	n.cds.mu.Lock()
	defer n.cds.mu.Unlock()
	defer close(reading)
	n.cds.reading = nil
	now := n.clock()
	if err != nil {
		n.cds.failed, n.cds.err = now, err
		if !n.cds.read.IsZero() && now.Sub(n.cds.read) < cdsServiceListingMaxAge {
			log.Printf("gateway: payer CDS service listing could not be read again (%v); using the listing read at %s", err, n.cds.read.UTC().Format(time.RFC3339))
			return n.cds.services, nil
		}
		return nil, err
	}
	n.cds.services, n.cds.read = services, now
	n.cds.failed, n.cds.err = time.Time{}, nil
	return services, nil
}

// serviceOverride is the configured service id for leg ("" when none).
func (n *nativeResponder) serviceOverride(leg string) string {
	if leg == "crd-order-dispatch" {
		return n.crdDispatchServiceID
	}
	return n.crdServiceID
}

// offeredHooksRefusal is the payer gateway's own refusal of a hook the payer
// does not serve on this leg: 422 with the hooks it does serve.
func offeredHooksRefusal(msg string, offered []string) (LegResult, error) {
	if offered == nil {
		offered = []string{}
	}
	body, err := json.Marshal(struct {
		Error   string   `json:"error"`
		Offered []string `json:"offered"`
	}{msg, offered})
	if err != nil {
		return LegResult{}, fmt.Errorf("engine: encode hook refusal: %w", err)
	}
	p, err := relay.Authored(relay.BuilderGatewayRefusal, body, "application/json")
	if err != nil {
		return LegResult{}, fmt.Errorf("engine: seal hook refusal: %w", err)
	}
	return LegResult{Status: http.StatusUnprocessableEntity, Message: msg, Response: p}, nil
}

// selectCRDService returns the payer's CDS service id for the CDS Hooks
// request in on leg. A refusal (nothing is sent to the payer) comes back as a
// LegResult with a Status:
//
//   - 400 when the request names no hook, or a hook this leg does not carry;
//   - 502 when the payer's service listing cannot be read;
//   - 422 with the payer's offered hooks when no listed service is for the
//     hook, or when the configured service is not listed or is for another
//     hook;
//   - 422 when several listed services are for the hook and none is
//     configured.
func (n *nativeResponder) selectCRDService(ctx context.Context, leg string, in relay.Body) (string, LegResult, error) {
	var req struct {
		Hook string `json:"hook"`
	}
	if ex, ok := ctx.Value(nativeExchangeKey{}).(ExchangeContext); ok && ex.crdHook != "" {
		if ex.legType != leg || !validCRDHook(leg, ex.crdHook) {
			return "", LegResult{Status: http.StatusForbidden, Message: "context_invalid"}, nil
		}
		req.Hook = ex.crdHook
	} else if err := relay.Decode(in, &req); err != nil {
		return "", LegResult{Status: http.StatusBadRequest, Message: "parse cds request failed"}, nil
	}
	if req.Hook == "" {
		return "", LegResult{Status: http.StatusBadRequest, Message: "CDS Hooks request names no hook"}, nil
	}
	if !slices.Contains(crdLegHooks[leg], req.Hook) {
		return "", LegResult{Status: http.StatusBadRequest, Message: fmt.Sprintf("hook %s is not carried on %s", req.Hook, leg)}, nil
	}
	services, err := n.cdsServices(ctx)
	if err != nil {
		return "", LegResult{Status: http.StatusBadGateway, Message: "payer CDS service listing unavailable"}, nil
	}
	var offered, matches []string
	hookOf := map[string]string{}
	for _, s := range services {
		if !slices.Contains(offered, s.Hook) {
			offered = append(offered, s.Hook)
		}
		if _, seen := hookOf[s.ID]; !seen {
			hookOf[s.ID] = s.Hook
		}
		if s.Hook == req.Hook {
			matches = append(matches, s.ID)
		}
	}
	if id := n.serviceOverride(leg); id != "" {
		hook, listed := hookOf[id]
		switch {
		case !listed:
			lr, err := offeredHooksRefusal(fmt.Sprintf("payer offers no CDS service %s", id), offered)
			return "", lr, err
		case hook != req.Hook:
			lr, err := offeredHooksRefusal(fmt.Sprintf("payer CDS service %s is for hook %s, not %s", id, hook, req.Hook), []string{hook})
			return "", lr, err
		}
		return id, LegResult{}, nil
	}
	switch len(matches) {
	case 1:
		return matches[0], LegResult{}, nil
	case 0:
		lr, err := offeredHooksRefusal("payer offers no CDS service for hook "+req.Hook, offered)
		return "", lr, err
	default:
		return "", LegResult{Status: http.StatusUnprocessableEntity, Message: "payer offers several CDS services for hook " + req.Hook}, nil
	}
}
