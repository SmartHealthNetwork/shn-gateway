// Package cdslisting reads a CDS Hooks discovery document (a CDS service
// listing, GET {base}/cds-services) by the one rule the gateway holds a
// listing to: CRD's own listing read (engine.DiscoverCDSServices) and the
// payer's connectivity check (checks, kind cds-services) both read it here, so
// the check passes exactly the listings CRD can use.
package cdslisting

import (
	"encoding/json"
	"errors"
	"fmt"
)

// MaxBody is the most of a listing's body either reader reads: a longer one
// is cut there, and so fails to parse for both alike.
const MaxBody = 8 << 20

// Service is one entry of a CDS service listing.
type Service struct {
	ID   string
	Hook string
}

// Read reads a discovery document's services as JSON specifies, member names
// case-sensitively (RFC 8259): its exact "services" member, and each entry's
// exact "id" and "hook". A struct-tag decode would fold "ID" into "id", and of
// two members it folds together take the last, reading another service than
// the payer listed. The body must be one JSON object whose services is an
// array, each entry an object naming a non-empty string id and hook.
func Read(body []byte) ([]Service, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	raw, ok := doc["services"]
	if !ok || string(raw) == "null" {
		return nil, errors.New("lists no services array")
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse services: %w", err)
	}
	services := make([]Service, 0, len(entries))
	for i, e := range entries {
		var s Service
		for name, to := range map[string]*string{"id": &s.ID, "hook": &s.Hook} {
			if v, ok := e[name]; ok {
				if err := json.Unmarshal(v, to); err != nil {
					return nil, fmt.Errorf("parse service %d's %s: %w", i, name, err)
				}
			}
		}
		if s.ID == "" || s.Hook == "" {
			return nil, fmt.Errorf("service %d has no id or no hook", i)
		}
		services = append(services, s)
	}
	return services, nil
}
