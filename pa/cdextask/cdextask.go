// Package cdextask reads a CDex Task Attachment Request (CDex 2.1.0,
// cdex-task-attachment-request) by structure: the Task a payer's system sends
// to ask a provider for documentation. It reads what the network decides on
// and nothing else:
//   - Task.code's codings, by system and code;
//   - the patient Task.for names: the contained Patient's identifiers, and
//     its bytes, for binding the request to a patient;
//   - Task.owner: an identifier on the reference itself, the
//     organization and practitioner identifiers of the contained
//     PractitionerRole it names, or the identifiers of the contained
//     Organization it names;
//   - Task.requester: its reference and identifier;
//   - the payer-url input: the input whose type is PASTempCodes#payer-url,
//     and its valueUrl.
//
// Unknown is not empty. Each field reads as Absent (the Task does not carry
// it), Unreadable (it carries it in a form this reader cannot read, or
// ambiguously), or Present with its value. A missing or unreadable field is
// never an empty value: every string read is non-empty. A member FHIR JSON
// does not allow to be empty (null, an empty array, a Reference with neither
// a reference nor an identifier) reads as unreadable, never as absent. A
// coding with no system or no code cannot match a system and code, so it is
// passed over, never read as a match or as a failure. Nothing is decided from
// text: no display, diagnostics or narrative is read.
//
// The body is scanned strictly first (relay.Decode), and every value under it
// is decoded with exact member names. A body that can be read two ways is not
// read at all: one that repeats a member name, exactly or but for case
// (relay.ErrDuplicateKey), or that names a member the reader reads only in
// another case (relay.ErrFieldNameCase), at any depth.
package cdextask

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// The codes the reader decides on.
const (
	// PASTempCodes is the PAS code system CDex 2.1.0 types an attachment
	// request's code and its payer-url input with.
	PASTempCodes = "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes"
	// CodeAttachmentRequest is Task.code for a request for attachments by
	// LOINC or PWK01 code.
	CodeAttachmentRequest = "attachment-request-code"
	// CodeAttachmentRequestQuestionnaire is Task.code for a request for a
	// questionnaire to be completed.
	CodeAttachmentRequestQuestionnaire = "attachment-request-questionnaire"
	// InputPayerURL is the input type code of the address the provider sends
	// its answer to.
	InputPayerURL = "payer-url"
)

// ErrNotTask is the error of a body that is not a Task resource.
var ErrNotTask = errors.New("cdextask: not a Task")

// State is what a field reads as.
type State uint8

const (
	// Absent: the Task does not carry the field.
	Absent State = iota
	// Unreadable: the Task carries the field in a form this reader cannot
	// read, or ambiguously.
	Unreadable
	// Present: the field is read, and its value is set.
	Present
)

func (s State) String() string {
	switch s {
	case Absent:
		return "absent"
	case Unreadable:
		return "unreadable"
	case Present:
		return "present"
	}
	return fmt.Sprintf("State(%d)", uint8(s))
}

// Coding is a coding's system and code. Both are set.
type Coding struct {
	System, Code string
}

// Identifier is an identifier's system and value, with its type's codings.
// Value is set; System is "" only when the identifier names none.
type Identifier struct {
	System, Value string
	Type          []Coding
}

// Codes is Task.code. It is Unreadable when it holds no coding with both a
// system and a code: it cannot then be decided by structure.
type Codes struct {
	State   State
	Codings []Coding
}

// Has reports whether Task.code is read and holds a coding of system and code.
func (c Codes) Has(system, code string) bool {
	if c.State != Present {
		return false
	}
	for _, x := range c.Codings {
		if x.System == system && x.Code == code {
			return true
		}
	}
	return false
}

// IdentifierField is one identifier: a Reference's identifier.
type IdentifierField struct {
	State      State
	Identifier Identifier
}

// Patient is the patient Task.for names.
type Patient struct {
	// State is Task.for's: Absent when the Task has none; Unreadable when it
	// cannot be read, holds neither a reference nor an identifier, or names
	// a contained resource that is not exactly one Patient with readable
	// identifiers.
	State State
	// Reference is Task.for.reference as written, "" when it has none.
	Reference string
	// Identifier is Task.for.identifier.
	Identifier IdentifierField
	// Contained reports whether Reference names a contained resource ("#id").
	// When it does not, Resource and Identifiers are not set: the reader
	// reads no resource outside the Task.
	Contained bool
	// Resource is the contained Patient, as the Task carries it.
	Resource []byte
	// Identifiers are the contained Patient's identifiers, in order. A
	// contained Patient with no identifier member has none.
	Identifiers []Identifier
}

