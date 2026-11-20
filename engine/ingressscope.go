// gateway/engine/ingressscope.go
//
// ingressscope.go — the SMART App Launch scopes a payer gateway's ingress grants
// and checks. The token endpoint grants a space-delimited set (RFC 6749 §3.3),
// each scope one the client is registered for, and each payer route takes a
// bearer whose set holds a system-level scope on Task, or a wildcard, that
// permits what the route does: create for the attachment-request Task routes,
// read for the read and vread of a Task at a provider's base. A scope is read
// as SMART defines it: v1's read, write and *, or v2's non-empty subset of c r
// u d s, in that order; a wildcard (system/*.<permissions>) permits the route
// for the permissions it grants, as it does each provider ingress route, so
// both halves of the network read one client's scope alike. A v2 scope with a
// query (a granular scope) permits nothing here: the gateway cannot enforce
// its filter, so admitting the route on it would be more permissive than the
// scope.
package engine

import (
	"fmt"
	"slices"
	"strings"
)

// taskPermission is the set of SMART permissions a Task scope grants.
type taskPermission uint8

// The SMART v2 permissions, in the order v2 spells them (c r u d s).
const (
	taskCreate taskPermission = 1 << iota
	taskRead
	taskUpdate
	taskDelete
	taskSearch
)

// taskV2Letters are the v2 permission letters, in the order a scope spells
// them; letter i is permission 1<<i.
const taskV2Letters = "cruds"

// taskScopePrefix is the context and resource of every scope the payer's
// ingress routes take: a system-level scope on Task.
const taskScopePrefix = "system/Task."

// parseTaskScope reads scope as a SMART system-level Task scope: the
// permissions it grants, whether it is granular (a v2 scope with a non-empty
// query) and whether it is a Task scope at all. v1's read is v2's rs, its
// write cud, and * all five (SMART App Launch 2, scopes).
func parseTaskScope(scope string) (perms taskPermission, granular bool, ok bool) {
	return parseResourceScope(scope, taskScopePrefix)
}

// parseScopeOn reads scope as a SMART system-level scope that applies to the
// resource prefix names ("system/<Resource>."): a scope on that resource, or a
// wildcard (system/*.), read as parseResourceScope reads either.
func parseScopeOn(scope, prefix string) (perms taskPermission, granular bool, ok bool) {
	if perms, granular, ok := parseResourceScope(scope, prefix); ok {
		return perms, granular, true
	}
	return parseResourceScope(scope, wildcardScopePrefix)
}

// parseResourceScope reads scope as a SMART system-level scope on the
// resource prefix names ("system/<Resource>."), as parseTaskScope reads a
// Task scope.
func parseResourceScope(scope, prefix string) (perms taskPermission, granular bool, ok bool) {
	rest, found := strings.CutPrefix(scope, prefix)
	if !found {
		return 0, false, false
	}
	switch rest {
	case "read":
		return taskRead | taskSearch, false, true
	case "write":
		return taskCreate | taskUpdate | taskDelete, false, true
	case "*":
		return taskCreate | taskRead | taskUpdate | taskDelete | taskSearch, false, true
	}
	letters, query, hasQuery := strings.Cut(rest, "?")
	if hasQuery && query == "" {
		return 0, false, false
	}
	perms, ok = taskV2Permissions(letters)
	if !ok {
		return 0, false, false
	}
	return perms, hasQuery, true
}

// taskV2Permissions reads a v2 permission string: a non-empty subset of c r u
// d s, each at most once, in that order. Any other string is none.
func taskV2Permissions(letters string) (taskPermission, bool) {
	if letters == "" {
		return 0, false
	}
	var perms taskPermission
	next := 0 // the first letter of taskV2Letters still allowed
	for i := 0; i < len(letters); i++ {
		at := strings.IndexByte(taskV2Letters[next:], letters[i])
		if at < 0 {
			return 0, false
		}
		next += at + 1
		perms |= 1 << (next - 1)
	}
	return perms, true
}

// taskScopesPermit reports whether granted, a scope set as the token endpoint
// grants it (space-delimited), holds a Task scope or a wildcard that permits
// need (parseScopeOn). A granular scope, a scope on another resource or
// context, and anything that is neither permit nothing.
func taskScopesPermit(granted string, need taskPermission) bool {
	for _, scope := range strings.Split(granted, " ") {
		if perms, granular, ok := parseScopeOn(scope, taskScopePrefix); ok && !granular && perms&need == need {
			return true
		}
	}
	return false
}

