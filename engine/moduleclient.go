package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// moduleclient.go — a registered module ingress client.
//
// A module plays the Da Vinci system a participant's own system cannot be: it
// builds the participant's request and sends it to this gateway's own ingress,
// as a native system does. Its body is then the gateway's own message, not the
// participant's, so the gateway seals it Authored by the registered builder the
// module declares, on the leg's originated row, never Exact on the carried one.
//
// Modules are compiled in from a closed set (ModuleIDs) and enabled at boot:
// a participant marks one of its own registered ingress clients as a module
// (IngressClientRegistration.Module). A request from that client declares its
// builder in BuilderHeader. The declaration is honoured only from such a
// client, identified by the token the ingress verified, never by anything the
// request states. Each refusal of a declaration is a fidelity refusal of SHN's
// own edits (RefusalFidelity, 422), at every conformance level, before any
// leg is sent (a module CRD request carrying the callback is refused before
// anything is read through it).

// BuilderHeader is the request header a module ingress client names the
// registered builder of its request in. A native client must not send it.
const BuilderHeader = "SHN-Builder"

// ModuleOnBehalf is the on-behalf building module (module 1): it builds a
// participant's Da Vinci requests from the participant's own records.
const ModuleOnBehalf = "on-behalf"

// moduleBuilders is the closed set of modules this build compiles in, each
// with the registered builders its client may declare. The set a client may
// declare is the module's, never configured.
var moduleBuilders = map[string][]relay.BuilderID{
	ModuleOnBehalf: {
		relay.BuilderSDKCRDRequest,
		relay.BuilderSDKDTRPackage,
		relay.BuilderSDKPASSubmit,
		relay.BuilderSDKPASUpdate,
		relay.BuilderSDKPASInquiry,
		relay.BuilderSDKEligibility,
		relay.BuilderSDKFederatedQuery,
	},
}

// ModuleIDs is the closed set of module ids this build compiles in, sorted.
func ModuleIDs() []string {
	out := make([]string, 0, len(moduleBuilders))
	for id := range moduleBuilders {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ModuleBuilders is the registered builders a client of module id may
// declare; nil for an id this build does not compile in.
func ModuleBuilders(id string) []relay.BuilderID { return slices.Clone(moduleBuilders[id]) }

// checkModuleRegistrations refuses, at boot, an ingress client marked as a
// module this build does not compile in.
func checkModuleRegistrations(clients map[string]IngressClientRegistration) error {
	for id, reg := range clients {
		if reg.Module == "" {
			continue
		}
		if _, ok := moduleBuilders[reg.Module]; !ok {
			return fmt.Errorf("ingress auth: registration %q: unknown module %q (this build compiles in %s)", id, reg.Module, strings.Join(ModuleIDs(), ", "))
		}
	}
	return nil
}

// ingressCallerKey carries an ingress call's caller holder, which the ingress
// auth check fills with the client it verified.
type ingressCallerKey struct{}

// ingressCaller is the client the ingress verified for one call; empty when
// none was (the test bypass, or a handler driven without the route).
type ingressCaller struct{ client string }

// withIngressCaller gives an ingress call a caller holder.
func withIngressCaller(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), ingressCallerKey{}, &ingressCaller{})))
	}
}

// noteIngressCaller records the client the ingress verified for the call.
func noteIngressCaller(ctx context.Context, client string) {
	if c, ok := ctx.Value(ingressCallerKey{}).(*ingressCaller); ok {
		c.client = client
	}
}

// ingressModule is the module the call's verified client is registered as;
// "" for a native client, and for a call no client was verified for.
func (g *Gateway) ingressModule(ctx context.Context) string {
	c, ok := ctx.Value(ingressCallerKey{}).(*ingressCaller)
	if !ok || c.client == "" || g.ingressAuth == nil {
		return ""
	}
	return g.ingressAuth.clients[c.client].Module
}

// Builder-declaration refusals. Stable text: a caller may match it.
const (
	errBuilderNative     = "a native client must not send " + BuilderHeader + ": only a registered module ingress client builds the gateway's own messages"
	errBuilderUndeclared = "a module ingress client must name its builder in " + BuilderHeader
)

