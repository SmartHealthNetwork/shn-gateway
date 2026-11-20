package engine

import (
	"slices"
	"strings"
	"testing"
)

// A system-level Task scope reads as SMART App Launch defines it: v1's read,
// write and *, and v2's non-empty subset of c r u d s in that order, with or
// without a query. Any other spelling is not a Task scope.
func TestParseTaskScope(t *testing.T) {
	// letters spells a permission set as v2 does, so each row states exactly
	// the permissions the scope grants.
	letters := func(perms taskPermission) string {
		var b strings.Builder
		for i := 0; i < len(taskV2Letters); i++ {
			if perms&(1<<i) != 0 {
				b.WriteByte(taskV2Letters[i])
			}
		}
		return b.String()
	}
	for _, row := range []struct {
		scope        string
		ok, granular bool
		perms        string
	}{
		{scope: "system/Task.write", ok: true, perms: "cud"},
		{scope: "system/Task.read", ok: true, perms: "rs"},
		{scope: "system/Task.*", ok: true, perms: "cruds"},
		{scope: "system/Task.c", ok: true, perms: "c"},
		{scope: "system/Task.cr", ok: true, perms: "cr"},
		{scope: "system/Task.cruds", ok: true, perms: "cruds"},
		{scope: "system/Task.rs", ok: true, perms: "rs"},
		{scope: "system/Task.r", ok: true, perms: "r"},
		{scope: "system/Task.s", ok: true, perms: "s"},
		{scope: "system/Task.uds", ok: true, perms: "uds"},
		{scope: "system/Task.cd", ok: true, perms: "cd"},
		{scope: "system/Task.c?status=requested", ok: true, granular: true, perms: "c"},
		{scope: "system/Task.rs?code=x", ok: true, granular: true, perms: "rs"},
		// Not Task scopes: letters out of order, repeated, outside c r u d
		// s or none; an empty query; a query on a v1 form; another
		// resource, context or spelling.
		{scope: "system/Task.rc"},
		{scope: "system/Task.cc"},
		{scope: "system/Task.x"},
		{scope: "system/Task."},
		{scope: "system/Task"},
		{scope: "system/Task.C"},
		{scope: "system/Task.cruds?"},
		{scope: "system/Task.read?status=requested"},
		{scope: "system/Task.*?status=requested"},
		{scope: "system/Task.writes"},
		{scope: "system/Patient.read"},
		{scope: "system/task.read"},
		{scope: "user/Task.write"},
		{scope: "system/*.write"},
		{scope: "system/Davinci.write"},
		{scope: ""},
	} {
		perms, granular, ok := parseTaskScope(row.scope)
		if ok != row.ok || granular != row.granular || letters(perms) != row.perms {
			t.Errorf("%q: ok %v granular %v permissions %q, want %v %v %q", row.scope, ok, granular, letters(perms), row.ok, row.granular, row.perms)
		}
	}
}

// A granted set permits a route when some scope in it does: a granular
// scope, another resource's or another context's permits nothing, since the
// gateway cannot enforce a query and grants no more than the scope says.
func TestTaskScopesPermit(t *testing.T) {
	for _, row := range []struct {
		granted      string
		create, read bool
	}{
		{"system/Task.write", true, false},
		{"system/Task.read", false, true},
		{"system/Task.write system/Task.read", true, true},
		{"system/Task.read system/Task.write", true, true},
		{"system/Task.*", true, true},
		{"system/Task.c", true, false},
		{"system/Task.cr", true, true},
		{"system/Task.r", false, true},
		{"system/Task.uds", false, false},
		{"system/Task.c?status=requested", false, false},
		{"system/Task.c?status=requested system/Task.r", false, true},
		{"system/Patient.read", false, false},
		{"system/Patient.write system/Davinci.write", false, false},
		{"system/Task.rc", false, false},
		{"", false, false},
		// A wildcard permits the routes for the permissions it grants.
		{"system/*.write", true, false},
		{"system/*.read", false, true},
		{"system/*.*", true, true},
		{"system/*.c", true, false},
		{"system/*.rs", false, true},
		{"system/*.cruds", true, true},
		{"system/*.uds", false, false},
		{"system/*.c?status=requested", false, false},
		{"system/*.c?status=requested system/*.r", false, true},
		{"system/*.rc", false, false},
		{"SYSTEM/*.write", false, false},
		{"patient/*.write", false, false},
	} {
		if got := taskScopesPermit(row.granted, taskCreate); got != row.create {
			t.Errorf("%q: create %v, want %v", row.granted, got, row.create)
		}
		if got := taskScopesPermit(row.granted, taskRead); got != row.read {
			t.Errorf("%q: read %v, want %v", row.granted, got, row.read)
		}
	}
}