// grantScope is the scope the token endpoint grants for requested, a
// space-delimited set (RFC 6749 §3.3), and whether it grants one: every scope
// in it must be one of registered, the client's own. Duplicates collapse and
// the order is the request's. An empty request grants none; an empty scope in
// a request (a leading, trailing or doubled space) refuses it.
func grantScope(requested string, registered []string) (string, bool) {
	if requested == "" {
		return "", true
	}
	var set []string
	for _, scope := range strings.Split(requested, " ") {
		if scope == "" || !slices.Contains(registered, scope) {
			return "", false
		}
		if !slices.Contains(set, scope) {
			set = append(set, scope)
		}
	}
	return strings.Join(set, " "), true
}

// namesTask reports whether scope names the system-level Task resource, well
// spelled or not: its context is system and its resource, up to the
// permissions, is Task in any case, so a scope spelled system/task is caught
// as a misspelling rather than kept as another resource's.
func namesTask(scope string) bool {
	context, rest, _ := strings.Cut(scope, "/")
	resource, _, _ := strings.Cut(rest, ".")
	return strings.EqualFold(context, "system") && strings.EqualFold(resource, "Task")
}

// CheckPayerIngressScopes is the boot rule for the scopes a payer gateway's
// ingress client registers: every scope naming Task, Subscription or a
// wildcard (system/*.) is one of SMART's spellings (v1 read, write or *; v2 a
// subset of c r u d s in order, granular included), and at least one, with no
// query, permits creating a Task: system/Task.write, system/Task.*, or a v2
// scope with c (from v1.10.0, when the rule took read and v2 scopes), or a
// wildcard that does, system/*.write, system/*.* or a v2 wildcard with c (from
// v1.16.0, when the payer's routes took a wildcard, as the provider's do).
// Creating a Task is what the payer's ingress is for: the read and vread of a
// Task at a provider's base (from v1.12.0) read one the payer's system
// created, so a client registered only to read is still a registration error.
// A misspelled Task scope, Subscription scope or wildcard would be granted and
// then permit nothing, so it is one too. A scope that permits
// reading, beside one that permits create, admits the read routes. A granular
// scope, or one for another resource or context, is kept beside such a scope
// and permits nothing on these routes.
func CheckPayerIngressScopes(scopes []string) error {
	usable := 0
	for _, scope := range scopes {
		// A Subscription scope, which the Subscription routes take (from
		// v1.13.0), is one of SMART's spellings too: a misspelled one would
		// be granted and permit nothing.
		if namesSubscription(scope) {
			if _, _, ok := parseResourceScope(scope, SubscriptionScopePrefix); !ok {
				return fmt.Errorf("scope %q is not a SMART system/Subscription scope (system/Subscription.read, system/Subscription.write, system/Subscription.*, or system/Subscription. followed by some of c, r, u, d, s in that order)", scope)
			}
			continue
		}
		// A wildcard permits the Task routes and the Subscription routes for
		// the permissions it grants, so it is held to SMART's spellings as
		// the provider's boot rule holds one: a misspelled one would be
		// granted and permit nothing.
		if namesWildcard(scope) {
			perms, granular, ok := parseResourceScope(scope, wildcardScopePrefix)
			if !ok {
				return fmt.Errorf("scope %q is not a SMART wildcard scope (system/*.read, system/*.write, system/*.*, or system/*. followed by some of c, r, u, d, s in that order)", scope)
			}
			if !granular && perms&taskCreate != 0 {
				usable++
			}
			continue
		}
		if !namesTask(scope) {
			continue
		}
		perms, granular, ok := parseTaskScope(scope)
		if !ok {
			return fmt.Errorf("scope %q is not a SMART system/Task scope (system/Task.read, system/Task.write, system/Task.*, or system/Task. followed by some of c, r, u, d, s in that order)", scope)
		}
		if !granular && perms&taskCreate != 0 {
			usable++
		}
	}
	if usable == 0 {
		return fmt.Errorf("lists no system/Task scope or wildcard that permits creating a Task (system/Task.write, system/Task.*, a v2 scope with c, or a wildcard that grants c such as system/*.write, with no query), which the payer ingress's Task routes and its provider listing take (list one, or leave scopes out for %s)", TaskWriteScope)
	}
	return nil
}

// scopesPermit reports whether any of scopes, a client's registered scopes,
// is one with no query that permits need on Task, a Task scope or a wildcard
// (parseScopeOn): a bearer the token endpoint grants that client can then
// reach a route that takes need.
func scopesPermit(scopes []string, need taskPermission) bool {
	for _, scope := range scopes {
		if perms, granular, ok := parseScopeOn(scope, taskScopePrefix); ok && !granular && perms&need == need {
			return true
		}
	}
	return false
}