// Owner is Task.owner.
type Owner struct {
	// State is Task.owner's own: Absent when the Task has none; Unreadable
	// when it cannot be read or holds neither a reference nor an identifier.
	State State
	// Reference is Task.owner.reference as written, "" when it has none.
	Reference string
	// Identifier is Task.owner.identifier.
	Identifier IdentifierField
	// Organization and Practitioner are the identifiers of the contained
	// PractitionerRole that Reference names ("#id"): its organization's and
	// its practitioner's reference identifiers. Both are Absent when
	// Reference is not a local "#id" reference or names a contained
	// Organization, and Unreadable when it names no contained resource,
	// several, or one that is neither a PractitionerRole nor an Organization.
	Organization IdentifierField
	Practitioner IdentifierField
	// ContainedOrganization is the contained Organization Reference names
	// ("#id"), or the one the contained PractitionerRole's organization
	// names by a local reference. It is Absent unless Reference names exactly
	// one or the role's organization is a local reference, and Unreadable
	// when that reference names no one contained Organization or the
	// Organization's identifiers cannot be read.
	ContainedOrganization ContainedOrganization
}

// ContainedOrganization is a contained Organization's identifiers, in order.
// One with no identifier member is Present with none.
type ContainedOrganization struct {
	State       State
	Identifiers []Identifier
}

// Requester is Task.requester.
type Requester struct {
	// State is Task.requester's own: Absent when the Task has none;
	// Unreadable when it cannot be read or holds neither a reference nor an
	// identifier.
	State State
	// Reference is Task.requester.reference as written, "" when it has none.
	Reference string
	// Identifier is Task.requester.identifier.
	Identifier IdentifierField
}

// PayerURL is the payer-url input.
type PayerURL struct {
	// State is Absent when no input is typed PASTempCodes#payer-url, and
	// Unreadable when several are, when an input or its type cannot be read
	// (it could be one), or when the one input's value is not a non-empty
	// valueUrl (Form names the value member it has).
	State State
	// Value is the valueUrl, set when State is Present.
	Value string
	// Index is the input's position in Task.input, set when exactly one input
	// is typed payer-url (-1 otherwise).
	Index int
	// Form is the value member the one payer-url input carries ("valueUrl",
	// "valueString", …), "" when it has none or there is not exactly one.
	Form string
}

// Task is what the reader reads from a Task Attachment Request.
type Task struct {
	Code      Codes
	Patient   Patient
	Owner     Owner
	Requester Requester
	PayerURL  PayerURL
}

// Read reads body, a Task. It returns an error, and reads nothing, when body
// is not one JSON object, when it can be read two ways (relay.ErrDuplicateKey,
// relay.ErrFieldNameCase: a caller refuses it as the engine refuses any body
// that reads two ways), or when it is not a Task (ErrNotTask).
func Read(body relay.Body) (Task, error) {
	var top struct {
		ResourceType json.RawMessage `json:"resourceType"`
		Code         json.RawMessage `json:"code"`
		For          json.RawMessage `json:"for"`
		Owner        json.RawMessage `json:"owner"`
		Requester    json.RawMessage `json:"requester"`
		Input        json.RawMessage `json:"input"`
		Contained    json.RawMessage `json:"contained"`
	}
	var whole json.RawMessage
	if err := relay.Decode(body, &whole); err != nil {
		return Task{}, fmt.Errorf("cdextask: %w", err)
	}
	r := &reader{origin: body.Origin()}
	if err := r.decode(whole, &top); err != nil {
		if r.twoWays != nil {
			return Task{}, fmt.Errorf("cdextask: %w", r.twoWays)
		}
		return Task{}, fmt.Errorf("cdextask: %w", err)
	}
	if rt, ok := r.str(top.ResourceType); !ok || rt != "Task" {
		if r.twoWays != nil {
			return Task{}, fmt.Errorf("cdextask: %w", r.twoWays)
		}
		return Task{}, ErrNotTask
	}
	r.contained = r.readContained(top.Contained)
	task := Task{
		Code:      r.codes(top.Code),
		Patient:   r.patient(top.For),
		Owner:     r.owner(top.Owner),
		Requester: r.requester(top.Requester),
		PayerURL:  r.payerURL(top.Input),
	}
	if r.twoWays != nil {
		return Task{}, fmt.Errorf("cdextask: %w", r.twoWays)
	}
	return task, nil
}

type reader struct {
	origin    relay.Origin
	contained []containedResource
	// twoWays is the first finding that the body reads two ways: a member
	// named only in another case than the one the reader reads.
	twoWays error
}