// ingressBuilder is the builder the call's request is the gateway's own
// message by: "" for a native client's request, which is the participant's.
// It refuses, writing a fidelity refusal (ok false), a native client that
// sends BuilderHeader, a module client that does not, and a builder that is
// not registered, not its module's, or not permitted on leg's originated
// request. It runs once leg is known and before any leg is sent.
func (g *Gateway) ingressBuilder(w http.ResponseWriter, r *http.Request, leg string) (relay.BuilderID, bool) {
	values := r.Header.Values(BuilderHeader)
	declared := ""
	if len(values) > 0 {
		declared = strings.TrimSpace(values[0])
	}
	module := g.ingressModule(r.Context())
	switch {
	case module == "" && len(values) > 0:
		refuseBuilder(w, errBuilderNative)
		return "", false
	case module == "":
		return "", true
	case len(values) > 1:
		refuseBuilder(w, "a module ingress client names one builder: "+BuilderHeader+" is repeated")
		return "", false
	case declared == "":
		refuseBuilder(w, errBuilderUndeclared)
		return "", false
	}
	b := relay.BuilderID(declared)
	if !slices.Contains(moduleBuilders[module], b) {
		refuseBuilder(w, fmt.Sprintf("%s names %s, which is not a builder of module %s", BuilderHeader, quoteBounded(declared), module))
		return "", false
	}
	if rule, ok := relay.LegOwnership()[requestKey(leg, false)]; !ok || !slices.Contains(rule.Builders, b) {
		refuseBuilder(w, fmt.Sprintf("%s names %s, which does not build a %s request", BuilderHeader, quoteBounded(declared), leg))
		return "", false
	}
	return b, true
}

// sealModuleRequest is the request a module client's body is sent as: the
// gateway's own message by builder, as it was built. A request this gateway
// would have to edit (p, as prepared for a native client, carries edits) is
// refused: a message is either the participant's, edited, or the gateway's own,
// never both. With no builder, p is returned unchanged; on a refusal (ok false)
// the payload returned is not to be sent.
func sealModuleRequest(w http.ResponseWriter, builder relay.BuilderID, p relay.Payload, body []byte) (relay.Payload, bool) {
	if builder == "" {
		return p, true
	}
	if edits := p.Edits(); len(edits) > 0 {
		ids := make([]string, len(edits))
		for i, e := range edits {
			ids[i] = string(e)
		}
		refuseBuilder(w, "a module ingress client's request is built whole: this gateway would have to edit it ("+strings.Join(ids, ", ")+")")
		return p, false
	}
	out, err := relay.Authored(builder, body, p.ContentType())
	if err != nil {
		refuseBuilder(w, "a module ingress client's request could not be sealed as "+string(builder)+": "+err.Error())
		return p, false
	}
	return out, true
}

// moduleCRDCallback refuses a module client's CDS Hooks request that carries
// the callback (fhirServer, fhirAuthorization), before anything is read
// through it: this gateway always removes it (E-01), and a module's request is
// sent as built. ok is true for a native request (builder ""), for a module
// request without the callback, and for a body that cannot be read, which the
// handler's own read refuses (400) before anything is read through it.
func moduleCRDCallback(w http.ResponseWriter, builder relay.BuilderID, body []byte) bool {
	if builder == "" {
		return true
	}
	var cb struct {
		FHIRServer        json.RawMessage `json:"fhirServer"`
		FHIRAuthorization json.RawMessage `json:"fhirAuthorization"`
	}
	if decodeMessage(body, &cb) != nil {
		return true
	}
	if cb.FHIRServer != nil || cb.FHIRAuthorization != nil {
		refuseBuilder(w, "a module ingress client's CDS Hooks request is built whole: it carries no fhirServer or fhirAuthorization, which this gateway would remove (E-01)")
		return false
	}
	return true
}

// refuseBuilder answers a builder declaration this gateway refuses: the
// fidelity of SHN's own edits, before any leg is sent.
func refuseBuilder(w http.ResponseWriter, msg string) {
	exchangeOfWriter(w).refused(RefusalFidelity)
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg})
}

// quoteBounded names a caller-stated value in a refusal: quoted when short,
// else by its size.
func quoteBounded(v string) string {
	if len(v) <= 64 {
		return fmt.Sprintf("%q", v)
	}
	return fmt.Sprintf("a %d-byte value", len(v))
}