// challengeScope is the scope a route that takes need names in its 403's
// WWW-Authenticate challenge (RFC 6750 §3): the v1 scope a client asks for.
func challengeScope(need taskPermission) string {
	switch need {
	case taskCreate:
		return TaskWriteScope
	case taskRead:
		return TaskReadScope
	}
	return ""
}

// The scopes a payer gateway's Subscription routes take (the notifications
// design §6.1): a system-level Subscription scope that permits the
// interaction, as SMART defines one, read as a Task scope is: create (c) for a
// create; read (r) for a read, a vread and an instance $status; update (u)
// for an update; delete (d) for a delete; search (s) for a search and its
// pages. v1's read is rs, its write cud, and * all five. A wildcard permits
// them as it does the Task routes. A granular scope (a v2 scope with a query)
// permits nothing here, as on the Task routes.
const (
	// SubscriptionScopePrefix is the context and resource of every scope the
	// Subscription routes take.
	SubscriptionScopePrefix = "system/Subscription."
	// SubscriptionWriteScope is the v1 scope a route that creates, updates or
	// deletes a Subscription names in its challenge.
	SubscriptionWriteScope = "system/Subscription.write"
	// SubscriptionReadScope is the v1 scope a route that reads or searches
	// Subscriptions names in its challenge.
	SubscriptionReadScope = "system/Subscription.read"
)

// subscriptionScopesPermit reports whether granted, a scope set as the token
// endpoint grants it, holds a Subscription scope or a wildcard with no query
// that permits need (parseScopeOn).
func subscriptionScopesPermit(granted string, need taskPermission) bool {
	for _, scope := range strings.Split(granted, " ") {
		if perms, granular, ok := parseScopeOn(scope, SubscriptionScopePrefix); ok && !granular && perms&need == need {
			return true
		}
	}
	return false
}

// subscriptionChallengeScope is the v1 scope a Subscription route that takes
// need names in its 403's challenge.
func subscriptionChallengeScope(need taskPermission) string {
	if need == taskRead || need == taskSearch {
		return SubscriptionReadScope
	}
	return SubscriptionWriteScope
}

// namesSubscription reports whether scope names the system-level
// Subscription resource, well spelled or not, as namesTask does Task.
func namesSubscription(scope string) bool {
	context, rest, _ := strings.Cut(scope, "/")
	resource, _, _ := strings.Cut(rest, ".")
	return strings.EqualFold(context, "system") && strings.EqualFold(resource, "Subscription")
}

// namesWildcard reports whether scope is a system-level wildcard, well spelled
// or not: its context is system in any case and its resource, up to the
// permissions, is *.
func namesWildcard(scope string) bool {
	context, rest, _ := strings.Cut(scope, "/")
	resource, _, _ := strings.Cut(rest, ".")
	return strings.EqualFold(context, "system") && resource == "*"
}

// listedSubscriptionScopes are the Subscription scopes, each with no query
// permitting something on Subscription, that one of clients is registered
// for, exactly as listed, sorted, and with wildcards, each such wildcard too:
// what a SMART configuration advertises beside its route scopes, so a scope
// is named only as one some client can be granted. A payer's lists wildcards
// (wildcards), as it lists a wildcard read scope; a provider's lists none, as
// its route scopes (providerScopes) name none.
func listedSubscriptionScopes(clients map[string]IngressClientRegistration, wildcards bool) []string {
	var out []string
	for _, c := range clients {
		if c.Module != "" {
			continue
		}
		for _, scope := range c.Scopes {
			read := parseResourceScope
			if wildcards {
				read = parseScopeOn
			}
			if perms, granular, ok := read(scope, SubscriptionScopePrefix); ok && !granular && perms != 0 && !slices.Contains(out, scope) {
				out = append(out, scope)
			}
		}
	}
	slices.Sort(out)
	return out
}

// ingressBypassSubscriptionScope is the Subscription scope the test-only auth
// bypass grants beside the Task scopes (EnableIngressForTest): every
// Subscription interaction, as a client registered for system/Subscription.*
// is granted.
const ingressBypassSubscriptionScope = "system/Subscription.*"