type containedResource struct {
	resourceType, id string
	raw              json.RawMessage
}

// errNull is the decode error of a JSON null: FHIR JSON never carries one.
var errNull = errors.New("cdextask: null")

// absent reports whether raw is a member the Task does not carry.
func absent(raw json.RawMessage) bool { return len(raw) == 0 }

// decode decodes raw into v with exact member names. A null is an error, and
// a member named only in another case is recorded as the body reading two
// ways.
func (r *reader) decode(raw json.RawMessage, v any) error {
	if string(raw) == "null" {
		return errNull
	}
	err := relay.Decode(relay.NewBody(raw, r.origin), v)
	if errors.Is(err, relay.ErrFieldNameCase) && r.twoWays == nil {
		r.twoWays = err
	}
	return err
}

// str reads raw as a non-empty JSON string.
func (r *reader) str(raw json.RawMessage) (string, bool) {
	var s string
	if absent(raw) || r.decode(raw, &s) != nil || s == "" {
		return "", false
	}
	return s, true
}

// list reads raw as a non-empty JSON array.
func (r *reader) list(raw json.RawMessage) ([]json.RawMessage, bool) {
	var items []json.RawMessage
	if r.decode(raw, &items) != nil || len(items) == 0 {
		return nil, false
	}
	return items, true
}

// codings reads raw, a CodeableConcept, as each coding that has both a system
// and a code. A coding missing either is passed over; one with either of
// another JSON type, or empty, makes the whole unreadable, as does any member
// that is not of its type.
func (r *reader) codings(raw json.RawMessage) ([]Coding, bool) {
	var cc struct {
		Coding json.RawMessage `json:"coding"`
	}
	if r.decode(raw, &cc) != nil {
		return nil, false
	}
	if absent(cc.Coding) {
		return nil, true
	}
	items, ok := r.list(cc.Coding)
	if !ok {
		return nil, false
	}
	// Every coding is decoded before the whole is decided, so a member a
	// later coding names in another case is found (reader.twoWays).
	var out []Coding
	bad := false
	for _, c := range items {
		var x struct {
			System json.RawMessage `json:"system"`
			Code   json.RawMessage `json:"code"`
		}
		if r.decode(c, &x) != nil {
			bad = true
			continue
		}
		if absent(x.System) || absent(x.Code) {
			continue
		}
		system, ok1 := r.str(x.System)
		code, ok2 := r.str(x.Code)
		if !ok1 || !ok2 {
			bad = true
			continue
		}
		out = append(out, Coding{System: system, Code: code})
	}
	if bad {
		return nil, false
	}
	return out, true
}

func (r *reader) codes(raw json.RawMessage) Codes {
	if absent(raw) {
		return Codes{State: Absent}
	}
	c, ok := r.codings(raw)
	if !ok || len(c) == 0 {
		return Codes{State: Unreadable}
	}
	return Codes{State: Present, Codings: c}
}

// identifier reads raw, an Identifier: a non-empty value, a system when it
// has one, and its type's codings.
func (r *reader) identifier(raw json.RawMessage) (Identifier, bool) {
	var x struct {
		System json.RawMessage `json:"system"`
		Value  json.RawMessage `json:"value"`
		Type   json.RawMessage `json:"type"`
	}
	if r.decode(raw, &x) != nil {
		return Identifier{}, false
	}
	// Each member is decoded before the identifier is decided.
	id := Identifier{}
	value, ok := r.str(x.Value)
	id.Value = value
	if !absent(x.System) {
		var okSystem bool
		id.System, okSystem = r.str(x.System)
		ok = ok && okSystem
	}
	if !absent(x.Type) {
		var okType bool
		id.Type, okType = r.codings(x.Type)
		ok = ok && okType
	}
	if !ok {
		return Identifier{}, false
	}
	return id, true
}

func (r *reader) identifierField(raw json.RawMessage) IdentifierField {
	if absent(raw) {
		return IdentifierField{State: Absent}
	}
	id, ok := r.identifier(raw)
	if !ok {
		return IdentifierField{State: Unreadable}
	}
	return IdentifierField{State: Present, Identifier: id}
}