// The token endpoint grants a space-delimited set (RFC 6749 §3.3) only when
// every scope in it is registered for the client: duplicates collapse, the
// order is the request's, an empty request grants none, and an empty token
// (a leading, trailing or doubled space) refuses the request, as an
// unregistered one does.
func TestGrantScope(t *testing.T) {
	registered := []string{"system/Task.write", "system/Task.read", "system/Task.c?status=requested"}
	for _, row := range []struct {
		requested, granted string
		ok                 bool
	}{
		{"", "", true},
		{"system/Task.write", "system/Task.write", true},
		{"system/Task.write system/Task.read", "system/Task.write system/Task.read", true},
		{"system/Task.read system/Task.write", "system/Task.read system/Task.write", true},
		{"system/Task.write system/Task.write", "system/Task.write", true},
		{"system/Task.read system/Task.write system/Task.read", "system/Task.read system/Task.write", true},
		{"system/Task.c?status=requested", "system/Task.c?status=requested", true},
		{"system/Task.write system/Task.cr", "", false},
		{"system/Task.cr", "", false},
		{"system/Patient.read", "", false},
		{" system/Task.write", "", false},
		{"system/Task.write ", "", false},
		{"system/Task.write  system/Task.read", "", false},
		{" ", "", false},
		{"system/Task.write\tsystem/Task.read", "", false},
	} {
		granted, ok := grantScope(row.requested, registered)
		if granted != row.granted || ok != row.ok {
			t.Errorf("%q: %q %v, want %q %v", row.requested, granted, ok, row.granted, row.ok)
		}
	}
}

