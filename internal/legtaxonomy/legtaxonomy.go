// Package legtaxonomy is the gateway's copy of the network's leg taxonomy:
// the same data files as the root module's internal/legtaxonomy (pa.json and
// fqs.json), embedded here because the gateway builds alone (GOWORK=off) and
// cannot import the root module. Each file must be byte-identical to the
// root's; the root module's hash fence (internal/legtaxonomy,
// TestLegTaxonomy_EveryCopyHasTheSameHash) compares every tracked
// *legtaxonomy/*.json with it.
//
// This package only decodes the file. The root package owns the taxonomy's
// invariants and checks them on the same bytes; the checks here are the ones
// the engine relies on to derive its catalog.
package legtaxonomy

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

//go:embed pa.json
var paFile []byte

//go:embed fqs.json
var fqsFile []byte

// Physics and payload binding values the engine reads.
const (
	PhysicsRequestResponse = "request-response"
	PayloadRequired        = "required"
)

// Taxonomy is one workstream's file.
type Taxonomy struct {
	Workstream      string `json:"workstream"`
	TaxonomyVersion int    `json:"taxonomyVersion"`
	Legs            []Leg  `json:"legs"`
}

// Leg is one row.
type Leg struct {
	TransactionType string   `json:"transactionType"`
	StagedName      string   `json:"stagedName"`
	Physics         string   `json:"physics"`
	Request         Side     `json:"request"`
	Response        *Side    `json:"response"`
	Scope           string   `json:"scope"`
	PayloadBinding  string   `json:"payloadBinding"`
	Consent         *Consent `json:"consent"`
	Observe         bool     `json:"observe"`
	FrameOperations []string `json:"frameOperations"`
	Advertise       bool     `json:"advertise"`
}

// Side is one side of a leg.
type Side struct {
	Frame     string   `json:"frame"`
	Roles     []string `json:"roles"`
	Operation string   `json:"operation"`
}

// Consent is a leg's consent conjunct.
type Consent struct {
	Purpose       string `json:"purpose"`
	CustodianFrom string `json:"custodianFrom"`
}

// Routable reports whether the leg rides an envelope through the Hub: a
// request-response leg whose token binds the payload. Parse refuses such a
// leg without a response side.
func (l Leg) Routable() bool {
	return l.Physics == PhysicsRequestResponse && l.PayloadBinding == PayloadRequired
}

// PA returns a fresh copy of the PA taxonomy. The embedded file is checked
// when the package loads, so a malformed copy fails every binary and test
// that links it.
func PA() Taxonomy {
	t, err := Parse(paFile)
	if err != nil {
		panic(err)
	}
	return t
}

// FQS returns a fresh copy of the Federated Query Services taxonomy: the
// federated query a PA attachment fetch composes. It is checked when the
// package loads, as PA's is.
func FQS() Taxonomy {
	t, err := Parse(fqsFile)
	if err != nil {
		panic(err)
	}
	return t
}

func init() {
	for _, b := range [][]byte{paFile, fqsFile} {
		if _, err := Parse(b); err != nil {
			panic(err)
		}
	}
}

// Parse decodes a taxonomy file strictly (unknown fields and trailing data
// refused) and checks what the engine derives from it: each routable leg has
// a transaction type, a response side, both sides' frame and operation, and a
// scope, and no transaction type or operation appears twice.
func Parse(b []byte) (Taxonomy, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var t Taxonomy
	if err := dec.Decode(&t); err != nil {
		return Taxonomy{}, fmt.Errorf("legtaxonomy: decode: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Taxonomy{}, errors.New("legtaxonomy: trailing data after the taxonomy")
	}
	var errs []error
	tx := map[string]bool{}
	ops := map[string]bool{}
	for i, l := range t.Legs {
		if l.TransactionType == "" || tx[l.TransactionType] {
			errs = append(errs, fmt.Errorf("legtaxonomy: leg %d: transactionType %q is empty or repeated", i, l.TransactionType))
		}
		tx[l.TransactionType] = true
		if l.Physics == PhysicsRequestResponse && l.Response == nil {
			errs = append(errs, fmt.Errorf("legtaxonomy: leg %q: a request-response leg needs a response side", l.TransactionType))
		}
		sides := []Side{l.Request}
		if l.Response != nil {
			sides = append(sides, *l.Response)
		}
		for _, s := range sides {
			if s.Operation == "" || ops[s.Operation] {
				errs = append(errs, fmt.Errorf("legtaxonomy: leg %q: operation %q is empty or repeated", l.TransactionType, s.Operation))
			}
			ops[s.Operation] = true
			if l.Routable() && s.Frame == "" {
				errs = append(errs, fmt.Errorf("legtaxonomy: leg %q: a side has no frame", l.TransactionType))
			}
		}
		if l.Routable() && l.Scope == "" {
			errs = append(errs, fmt.Errorf("legtaxonomy: leg %q: empty scope", l.TransactionType))
		}
	}
	if len(t.Legs) == 0 {
		errs = append(errs, errors.New("legtaxonomy: no legs"))
	}
	return t, errors.Join(errs...)
}