// reference reads raw, a Reference that the Task carries, as its reference
// string and its identifier. ok is false when raw cannot be read, its
// reference is present but not a non-empty string, or it holds neither a
// reference nor an identifier.
func (r *reader) reference(raw json.RawMessage) (ref string, identifier IdentifierField, ok bool) {
	var x struct {
		Reference  json.RawMessage `json:"reference"`
		Identifier json.RawMessage `json:"identifier"`
	}
	if r.decode(raw, &x) != nil {
		return "", IdentifierField{}, false
	}
	if absent(x.Reference) && absent(x.Identifier) {
		return "", IdentifierField{}, false
	}
	identifier = r.identifierField(x.Identifier)
	if !absent(x.Reference) {
		if ref, ok = r.str(x.Reference); !ok {
			return "", IdentifierField{}, false
		}
	}
	return ref, identifier, true
}

// referenceIdentifier reads raw, a Reference a contained resource carries, as
// its identifier.
func (r *reader) referenceIdentifier(raw json.RawMessage) IdentifierField {
	if absent(raw) {
		return IdentifierField{State: Absent}
	}
	_, id, ok := r.reference(raw)
	if !ok {
		return IdentifierField{State: Unreadable}
	}
	return id
}

// readContained reads Task.contained as each resource's type and id. When it
// cannot be read, it reads as none: then no "#id" reference resolves, so the
// field that names one is unreadable, never absent.
func (r *reader) readContained(raw json.RawMessage) []containedResource {
	if absent(raw) {
		return nil
	}
	items, ok := r.list(raw)
	if !ok {
		return nil
	}
	out := make([]containedResource, 0, len(items))
	bad := false
	for _, item := range items {
		var x struct {
			ResourceType json.RawMessage `json:"resourceType"`
			ID           json.RawMessage `json:"id"`
		}
		if r.decode(item, &x) != nil {
			bad = true
			continue
		}
		rt, ok1 := r.str(x.ResourceType)
		id, ok2 := r.str(x.ID)
		if !ok1 || !ok2 {
			bad = true
			continue
		}
		out = append(out, containedResource{resourceType: rt, id: id, raw: item})
	}
	if bad {
		return nil
	}
	return out
}

// local resolves ref, a local reference "#id", to the one contained resource
// of resourceType with that id. ok is false when ref is not local, the
// contained resources cannot be read, or the id names no resource, several,
// or one of another type.
func (r *reader) local(ref, resourceType string) (json.RawMessage, bool) {
	id, isLocal := strings.CutPrefix(ref, "#")
	if !isLocal || id == "" {
		return nil, false
	}
	var found []containedResource
	for _, c := range r.contained {
		if c.id == id {
			found = append(found, c)
		}
	}
	if len(found) != 1 || found[0].resourceType != resourceType {
		return nil, false
	}
	return found[0].raw, true
}

func (r *reader) patient(raw json.RawMessage) Patient {
	if absent(raw) {
		return Patient{State: Absent}
	}
	ref, identifier, ok := r.reference(raw)
	if !ok {
		return Patient{State: Unreadable}
	}
	if !strings.HasPrefix(ref, "#") {
		return Patient{State: Present, Reference: ref, Identifier: identifier}
	}
	res, ok := r.local(ref, "Patient")
	if !ok {
		return Patient{State: Unreadable}
	}
	var p struct {
		Identifier json.RawMessage `json:"identifier"`
	}
	if r.decode(res, &p) != nil {
		return Patient{State: Unreadable}
	}
	var ids []Identifier
	if !absent(p.Identifier) {
		items, ok := r.list(p.Identifier)
		if !ok {
			return Patient{State: Unreadable}
		}
		bad := false
		for _, item := range items {
			id, ok := r.identifier(item)
			if !ok {
				bad = true
				continue
			}
			ids = append(ids, id)
		}
		if bad {
			return Patient{State: Unreadable}
		}
	}
	return Patient{State: Present, Reference: ref, Identifier: identifier, Contained: true, Resource: append([]byte(nil), res...), Identifiers: ids}
}

func (r *reader) owner(raw json.RawMessage) Owner {
	if absent(raw) {
		return Owner{State: Absent}
	}
	ref, identifier, ok := r.reference(raw)
	if !ok {
		return Owner{State: Unreadable}
	}
	out := Owner{State: Present, Reference: ref, Identifier: identifier}
	if !strings.HasPrefix(ref, "#") {
		return out
	}
	unreadable := IdentifierField{State: Unreadable}
	if org, ok := r.local(ref, "Organization"); ok {
		out.ContainedOrganization = r.containedOrganization(org)
		return out
	}
	role, ok := r.local(ref, "PractitionerRole")
	if !ok {
		out.Organization, out.Practitioner = unreadable, unreadable
		return out
	}
	var pr struct {
		Organization json.RawMessage `json:"organization"`
		Practitioner json.RawMessage `json:"practitioner"`
	}
	if r.decode(role, &pr) != nil {
		out.Organization, out.Practitioner = unreadable, unreadable
		return out
	}
	out.Organization = r.referenceIdentifier(pr.Organization)
	out.Practitioner = r.referenceIdentifier(pr.Practitioner)
	// The role's organization can itself be contained, named by a local
	// reference: it is read as an Organization the owner names is.
	if !absent(pr.Organization) {
		if orgRef, _, ok := r.reference(pr.Organization); ok && strings.HasPrefix(orgRef, "#") {
			out.ContainedOrganization = ContainedOrganization{State: Unreadable}
			if org, ok := r.local(orgRef, "Organization"); ok {
				out.ContainedOrganization = r.containedOrganization(org)
			}
		}
	}
	return out
}