// The scopes a provider gateway's ingress routes take. Each route names its
// own (routeScope): the FHIR resource types a system-level scope may name to
// permit it, read as a Task scope is, and the permission it needs, decided by
// the route alone, never by reading the request. Three more readings hold on
// these routes and no other:
//
//   - ProviderUmbrellaScope, system/Davinci.write, permits every one of them:
//     the scope the provider's ingress has always advertised, and the one a
//     client that lists no scopes is registered for (app.go,
//     loadIngressClients);
//   - a wildcard scope, system/*. followed by SMART's permissions, permits the
//     route for the permissions it grants, whatever resource the route names
//     (SMART App Launch 2, scopes and launch context, wildcard scopes), but
//     never a route that names no resource (the CDS Hooks routes), which the
//     umbrella alone permits;
//   - the three scopes the Da Vinci DTR IG names as written
//     (dtrScopeAliases) are read as the resource-type scopes they mean.
const (
	// ProviderUmbrellaScope permits every provider ingress route.
	ProviderUmbrellaScope = ingressScope
	// wildcardScopePrefix is the context and resource of a SMART wildcard
	// scope.
	wildcardScopePrefix = "system/*."
)

// dtrScopeAliases are the scopes the Da Vinci DTR IG names, in its own
// spelling, as sufficient to invoke its Questionnaire operations
// (https://hl7.org/fhir/us/davinci-dtr/specification.html#authenticating-dtr-client-to-payer-api:
// "system/questionnaire.rs system/valueset.rs system/library.rs"), each with
// the resource-type scope it means. A client that registers them as the IG
// writes them is read as registered for those; no other spelling of a
// resource type in another case is read so.
var dtrScopeAliases = map[string]string{
	"system/questionnaire.rs": "system/Questionnaire.rs",
	"system/valueset.rs":      "system/ValueSet.rs",
	"system/library.rs":       "system/Library.rs",
}

// routeScope is the scope a provider ingress route takes: a scope on one of
// resources (a system-level scope naming that resource type, or a wildcard)
// with no query that permits need, all of it, or, with any, some of it; or
// ProviderUmbrellaScope, unless noUmbrella. A route with no resources takes
// the umbrella alone. A listing's scope is instead its alternatives: any scope
// one of them takes (listingScope), and none when it has none. reason is the route's refusal sentence.
type routeScope struct {
	resources    []string
	need         taskPermission
	any          bool
	noUmbrella   bool
	listing      bool
	alternatives []routeScope
	reason       refusalReason
}

// scopedPattern is one provider ingress route, as its workstream mounts it
// ("METHOD /path"), with the scope its handler checks.
type scopedPattern struct {
	pattern string
	scope   routeScope
}

// listingScope is the scope a listing of bases takes: any scope some route of
// routes under base takes (a pattern whose path is base or below it), so a
// route added under the bases widens the listing, by structure.
func listingScope(base string, routes []scopedPattern, reason refusalReason) routeScope {
	s := routeScope{listing: true, reason: reason}
	for _, r := range routes {
		_, path, _ := strings.Cut(r.pattern, " ")
		if path == base || strings.HasPrefix(path, base+"/") {
			s.alternatives = append(s.alternatives, r.scope)
		}
	}
	return s
}

// permits reports whether a scope's permissions perms cover what s needs.
func (s routeScope) permits(perms taskPermission) bool {
	if s.any {
		return perms&s.need != 0
	}
	return perms&s.need == s.need
}

// routeScopesPermit reports whether granted, a space-delimited scope set,
// holds a scope that permits route s.
func routeScopesPermit(granted string, s routeScope) bool {
	if s.listing {
		for _, alt := range s.alternatives {
			if routeScopesPermit(granted, alt) {
				return true
			}
		}
		return false
	}
	for _, scope := range strings.Split(granted, " ") {
		if scope == ProviderUmbrellaScope && !s.noUmbrella {
			return true
		}
		if len(s.resources) == 0 {
			continue
		}
		if alias, ok := dtrScopeAliases[scope]; ok {
			scope = alias
		}
		if perms, granular, ok := parseResourceScope(scope, wildcardScopePrefix); ok && !granular && s.permits(perms) {
			return true
		}
		for _, resource := range s.resources {
			if perms, granular, ok := parseResourceScope(scope, "system/"+resource+"."); ok && !granular && s.permits(perms) {
				return true
			}
		}
	}
	return false
}

