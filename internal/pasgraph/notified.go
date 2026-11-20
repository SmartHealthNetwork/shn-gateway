package pasgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
)

// A notified decision (the notifications design §7.4): a PAS Response Bundle,
// or a bare ClaimResponse, that a payer's content notification carries. The
// payer's gateway binds the notification's patients from it by the identity
// rules every PAS answer is read by (readEntry, SubjectIdentity), reading only
// what the binding needs: a defect that keeps a patient from being
// identified refuses (ErrUnbindable), and any other is reported (Defects),
// for the caller to check that no entry it could not read bears on a
// patient's binding (Scope). The decision's shape is the response graph
// rule's, read on its own (ValidateNotified).

// ErrUnbindable is a notified decision whose patient the network cannot
// identify: it carries no ClaimResponse, a ClaimResponse names no patient or
// one that resolves to no identity, or a defect touches the binding (a
// ClaimResponse's entry, or the identity a patient resolves to, claimed by an
// entry the network could not read or by two entries).
var ErrUnbindable = errors.New("pasgraph: the notified decision's patient cannot be identified")

// ErrOverLimit is a notified decision larger, or with more entries, than the
// gateway reads within its own limit (MaxBytes, MaxResources): its patient
// cannot be bound within that limit.
var ErrOverLimit = errors.New("pasgraph: the notified decision is over the limit the gateway reads within")

// Notified is a notified decision as read for its patients.
type Notified struct {
	// Graph is the decision's graph of the entries read cleanly, with
	// OpenSubjects set: a notification states no IG line, so a subject no
	// entry resolves binds by the Patient/<id> it names.
	Graph *Graph
	// Patients are each ClaimResponse's patient, in Bundle order.
	Patients []NotifiedPatient
	// Defects are the rules the Bundle or an entry fails that do not touch
	// the binding, each as the rule a PAS answer is refused by.
	Defects []error
	// bindDefective are the identities of the entries that were not read,
	// which no patient may be bound to.
	bindDefective []readFacts
	// scope is the decision as read cleanly, the comparison side of its
	// patients' binding (Scope).
	scope []byte
}

// Scope is the decision as read cleanly: the bare ClaimResponse as sent, or
// a Bundle of only the entries read cleanly, in Bundle order. A caller binds
// each member from the decision as sent and from Scope, and refuses when the
// two differ: an entry the network could not read then bears on the binding.
func (n *Notified) Scope() []byte { return n.scope }

// NotifiedPatient is one ClaimResponse's patient: the identity its patient
// reference denotes, and the Patient/<id> that identity names.
type NotifiedPatient struct {
	Response *Entry
	Identity string
	Tail     string
}

// Member is the id the patient's Patient/<id> names: the member the payer's
// gateway binds, as a PAS request's Claim.patient is bound.
func (p NotifiedPatient) Member() string { return strings.TrimPrefix(p.Tail, "Patient/") }

// ReadNotified reads raw, a notified decision, for its patients. err wraps
// ErrUnbindable or ErrOverLimit, with why.
func ReadNotified(raw []byte) (*Notified, error) { return ReadNotifiedAt(raw, "") }

