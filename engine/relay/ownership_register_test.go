package relay

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// mustPanic runs f and fails t unless it panics with a message containing
// want.
func mustPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("no panic; want one containing %q", want)
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, want) {
			t.Fatalf("panic %q; want one containing %q", msg, want)
		}
	}()
	f()
}

var (
	toyCarried = Key{"toy-leg", RoleRecipient, DirectionRequest, OutcomeCarried}
	toyRows    = map[Key]Rule{toyCarried: {Allowed: []Ownership{OwnershipRelayed}}}
)

// Before any workstream registers, every transmit is refused with an error
// that names the cause, and nothing is sent.
func TestOwnershipUnregisteredFailsClosed(t *testing.T) {
	var tbl ownershipTable
	p := Exact(NewBody([]byte(`{}`), OriginPeerFrame), fhirJSON)
	err := tbl.check(toyCarried)(p)
	var oe *OwnershipError
	if !errors.Is(err, ErrNoOwnershipRegistered) || !errors.Is(err, ErrOwnershipRefused) || !errors.As(err, &oe) || oe.Key != toyCarried {
		t.Fatalf("want an *OwnershipError matching ErrNoOwnershipRegistered and ErrOwnershipRefused, got %v", err)
	}
	if !strings.Contains(err.Error(), "no workstream ownership registered") {
		t.Fatalf("the error does not name the cause: %v", err)
	}
	if _, terr := Transmit(p, tbl.check(toyCarried)); terr == nil {
		t.Fatal("Transmit sent a payload before any registration")
	}
	if tbl.current() != nil {
		t.Fatal("an unregistered table has rows")
	}
	// A refusal for any other reason is not mistaken for this one.
	tbl2 := ownershipTable{}
	tbl2.register([]string{"toy-leg"}, toyRows)
	other := tbl2.check(Key{"toy-leg", RoleRecipient, DirectionResponse, OutcomeAnswered})(p)
	if !errors.Is(other, ErrOwnershipRefused) || errors.Is(other, ErrNoOwnershipRegistered) {
		t.Fatalf("an unlisted key's refusal: %v", other)
	}
}

// Registration is write-once: a second registration, and one after the table
// was read, panic.
func TestRegisterOwnershipIsWriteOnce(t *testing.T) {
	var twice ownershipTable
	twice.register([]string{"toy-leg"}, toyRows)
	mustPanic(t, "registered twice", func() { twice.register([]string{"toy-leg"}, toyRows) })

	p := Exact(NewBody([]byte(`{}`), OriginPeerFrame), fhirJSON)
	// Check, LegOwnership and Legs are each a table method on the package's
	// table, so each read is checked here on a table of its own.
	for name, read := range map[string]func(*ownershipTable){
		"Check":        func(tbl *ownershipTable) { _ = tbl.check(toyCarried)(p) },
		"LegOwnership": func(tbl *ownershipTable) { _ = tbl.ownership() },
		"Legs":         func(tbl *ownershipTable) { _ = tbl.legs() },
	} {
		var late ownershipTable
		read(&late)
		mustPanic(t, "registered after the table was read", func() { late.register([]string{"toy-leg"}, toyRows) })
		if late.current() != nil {
			t.Errorf("%s: a refused registration left rows", name)
		}
	}
}

