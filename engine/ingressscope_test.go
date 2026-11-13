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
	} {
		if err := CheckPayerIngressScopes(scopes); err != nil {
			t.Errorf("%v: refused, want taken: %v", scopes, err)
		}
	}
	for _, row := range []struct {
		scopes []string
		want   string
	}{
		{nil, "lists no system/Task scope that permits creating a Task"},
		{[]string{"system/Davinci.write"}, "lists no system/Task scope that permits creating a Task"},
		{[]string{"system/Patient.read"}, "lists no system/Task scope that permits creating a Task"},
		{[]string{"system/Task.c?status=requested"}, "lists no system/Task scope that permits creating a Task"},
		{[]string{"system/Task.uds"}, "lists no system/Task scope that permits creating a Task"},
		// Registered only to read: a payer's ingress is for creating its Task,
		// and its read routes read one its system created.
		{[]string{"system/Task.read"}, "lists no system/Task scope that permits creating a Task"},
		{[]string{"system/Task.rs"}, "lists no system/Task scope that permits creating a Task"},
		{[]string{"system/Task.read", "system/Task.c?status=requested"}, "lists no system/Task scope that permits creating a Task"},
		{[]string{"system/Davinci.write", "system/Task.u"}, "lists no system/Task scope that permits creating a Task"},
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