// ReadNotifiedAt is ReadNotified for a decision carried in an entry whose
// fullUrl is fullURL ("" for none): a bare ClaimResponse is that entry's
// resource, so a relative reference in it resolves against that fullUrl when
// it is the ClaimResponse's RESTful identity (BareFullURL; FHIR R4's Bundle
// rule), and otherwise by the Patient/<id> it names.
func ReadNotifiedAt(raw []byte, fullURL string) (*Notified, error) {
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes, more than the %d it reads", ErrOverLimit, len(raw), MaxBytes)
	}
	g := &Graph{ByURL: make(map[string]*Entry), OpenSubjects: true}
	if err := DecodeObject(raw, &g.Bundle); err != nil {
		return nil, fmt.Errorf("%w: it cannot be read: %v", ErrUnbindable, err)
	}
	n := &Notified{Graph: g}
	var responses []*Entry
	switch g.Bundle["resourceType"] {
	case "ClaimResponse":
		// A bare ClaimResponse's entry identity is the fullUrl of the entry
		// carrying it: its patient binds by an absolute reference, a relative
		// one against a RESTful fullUrl, or else by the Patient/<id> it names.
		responses = append(responses, &Entry{Resource: g.Bundle, FullURL: BareFullURL(g.Bundle, fullURL)})
		n.scope = raw
	case "Bundle":
		if g.Bundle["type"] != "collection" {
			n.Defects = append(n.Defects, Refuse(RefusalNotCollection))
		}
		var ok bool
		g.Entries, ok = g.Bundle["entry"].([]any)
		if !ok || len(g.Entries) == 0 {
			return nil, fmt.Errorf("%w: the Bundle has no entries, so no ClaimResponse", ErrUnbindable)
		}
		if len(g.Entries) > MaxResources {
			return nil, fmt.Errorf("%w: %d entries, more than the %d it reads", ErrOverLimit, len(g.Entries), MaxResources)
		}
		var defective []readFacts
		for i, v := range g.Entries {
			entry, facts, err := g.readEntry(i, v)
			if err != nil {
				// A ClaimResponse the network cannot read names a patient it
				// cannot bind. Any other entry, a Patient's included, touches
				// the binding only when a patient resolves to the identity
				// it claims (bind); otherwise it is a defect elsewhere.
				if jsonmember.FoldedName(facts.typ) == jsonmember.FoldedName("ClaimResponse") {
					return nil, fmt.Errorf("%w: %v", ErrUnbindable, err)
				}
				if facts.full != "" || facts.typ != "" {
					defective = append(defective, facts)
				}
				n.Defects = append(n.Defects, err)
				continue
			}
			if entry.ResourceType() == "ClaimResponse" {
				responses = append(responses, entry)
			}
		}
		// An identity a defective entry claimed is one no binding may rest
		// on (bind): two entries claim it, or the entry that claims it was
		// not read. Elsewhere it is a defect like any other.
		n.bindDefective = defective
		clean := make([]map[string]any, 0, len(g.ByURL))
		for _, e := range g.Ordered() {
			clean = append(clean, map[string]any{"fullUrl": e.FullURL, "resource": e.Resource})
		}
		scope, err := json.Marshal(map[string]any{"resourceType": "Bundle", "type": "collection", "entry": clean})
		if err != nil {
			return nil, fmt.Errorf("%w: it cannot be read: %v", ErrUnbindable, err)
		}
		n.scope = scope
	default:
		return nil, fmt.Errorf("%w: it is neither a Bundle nor a ClaimResponse", ErrUnbindable)
	}
	if len(responses) == 0 {
		return nil, fmt.Errorf("%w: it carries no ClaimResponse", ErrUnbindable)
	}
	for _, r := range responses {
		p, why := n.bind(r)
		if why != "" {
			return nil, fmt.Errorf("%w: %s %s", ErrUnbindable, r.Label(), why)
		}
		n.Patients = append(n.Patients, p)
	}
	return n, nil
}

