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
	rest, found := strings.CutPrefix(scope, taskScopePrefix)
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