// A registration adds the rows every leg has, keeps its own copy, and is
// refused when its legs or rows are malformed.
func TestRegisterOwnershipBuildsAndChecksTheTable(t *testing.T) {
	legs := []string{"toy-b", "toy-a"}
	rows := map[Key]Rule{
		{"toy-a", RoleRequester, DirectionRequest, OutcomeOriginated}: {Allowed: []Ownership{OwnershipAuthored}, Builders: []BuilderID{BuilderSDKPASSubmit}},
		{"toy-b", RoleRecipient, DirectionResponse, OutcomeAnswered}:  {Allowed: []Ownership{OwnershipRelayed}},
	}
	var tbl ownershipTable
	tbl.register(legs, rows)
	legs[0] = "changed"
	for k := range rows {
		r := rows[k]
		r.Allowed[0] = OwnershipEdited
		rows[k] = r
	}
	r := tbl.current()
	if !slices.Equal(r.legs, []string{"toy-a", "toy-b"}) {
		t.Fatalf("legs %v, want sorted and unchanged by the caller", r.legs)
	}
	want := 2 + 2 /* requester upstream errors */ + 2*3 /* refusals: two legs and LegUnestablished, two roles */
	if len(r.rules) != want {
		t.Fatalf("%d rows, want %d", len(r.rules), want)
	}
	for _, k := range []Key{
		{"toy-a", RoleRequester, DirectionResponse, OutcomeUpstreamError},
		{"toy-b", RoleRecipient, DirectionResponse, OutcomeRefused},
		{LegUnestablished, RoleRequester, DirectionResponse, OutcomeRefused},
	} {
		if _, ok := r.rules[k]; !ok {
			t.Errorf("the table has no %s row", k)
		}
	}
	if got := r.rules[Key{"toy-b", RoleRecipient, DirectionResponse, OutcomeAnswered}].Allowed; !slices.Equal(got, []Ownership{OwnershipRelayed}) {
		t.Fatalf("the table shares a rule with its caller: %v", got)
	}

	ok := Rule{Allowed: []Ownership{OwnershipRelayed}}
	for name, c := range map[string]struct {
		legs []string
		rows map[Key]Rule
		want string
	}{
		"no legs":                                {nil, nil, "no legs"},
		"an unnamed leg":                         {[]string{"toy-leg", ""}, nil, "has no name"},
		"a leg twice":                            {[]string{"toy-leg", "toy-leg"}, nil, `"toy-leg" registered twice`},
		"an unregistered leg":                    {[]string{"toy-leg"}, map[Key]Rule{{"other", RoleRecipient, DirectionRequest, OutcomeCarried}: ok}, "is not registered"},
		"an invalid role":                        {[]string{"toy-leg"}, map[Key]Rule{{"toy-leg", 0, DirectionRequest, OutcomeCarried}: ok}, "invalid role"},
		"an outcome the direction cannot carry":  {[]string{"toy-leg"}, map[Key]Rule{{"toy-leg", RoleRecipient, DirectionRequest, OutcomeAnswered}: ok}, "does not fit the direction"},
		"a refusal row":                          {[]string{"toy-leg"}, map[Key]Rule{{"toy-leg", RoleRecipient, DirectionResponse, OutcomeRefused}: ok}, "refusals are the table's own rows"},
		"the requester's application error":      {[]string{"toy-leg"}, map[Key]Rule{{"toy-leg", RoleRequester, DirectionResponse, OutcomeUpstreamError}: ok}, "is the table's own row"},
		"a row that permits nothing":             {[]string{"toy-leg"}, map[Key]Rule{toyCarried: {}}, "permits nothing"},
		"an ownership value that does not exist": {[]string{"toy-leg"}, map[Key]Rule{toyCarried: {Allowed: []Ownership{0}}}, "invalid ownership"},
		"edits without Edited":                   {[]string{"toy-leg"}, map[Key]Rule{toyCarried: {Allowed: []Ownership{OwnershipRelayed}, Edits: []EditID{EditPayorEdgeRestamp}}}, "edits are listed without Edited"},
		"Edited without edits":                   {[]string{"toy-leg"}, map[Key]Rule{toyCarried: {Allowed: []Ownership{OwnershipEdited}}}, "Edited without edits"},
		"builders without Authored":              {[]string{"toy-leg"}, map[Key]Rule{toyCarried: {Allowed: []Ownership{OwnershipRelayed}, Builders: []BuilderID{BuilderSDKPASSubmit}}}, "builders are listed without Authored"},
		"Authored without builders":              {[]string{"toy-leg"}, map[Key]Rule{toyCarried: {Allowed: []Ownership{OwnershipAuthored}}}, "Authored without builders"},
		"an unregistered edit":                   {[]string{"toy-leg"}, map[Key]Rule{toyCarried: {Allowed: []Ownership{OwnershipEdited}, Edits: []EditID{"E-99"}}}, "edit E-99 is not registered"},
		"an unregistered builder":                {[]string{"toy-leg"}, map[Key]Rule{toyCarried: {Allowed: []Ownership{OwnershipAuthored}, Builders: []BuilderID{"made-up"}}}, "builder made-up is not registered"},
		"the test builder":                       {[]string{"toy-leg"}, map[Key]Rule{toyCarried: {Allowed: []Ownership{OwnershipAuthored}, Builders: []BuilderID{builderTestInjected}}}, "builder test-injected is not registered"},
		"the gateway's refusal":                  {[]string{"toy-leg"}, map[Key]Rule{{"toy-leg", RoleRecipient, DirectionResponse, OutcomeAnswered}: {Allowed: []Ownership{OwnershipAuthored}, Builders: []BuilderID{BuilderGatewayRefusal}}}, "the gateway's refusal is the table's own row"},
	} {
		t.Run(name, func(t *testing.T) {
			var tbl ownershipTable
			mustPanic(t, c.want, func() { tbl.register(c.legs, c.rows) })
			if tbl.reg.Load() != nil {
				t.Fatal("a refused registration left rows")
			}
		})
	}
}