// BareFullURL is fullURL, the fullUrl of the entry carrying cr, a bare
// ClaimResponse, when it is cr's RESTful identity: an http or https address,
// versionless, with no query or fragment, ending in /ClaimResponse/<cr's own
// id>, as an entry's fullUrl is checked (readEntry). Any other fullUrl is no
// base its references resolve against, so "" (they resolve by the
// Patient/<id> they name).
func BareFullURL(cr map[string]any, fullURL string) string {
	u, err := url.Parse(fullURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(fullURL, "/_history/") {
		return ""
	}
	id, ok := cr["id"].(string)
	if !ok || !SafeResourceID(id) || !strings.HasSuffix(u.Path, "/ClaimResponse/"+id) {
		return ""
	}
	return fullURL
}

// bind is r's patient: the identity its patient reference denotes, by
// SubjectIdentity, or, when that denotes none and no entry names it, by the
// Patient/<id> it names (OpenSubjects); why when it denotes no patient, or
// one a defective entry claims.
func (n *Notified) bind(r *Entry) (NotifiedPatient, string) {
	patient, _ := r.Resource["patient"].(map[string]any)
	ref, _ := patient["reference"].(string)
	if ref == "" {
		return NotifiedPatient{}, "names no patient by reference"
	}
	identity, why := n.Graph.SubjectIdentity(r, ref)
	if why != "" {
		// A tail a defective entry claims is refused below, as any identity
		// one claims is.
		if tail := PatientTail(ref); tail != "" && n.Graph.NamesNoEntry(tail) {
			identity, why = tail, ""
		}
	}
	if why != "" {
		return NotifiedPatient{}, fmt.Sprintf("/patient %q %s", ref, why)
	}
	tail := PatientTail(identity)
	if tail == "" {
		// An identity with no Patient/<id> in it (a urn:uuid or urn:oid
		// fullUrl, FHIR R4's Bundle resolution matching the entry whose
		// fullUrl it is exactly) names the Patient entry it resolves to, by
		// that Patient's id.
		if e := n.Graph.ByURL[identity]; e != nil && e.ResourceType() == "Patient" {
			if id, ok := e.Resource["id"].(string); ok && SafeResourceID(id) {
				tail = "Patient/" + id
			}
		}
	}
	if tail == "" {
		return NotifiedPatient{}, fmt.Sprintf("/patient %q denotes %q, which names no Patient by an id", ref, identity)
	}
	for _, d := range n.bindDefective {
		if d.claims(identity, tail) {
			return NotifiedPatient{}, fmt.Sprintf("/patient %q denotes %q, which an entry the network could not read claims", ref, identity)
		}
	}
	return NotifiedPatient{Response: r, Identity: identity, Tail: tail}, ""
}

// claims reports whether the entry d, which the network could not read,
// claims the patient identity denotes, tail its Patient/<id>: by its fullUrl,
// the identity itself or one ending in tail at any base, or by a Patient
// resource whose id is tail's, wherever its fullUrl points or with none. The
// member's demographics are read from any Patient with its id the decision
// carries, so an unreadable one bears on the binding wherever it sits.
func (d readFacts) claims(identity, tail string) bool {
	switch {
	case d.full != "" && (d.full == identity || strings.HasSuffix(d.full, "/"+tail)):
		return true
	case jsonmember.FoldedName(d.typ) == jsonmember.FoldedName("Patient") && d.id != "" && "Patient/"+d.id == tail:
		return true
	}
	return false
}

// NotifiedLines are the IG lines a notified decision's graph is read at when
// the payer declares none, in order: a notification states no IG line of its
// own, so the decision is read as a PAS $submit or amendment answer at each
// line it could be on, each by the response graph rule at that line
// (ValidateAnswerAt), the line that requires every reference to resolve
// first.
var NotifiedLines = []string{"2.0.1", "2.2", "2.1"}

// ValidateNotified reads raw, a notified decision that is a PAS Response
// Bundle, by the response graph rule a PAS answer is read by
// (ValidateBundleAt), at each of lines: the lines the payer declares, or
// NotifiedLines when it declares none (lines empty). line is the first it
// meets; when it meets none, err names each line tried and the rule the
// decision fails there (one ClaimResponse, a collection, entries the rule
// reads, every reference resolving as that line requires).
func ValidateNotified(raw []byte, lines []string) (line string, err error) {
	where := "the IG lines the payer declares"
	if len(lines) == 0 {
		lines, where = NotifiedLines, "the IG lines it could be on"
	}
	failures := make([]string, 0, len(lines))
	for _, l := range lines {
		e := ValidateBundleAt(raw, l)
		if e == nil {
			return l, nil
		}
		failures = append(failures, l+": "+strings.TrimPrefix(e.Error(), "PAS response graph: "))
	}
	return "", errors.New("it meets the response graph rule at none of " + where + " (" + strings.Join(failures, "; ") + ")")
}