// challenge is the scope set route s names in its 403's WWW-Authenticate
// challenge (RFC 6750 §3): SMART v1's scope on each of its resources that a
// client asks for, write for a route that creates, updates or deletes, read
// for one that reads or searches, as a payer's routes name theirs; the
// umbrella for a route that names no resource; and, for a listing, each
// alternative's, once.
func (s routeScope) challenge() string {
	if s.listing {
		var scopes []string
		for _, alt := range s.alternatives {
			for _, scope := range strings.Split(alt.challenge(), " ") {
				if !slices.Contains(scopes, scope) {
					scopes = append(scopes, scope)
				}
			}
		}
		return strings.Join(scopes, " ")
	}
	if len(s.resources) == 0 {
		return ProviderUmbrellaScope
	}
	form := "read"
	if s.need&(taskCreate|taskUpdate|taskDelete) != 0 {
		form = "write"
	}
	scopes := make([]string, len(s.resources))
	for i, resource := range s.resources {
		scopes[i] = "system/" + resource + "." + form
	}
	return strings.Join(scopes, " ")
}

// providerScopes are the scopes a provider gateway's SMART configuration
// advertises: the umbrella, then each scope the challenges of the Da Vinci
// routes the umbrella permits name, sorted, each once. A route the umbrella
// does not permit (the plain base's refused Subscriptions) and a listing,
// whose alternatives are routes of their own, add none.
func providerScopes(routes []routeScope) []string {
	var out []string
	for _, s := range routes {
		if len(s.resources) == 0 || s.noUmbrella || s.listing {
			continue
		}
		for _, scope := range strings.Split(s.challenge(), " ") {
			if !slices.Contains(out, scope) {
				out = append(out, scope)
			}
		}
	}
	slices.Sort(out)
	return append([]string{ProviderUmbrellaScope}, out...)
}

// CheckProviderIngressScopes is the boot rule for the scopes a provider
// gateway's ingress client registers: a scope naming the umbrella, a wildcard,
// Subscription or one of resources, the resource types the provider's routes
// take, in any case, must be spelled as SMART spells it (v1 read, write or *; v2 a subset
// of c r u d s in order, granular included) with the resource type in its own
// case, or be one of the DTR IG's own scopes (dtrScopeAliases). A misspelled
// one would be granted and then permit nothing. A scope for another resource
// or context is kept and permits nothing on these routes.
func CheckProviderIngressScopes(scopes []string, resources []string) error {
	for _, scope := range scopes {
		if _, ok := dtrScopeAliases[scope]; ok {
			continue
		}
		// A Subscription scope, which the plain base's Subscription routes and
		// those at a payer's PAS base take (routeScopeRefused), is held
		// to SMART's spellings as a payer's is.
		if namesSubscription(scope) {
			if _, _, ok := parseResourceScope(scope, SubscriptionScopePrefix); !ok {
				return fmt.Errorf("scope %q is not a SMART system/Subscription scope (system/Subscription.read, system/Subscription.write, system/Subscription.*, or system/Subscription. followed by some of c, r, u, d, s in that order)", scope)
			}
			continue
		}
		context, rest, _ := strings.Cut(scope, "/")
		named, _, _ := strings.Cut(rest, ".")
		if !strings.EqualFold(context, "system") {
			continue
		}
		switch {
		case strings.EqualFold(scope, ProviderUmbrellaScope):
			if scope != ProviderUmbrellaScope {
				return fmt.Errorf("scope %q is not %s, the provider ingress's scope for every route, as spelled", scope, ProviderUmbrellaScope)
			}
		case named == "*":
			if _, _, ok := parseResourceScope(scope, wildcardScopePrefix); !ok {
				return fmt.Errorf("scope %q is not a SMART wildcard scope (system/*.read, system/*.write, system/*.*, or system/*. followed by some of c, r, u, d, s in that order)", scope)
			}
		default:
			for _, resource := range resources {
				if !strings.EqualFold(named, resource) {
					continue
				}
				if _, _, ok := parseResourceScope(scope, "system/"+resource+"."); !ok {
					return fmt.Errorf("scope %q is not a SMART system/%s scope (system/%s.read, system/%s.write, system/%s.*, or system/%s. followed by some of c, r, u, d, s in that order, the resource type in its own case)", scope, resource, resource, resource, resource, resource)
				}
			}
		}
	}
	return nil
}

// ProviderScopesReachARoute reports whether a client registered for scopes
// can be granted a scope some provider ingress route takes
// (routeScopesPermit): the umbrella, or a route's own scope.
func ProviderScopesReachARoute(scopes []string) bool {
	granted := strings.Join(scopes, " ")
	for _, s := range compiledRouteScopes() {
		if routeScopesPermit(granted, s) {
			return true
		}
	}
	return false
}