// containedOrganization reads a contained Organization's identifiers, as a
// contained Patient's are read: every identifier readable, or the whole
// Unreadable.
func (r *reader) containedOrganization(res json.RawMessage) ContainedOrganization {
	var o struct {
		Identifier json.RawMessage `json:"identifier"`
	}
	if r.decode(res, &o) != nil {
		return ContainedOrganization{State: Unreadable}
	}
	if absent(o.Identifier) {
		return ContainedOrganization{State: Present}
	}
	items, ok := r.list(o.Identifier)
	if !ok {
		return ContainedOrganization{State: Unreadable}
	}
	// Every identifier is read before the field is decided, so one that
	// names a member in another case is found whatever its siblings hold.
	var ids []Identifier
	bad := false
	for _, item := range items {
		id, ok := r.identifier(item)
		if !ok {
			bad = true
			continue
		}
		ids = append(ids, id)
	}
	if bad {
		return ContainedOrganization{State: Unreadable}
	}
	return ContainedOrganization{State: Present, Identifiers: ids}
}

func (r *reader) requester(raw json.RawMessage) Requester {
	if absent(raw) {
		return Requester{State: Absent}
	}
	ref, identifier, ok := r.reference(raw)
	if !ok {
		return Requester{State: Unreadable}
	}
	return Requester{State: Present, Reference: ref, Identifier: identifier}
}

// valueMember reports whether name is a value[x] member: "value" and a type
// name, which begins with an upper-case letter.
func valueMember(name string) bool {
	rest, ok := strings.CutPrefix(name, "value")
	return ok && rest != "" && unicode.IsUpper(rune(rest[0]))
}

func (r *reader) payerURL(raw json.RawMessage) PayerURL {
	if absent(raw) {
		return PayerURL{State: Absent, Index: -1}
	}
	unreadable := PayerURL{State: Unreadable, Index: -1}
	inputs, ok := r.list(raw)
	if !ok {
		return unreadable
	}
	// Every input is decoded before the field is decided, so an input that
	// names its value in another case is found whatever its siblings hold.
	bad := false
	var matches []int
	members := make([]map[string]json.RawMessage, len(inputs))
	for i, in := range inputs {
		if r.decode(in, &members[i]) != nil {
			bad = true
			continue
		}
		for name := range members[i] {
			if name != "valueUrl" && strings.EqualFold(name, "valueUrl") && r.twoWays == nil {
				// The member the reader reads, named in another case: the
				// input reads two ways.
				r.twoWays = fmt.Errorf("an input names %q: %w", name, relay.ErrFieldNameCase)
			}
		}
		var x struct {
			Type json.RawMessage `json:"type"`
		}
		if r.decode(in, &x) != nil {
			bad = true
			continue
		}
		// An input whose type is missing, null or cannot be read could be
		// the payer-url: codings refuses each, a missing one as no JSON.
		codings, ok := r.codings(x.Type)
		if !ok {
			bad = true
			continue
		}
		for _, c := range codings {
			if c.System == PASTempCodes && c.Code == InputPayerURL {
				matches = append(matches, i)
				break
			}
		}
	}
	switch {
	case bad, len(matches) > 1:
		return unreadable
	case len(matches) == 0:
		return PayerURL{State: Absent, Index: -1}
	}
	index := matches[0]
	form := ""
	for name := range members[index] {
		if valueMember(name) {
			if form != "" {
				// Two value members: not one value[x].
				return PayerURL{State: Unreadable, Index: index}
			}
			form = name
		}
	}
	// Only a non-empty valueUrl is the address; any other form is unreadable.
	v, ok := r.str(members[index]["valueUrl"])
	if !ok {
		return PayerURL{State: Unreadable, Index: index, Form: form}
	}
	return PayerURL{State: Present, Value: v, Index: index, Form: form}
}
