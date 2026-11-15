// gateway/engine/ingressscope.go
//
// ingressscope.go — the SMART App Launch scopes a payer gateway's ingress grants
// and checks. The token endpoint grants a space-delimited set (RFC 6749 §3.3),
// each scope one the client is registered for, and each payer route takes a
// bearer whose set holds a system-level Task scope that permits what the route
// does: create for the attachment-request Task routes, read for the read and
// vread of a Task at a provider's base. A scope is read as SMART
// defines it: v1's read, write and *, or v2's non-empty subset of c r u d s, in
// that order. A v2 scope with a query (a granular scope) permits nothing here:
// the gateway cannot enforce its filter, so admitting the route on it would be
// more permissive than the scope.
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
// grants it (space-delimited), holds a scope that permits need on Task. A
// granular scope, a scope on another resource or context, and anything that is
// not a Task scope permit nothing.
func taskScopesPermit(granted string, need taskPermission) bool {
	for _, scope := range strings.Split(granted, " ") {
		if perms, granular, ok := parseTaskScope(scope); ok && !granular && perms&need == need {
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
// ingress client registers: every scope naming Task is one of SMART's
// spellings (v1 read, write or *; v2 a subset of c r u d s in order, granular
// included), and at least one, with no query, permits creating a Task:
// system/Task.write, system/Task.*, or a v2 scope with c (from v1.10.0, when
// the rule took read and v2 scopes). Creating a Task is what the payer's
// ingress is for: the read and vread of a Task at a provider's base (from
// v1.12.0) read one the payer's system created, so a client registered only
// to read is still a registration error. A misspelled Task scope would be
// granted and then permit nothing, so it is one too. A scope that permits
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
		return fmt.Errorf("lists no system/Task scope that permits creating a Task (system/Task.write, system/Task.*, or a v2 scope with c, with no query), which the payer ingress's Task routes and its provider listing take (list one, or leave scopes out for %s)", TaskWriteScope)
	}
	return nil
}

// scopesPermit reports whether any of scopes, a client's registered scopes,
// is one with no query that permits need on Task: a bearer the token endpoint
// grants that client can then reach a route that takes need.
func scopesPermit(scopes []string, need taskPermission) bool {
	for _, scope := range scopes {
		if perms, granular, ok := parseTaskScope(scope); ok && !granular && perms&need == need {
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
// pages. v1's read is rs, its write cud, and * all five. A granular scope (a
// v2 scope with a query) permits nothing here, as on the Task routes.
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
// endpoint grants it, holds a Subscription scope with no query that permits
// need.
func subscriptionScopesPermit(granted string, need taskPermission) bool {
	for _, scope := range strings.Split(granted, " ") {
		if perms, granular, ok := parseResourceScope(scope, SubscriptionScopePrefix); ok && !granular && perms&need == need {
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

// listedSubscriptionScopes are the Subscription scopes, each with no query
// permitting something, that one of clients is registered for, exactly as
// listed, sorted: what a payer's SMART configuration advertises beside its
// Task scopes, as it does a read scope, so a scope is named only as one some
// client can be granted.
func listedSubscriptionScopes(clients map[string]IngressClientRegistration) []string {
	var out []string
	for _, c := range clients {
		if c.Module != "" {
			continue
		}
		for _, scope := range c.Scopes {
			if perms, granular, ok := parseResourceScope(scope, SubscriptionScopePrefix); ok && !granular && perms != 0 && !slices.Contains(out, scope) {
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
