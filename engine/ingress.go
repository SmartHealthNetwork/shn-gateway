// Native Da Vinci ingress adapters and legacy metadata readers. The shared
// native dispatcher authenticates context, prepares the participant boundary
// and delivers through the existing originator pipeline.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// resolverFromResources builds a resolveRef that matches "<Type>/<id>" against a flat list of FHIR
// resource JSON blobs (a Bundle's entry.resource, a CDS Hooks prefetch value, or a Parameters
// parameter.resource). It is the shared core of the inbound-payload payor resolvers: an EXTERNAL
// Coverage.payor Organization a conformant partner references lives among THESE resources, not in the
// provider SoR — resolving it here (not via SoR) is the Finding-1 fix. refs are unique (Type/id), so
// the first match is deterministic regardless of the source's iteration order.
func resolverFromResources(resources [][]byte) func(ref string) ([]byte, bool) {
	return func(ref string) ([]byte, bool) {
		for _, res := range resources {
			var rt struct {
				ResourceType string `json:"resourceType"`
				ID           string `json:"id"`
			}
			if decodeMessage(res, &rt) != nil || rt.ResourceType == "" || rt.ID == "" {
				continue
			}
			if rt.ResourceType+"/"+rt.ID == ref {
				return res, true
			}
		}
		return nil, false
	}
}

// bundleRefResolver resolves "<Type>/<id>" against the entries of an inbound FHIR Bundle (payor
// Organizations et al. live IN the partner's bundle, not the provider SoR). Used by the PAS ingress.
func bundleRefResolver(bundleJSON []byte) func(ref string) ([]byte, bool) {
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := decodeMessage(bundleJSON, &b); err != nil {
		return func(string) ([]byte, bool) { return nil, false }
	}
	resources := make([][]byte, 0, len(b.Entry))
	for _, e := range b.Entry {
		if len(e.Resource) > 0 {
			resources = append(resources, e.Resource)
		}
	}
	return resolverFromResources(resources)
}

// prefetchResources flattens a CDS Hooks request's prefetch values into a
// resource list — an external payor Organization arrives as another prefetch
// value (a resource, or an entry of a Bundle value), so the CRD ingress
// resolves against it. Every value is read, whatever its key, in key order;
// null values are skipped.
func prefetchResources(prefetch map[string][]byte) [][]byte {
	keys := make([]string, 0, len(prefetch))
	for k := range prefetch {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([][]byte, 0, len(prefetch))
	for _, key := range keys {
		v := bytes.TrimSpace(prefetch[key])
		if len(v) == 0 || string(v) == "null" {
			continue
		}
		out = append(out, v)
		var b struct {
			ResourceType string `json:"resourceType"`
			Entry        []struct {
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		}
		if decodeMessage(v, &b) != nil || b.ResourceType != "Bundle" {
			continue
		}
		for _, e := range b.Entry {
			if len(e.Resource) > 0 {
				out = append(out, e.Resource)
			}
		}
	}
	return out
}

// crdIngressRecipient routes a prepared CDS Hooks request by the coverage it
// carries (FR-G40; no default): a bare Coverage or a Bundle of Coverages that
// name one payer. The payer's Organization is looked up among the request's
// own prefetch values and, for a coverage read from the system of record, in
// that system.
func (g *Gateway) crdIngressRecipient(ctx context.Context, prepared crdIngressRequest) (string, int, string) {
	coverage, carried := prepared.values["coverage"]
	switch {
	case !carried && prepared.coverageStatus != 0:
		return "", prepared.coverageStatus, prepared.coverageMsg
	case !carried || string(bytes.TrimSpace(coverage)) == "null":
		return "", http.StatusUnprocessableEntity, "no coverage in request or system of record"
	}
	local := resolverFromResources(prefetchResources(prepared.values))
	resolve := local
	readErr := new(error)
	if prepared.coverageFromSoR {
		var fromSoR func(string) ([]byte, bool)
		fromSoR, readErr = sorReferenceCallback(ctx, g.cfg.SoR)
		resolve = func(ref string) ([]byte, bool) {
			if b, ok := local(ref); ok {
				return b, true
			}
			return fromSoR(ref)
		}
	}
	recipient, _, status, msg := g.recipientForWith(coverage, resolve)
	if *readErr != nil {
		status, msg := SoRFailureResponse(*readErr)
		return "", status, msg
	}
	return recipient, status, msg
}

// handleIngressMetadata serves the provider ingress CapabilityStatement
// (FR-37 per-role). Public like the payer's
// /metadata — a conformance statement is discovery surface, not PHI.
func (g *Gateway) handleIngressMetadata(w http.ResponseWriter, _ *http.Request) {
	// D1a: the published conformance surface names THE DECLARED SET, single-sourced
	// through the same accessor selection and the registry stamp read — a gateway
	// cannot advertise one set and route on another.
	b, err := shnsdk.BuildProviderIngressCapabilityStatement(g.cfg.Clock(), g.declaredContractVersions())
	if err != nil {
		http.Error(w, "capability statement build failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/fhir+json")
	_, _ = w.Write(b)
}

func (g *Gateway) handleCDSDiscovery(w http.ResponseWriter, r *http.Request) {
	if g.ingressAuthRefused(w, r) {
		return
	}
	body, err := g.cdsDiscoveryJSON()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build discovery failed"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleCRDIngress delivers a native CDS Hooks request through verified or
// minimally extracted context and the required participant boundary preparation.
func (g *Gateway) handleCRDIngress(w http.ResponseWriter, r *http.Request) {
	g.handleNativeIngress(w, r)
}

// handleDTRIngress delivers the mounted questionnaire-package operation through
// the shared native dispatcher. The participant DTR application owns population.
func (g *Gateway) handleDTRIngress(w http.ResponseWriter, r *http.Request) {
	g.handleNativeIngress(w, r)
}

// subjectsOf returns a 1-element subjects slice for a non-empty pci, else nil.
func subjectsOf(pci string) []string {
	if pci == "" {
		return nil
	}
	return []string{pci}
}

func (g *Gateway) handlePASIngress(w http.ResponseWriter, r *http.Request) {
	g.handleNativeIngress(w, r)
}