// A payer's ingress client registers at least one Task scope, and each Task
// scope it registers is one: a misspelled one would be granted and then
// permit nothing, so it is a registration error. Scopes for another resource
// or context are kept, as before, and permit nothing here.
func TestCheckPayerIngressScopes(t *testing.T) {
	for _, scopes := range [][]string{
		{TaskWriteScope},
		{"system/Task.*"},
		{"system/Task.c"},
		{"system/Task.cr", "system/Task.rs"},
		{TaskWriteScope, "system/Task.c?status=requested"},
		{TaskWriteScope, "system/Task.read"},
		{"system/Davinci.write", TaskWriteScope},
		// A wildcard that permits create is a client's create scope, as a
		// Task scope is; one for another context is kept and permits nothing.
		{"system/*.write"},
		{"system/*.*"},
		{"system/*.c"},
		{"system/*.cruds"},
		{"system/*.rs", "system/*.c"},
		{TaskWriteScope, "system/*.rs"},
		{TaskWriteScope, "system/*.c?status=requested"},
		{TaskWriteScope, "patient/*.x"},
	} {
		if err := CheckPayerIngressScopes(scopes); err != nil {
			t.Errorf("%v: refused, want taken: %v", scopes, err)
		}
	}
	for _, row := range []struct {
		scopes []string
		want   string
	}{
		{nil, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/Davinci.write"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/Patient.read"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/Task.c?status=requested"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/Task.uds"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		// A wildcard that does not permit create, or a granular one, is not
		// a create scope.
		{[]string{"system/*.read"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/*.rs"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/*.c?status=requested"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"patient/*.write"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		// A misspelled wildcard would be granted and permit nothing, so it
		// is a registration error, as it is on the provider's ingress.
		{[]string{"system/*.rc"}, `"system/*.rc" is not a SMART wildcard scope`},
		{[]string{TaskWriteScope, "system/*.x"}, `"system/*.x" is not a SMART wildcard scope`},
		{[]string{TaskWriteScope, "system/*."}, `"system/*." is not a SMART wildcard scope`},
		{[]string{TaskWriteScope, "system/*"}, `"system/*" is not a SMART wildcard scope`},
		{[]string{TaskWriteScope, "System/*.read"}, `"System/*.read" is not a SMART wildcard scope`},
		{[]string{TaskWriteScope, "system/*.read?x=1"}, `"system/*.read?x=1" is not a SMART wildcard scope`},
		{[]string{TaskWriteScope, "system/*.c?"}, `"system/*.c?" is not a SMART wildcard scope`},
		// Registered only to read: a payer's ingress is for creating its Task,
		// and its read routes read one its system created.
		{[]string{"system/Task.read"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/Task.rs"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/Task.read", "system/Task.c?status=requested"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/Davinci.write", "system/Task.u"}, "lists no system/Task scope or wildcard that permits creating a Task"},
		{[]string{"system/task.read"}, `"system/task.read" is not a SMART system/Task scope`},
		{[]string{TaskWriteScope, "System/Task.read"}, `"System/Task.read" is not a SMART system/Task scope`},
		{[]string{"system/Task.rc"}, `"system/Task.rc" is not a SMART system/Task scope`},
		{[]string{"system/Task.cc"}, `"system/Task.cc" is not a SMART system/Task scope`},
		{[]string{"system/Task.x"}, `"system/Task.x" is not a SMART system/Task scope`},
		{[]string{"system/Task."}, `"system/Task." is not a SMART system/Task scope`},
		{[]string{"system/Task"}, `"system/Task" is not a SMART system/Task scope`},
		{[]string{TaskWriteScope, "system/Task.rc"}, `"system/Task.rc" is not a SMART system/Task scope`},
		{[]string{TaskWriteScope, "system/Task.read?x=1"}, `"system/Task.read?x=1" is not a SMART system/Task scope`},
	} {
		err := CheckPayerIngressScopes(row.scopes)
		if err == nil || !strings.Contains(err.Error(), row.want) {
			t.Errorf("%v: %v, want a refusal naming %q", row.scopes, err, row.want)
		}
	}
}

// The create routes' refusal names what they take: the reason is the fixed
// sentence registered for it, and the challenge names the v1 write scope a
// client asks for today. The read routes' names the read scope, and a
// permission no route takes has neither.
func TestScopeReason_Create(t *testing.T) {
	for need, want := range map[taskPermission][]string{
		taskCreate: {"insufficient_scope", TaskWriteScope, "system/Task.c", "system/Task.*"},
		taskRead:   {"insufficient_scope", TaskReadScope, "system/Task.r", "system/Task.*"},
	} {
		r := scopeReason(need)
		if r == "" || !slices.Contains(coreRefusalReasons(), r) {
			t.Fatalf("reason %q is not a registered reason", r)
		}
		for _, s := range want {
			if !strings.Contains(string(r), s) {
				t.Errorf("reason %q does not name %s", r, s)
			}
		}
	}
	if challengeScope(taskCreate) != TaskWriteScope || challengeScope(taskRead) != TaskReadScope {
		t.Fatalf("challenges %q and %q, want %s and %s", challengeScope(taskCreate), challengeScope(taskRead), TaskWriteScope, TaskReadScope)
	}
	if scopeReason(taskUpdate) != "" || challengeScope(taskUpdate) != "" {
		t.Fatalf("a permission no route takes: reason %q, challenge %q", scopeReason(taskUpdate), challengeScope(taskUpdate))
	}
}

// TestRouteScopesPermit: a provider route's scope check, on test routes: a
// resource scope on one of the route's resources (v1 or v2) with no query
// that permits what the route needs; a wildcard for the permissions it
// grants, never on a route that names no resource; the umbrella on every
// route; the DTR IG's own spellings; and nothing else.
func TestRouteScopesPermit(t *testing.T) {
	create := routeScope{resources: []string{"Claim"}, need: taskCreate}
	read := routeScope{resources: []string{"Questionnaire"}, need: taskRead}
	either := routeScope{resources: []string{"DocumentReference", "QuestionnaireResponse"}, need: taskCreate}
	anyOf := routeScope{resources: []string{"Claim"}, need: taskCreate | taskRead, any: true}
	none := routeScope{}
	for _, row := range []struct {
		granted string
		route   routeScope
		want    bool
	}{
		{"system/Claim.c", create, true},
		{"system/Claim.write", create, true},
		{"system/Claim.*", create, true},
		{"system/Claim.cruds", create, true},
		{"system/Claim.r", create, false},
		{"system/Claim.c?patient=1", create, false},
		{"system/claim.c", create, false},
		{"user/Claim.c", create, false},
		{"system/Task.c", create, false},
		{"system/*.c", create, true},
		{"system/*.write", create, true},
		{"system/*.rs", create, false},
		{"system/*.c?patient=1", create, false},
		{ProviderUmbrellaScope, create, true},
		{"system/davinci.write", create, false},
		{"SYSTEM/Davinci.write", none, false},
		{"system/Davinci.read", create, false},
		{"", create, false},
		{"system/Questionnaire.rs system/Claim.c", read, true},
		{"system/questionnaire.rs", read, true},
		{"system/questionnaire.r", read, false},
		{"system/valueset.rs", read, false},
		{"system/DocumentReference.c", either, true},
		{"system/QuestionnaireResponse.c", either, true},
		{"system/DocumentReference.r", either, false},
		{"system/Claim.r", anyOf, true},
		{"system/Claim.c", anyOf, true},
		{"system/Claim.u", anyOf, false},
		{ProviderUmbrellaScope, none, true},
		{"system/*.*", none, false},
		{"system/Claim.*", none, false},
	} {
		if got := routeScopesPermit(row.granted, row.route); got != row.want {
			t.Errorf("routeScopesPermit(%q, %+v) = %v, want %v", row.granted, row.route, got, row.want)
		}
	}
}

// TestRouteScopeChallenge: the 403's challenge names SMART v1's scope on each
// of the route's resources, write for a create and read for a read, and the
// umbrella for a route that names none; the SMART configuration advertises
// the umbrella and each challenge's scopes, sorted, once.
func TestRouteScopeChallenge(t *testing.T) {
	create := routeScope{resources: []string{"DocumentReference", "QuestionnaireResponse"}, need: taskCreate}
	read := routeScope{resources: []string{"Claim"}, need: taskRead}
	for _, row := range []struct {
		route routeScope
		want  string
	}{
		{create, "system/DocumentReference.write system/QuestionnaireResponse.write"},
		{read, "system/Claim.read"},
		{routeScope{}, ProviderUmbrellaScope},
	} {
		if got := row.route.challenge(); got != row.want {
			t.Errorf("challenge %+v = %q, want %q", row.route, got, row.want)
		}
	}
	want := []string{ProviderUmbrellaScope, "system/Claim.read", "system/DocumentReference.write", "system/QuestionnaireResponse.write"}
	if got := providerScopes([]routeScope{routeScope{}, create, read, read}); !slices.Equal(got, want) {
		t.Errorf("providerScopes = %q, want %q", got, want)
	}
}

// TestCheckProviderIngressScopes: the provider's boot rule refuses a scope
// naming the umbrella, a wildcard, Subscription or one of the routes'
// resource types that SMART does not spell, the resource type in its own case,
// but the DTR IG's own scopes; a scope on another resource or context is kept.
func TestCheckProviderIngressScopes(t *testing.T) {
	resources := []string{"Claim", "Questionnaire"}
	for _, scope := range []string{
		ProviderUmbrellaScope, "system/Claim.c", "system/Claim.write", "system/Claim.c?patient=1", "system/*.rs",
		"system/*.write", "system/Subscription.*", "system/Questionnaire.rs", "system/questionnaire.rs",
		"system/valueset.rs", "system/library.rs", "system/Patient.read", "user/claim.read", "openid",
	} {
		if err := CheckProviderIngressScopes([]string{scope}, resources); err != nil {
			t.Errorf("%q refused: %v", scope, err)
		}
	}
	for _, scope := range []string{
		"system/davinci.write", "system/DAVINCI.WRITE", "system/claim.c", "system/Claim.wrote", "system/Claim.rc",
		"system/*.wrte", "system/*.", "system/subscription.*", "system/Subscription.wrte", "system/questionnaire.r",
		"SYSTEM/Claim.x",
	} {
		if err := CheckProviderIngressScopes([]string{ProviderUmbrellaScope, scope}, resources); err == nil {
			t.Errorf("%q started", scope)
		}
	}
}

// TestListingScope: a listing of bases takes any scope that some route under
// those bases takes (a pattern whose path is the base or below it), and no
// other: a route added under the bases widens it, and one beside them does
// not. The umbrella permits it through any alternative that takes the
// umbrella.
func TestListingScope(t *testing.T) {
	submit := routeScope{resources: []string{"Claim"}, need: taskCreate}
	subscribe := routeScope{resources: []string{"Subscription"}, need: taskCreate, noUmbrella: true}
	routes := []scopedPattern{
		{"POST /base/{id}/Claim/$submit", submit},
		{"POST /base/{id}/Subscription", subscribe},
		{"POST /base/{id}", submit},
		{"POST /based/{id}/Task", routeScope{resources: []string{"Task"}, need: taskCreate}},
		{"POST /base/{id}s/Task", routeScope{resources: []string{"Task"}, need: taskCreate}},
		{"GET /base/{id}s", routeScope{resources: []string{"Task"}, need: taskCreate}},
		{"POST /Questionnaire/$next-question", routeScope{resources: []string{"Questionnaire"}, need: taskRead}},
	}
	listing := listingScope("/base/{id}", routes, "")
	if len(listing.alternatives) != 3 {
		t.Fatalf("alternatives %+v, want the three routes under /base/{id}", listing.alternatives)
	}
	for _, row := range []struct {
		granted string
		want    bool
	}{
		{"system/Claim.c", true}, {"system/Subscription.c", true}, {"system/*.c", true}, {ProviderUmbrellaScope, true},
		{"system/Task.c", false}, {"system/Questionnaire.r", false}, {"system/Claim.r", false}, {"", false},
	} {
		if got := routeScopesPermit(row.granted, listing); got != row.want {
			t.Errorf("listing permits %q = %v, want %v", row.granted, got, row.want)
		}
	}
	if got, want := listing.challenge(), "system/Claim.write system/Subscription.write"; got != want {
		t.Errorf("challenge %q, want %q", got, want)
	}
	if got := listingScope("/nothing", routes, ""); routeScopesPermit(ProviderUmbrellaScope, got) {
		t.Error("a listing over no route permits the umbrella")
	}
}
