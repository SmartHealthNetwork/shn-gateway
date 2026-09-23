package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// relayEditKind is the one kind of change a registered edit makes.
type relayEditKind string

const (
	editRemoveMember relayEditKind = "remove-member"
	editInsertMember relayEditKind = "insert-member"
	editReplaceValue relayEditKind = "replace-value"
	editArrayAppend  relayEditKind = "array-append"
)

// relayEdit declares one registered edit: the only changes a gateway may
// make to a participant's message it relays. Anything not declared here is
// not an edit and is never made to a relayed message.
type relayEdit struct {
	ID   relay.EditID
	Name string
	// Legs, Role and Direction name the transmit the edit applies at.
	Legs      []string
	Role      relay.Role
	Direction relay.Direction
	// Paths are the exact JSON locations the edit may change.
	Paths []string
	Kind  relayEditKind
	// Precondition says when the edit is made; Authority says where the
	// value it writes comes from.
	Precondition string
	Authority    string
	// Disclosure is the statement participants are given about the edit.
	Disclosure string
}

// relayEdits is the closed edit registry, in id order.
var relayEdits = []relayEdit{
	{
		ID:        relay.EditCDSCallbackStrip,
		Name:      "cds-callback-strip",
		Legs:      []string{"crd-order-dispatch", "crd-order-select"},
		Role:      relay.RoleRequester,
		Direction: relay.DirectionRequest,
		Paths:     []string{"$.fhirServer", "$.fhirAuthorization"},
		Kind:      editRemoveMember,
		Precondition: "At the source boundary unless authenticated, byte-bound connector completion is verified; " +
			"when present, the member is removed. A payer never receives a route or a credential into the provider's system.",
		Authority: "None: the members are removed, nothing is written.",
		Disclosure: "Before a CDS Hooks request leaves the provider, its gateway removes fhirServer and fhirAuthorization " +
			"or verifies the source connector's byte-bound declaration that preparation is complete; the payer answers from the request and its prefetch.",
	},
	{
		ID:        relay.EditCDSPrefetchObtain,
		Name:      "cds-prefetch-obtain",
		Legs:      []string{"crd-order-dispatch", "crd-order-select"},
		Role:      relay.RoleRequester,
		Direction: relay.DirectionRequest,
		Paths:     []string{"$.prefetch", "$.prefetch.<key>"},
		Kind:      editInsertMember,
		Precondition: "Only for explicit source-data assembly, for a prefetch key the payer's service advertises and the request left out; " +
			"$.prefetch itself is created when the request has none. A key the request carries is never changed.",
		Authority: "The provider's own system, read over the gateway's authenticated connection. The patient " +
			"is that system's bytes; a search is a searchset the gateway writes around that system's records " +
			"(each matching or included record byte for byte, under a urn:uuid entry address the gateway " +
			"assigns, with no links or addresses of that system); null when it holds nothing.",
		Disclosure: "When source-data assembly is requested for an absent prefetch value the payer's service asks for, the provider's " +
			"gateway adds it from the provider's own system: the records exactly as that system holds them, in a " +
			"searchset the gateway writes, never an address in that system; values the request carries are sent unchanged.",
	},
	{
		ID:   relay.EditPayorEdgeRestamp,
		Name: "payor-edge-restamp",
		Legs: []string{"crd-order-dispatch", "crd-order-select", "dtr-questionnaire-fetch",
			"pas-claim", "pas-claim-inquire", "pas-claim-update"},
		Role:      relay.RoleRecipient,
		Direction: relay.DirectionRequest,
		Paths: []string{
			"Coverage.payor[i].identifier.system",
			"Coverage.payor[i].identifier.value",
			"Organization.identifier[j].system",
			"Organization.identifier[j].value",
			"Claim.insurer (resolved the same way as Coverage.payor)",
		},
		Kind: editReplaceValue,
		Precondition: "Only when the payer gateway's identity mapping is configured (it is off by default), " +
			"and only string tokens that differ from the mapped identity. Every Coverage is mapped by its " +
			"routing payor (payor[0]): one naming a different payer is refused, never skipped; Coverages " +
			"naming different payers are refused; a reference that resolves to no resource or to several is " +
			"refused. The referenced Organization's identity is its first identifier with a non-empty system " +
			"and value, found in the Coverage's contained resources, the Bundle, or the sibling resources of " +
			"the Parameters or prefetch. A claim's insurer is resolved the same way (an unresolved or " +
			"ambiguous reference is refused) and is mapped only when it names this payer; an insurer naming " +
			"another identity is left as sent.",
		Authority: "The payer gateway's configured mapping from its network identity to the identity " +
			"its payer's system expects.",
		Disclosure: "When a payer maps its network identity to the identity its own system expects, the payer's " +
			"gateway replaces the matching payer identifier values in each Coverage (and a claim's insurer) " +
			"and changes nothing else.",
	},
	{
		ID:        relay.EditDTRCoverageObtain,
		Name:      "dtr-coverage-obtain",
		Legs:      []string{"dtr-questionnaire-fetch"},
		Role:      relay.RoleRequester,
		Direction: relay.DirectionRequest,
		Paths:     []string{"$.parameter"},
		Kind:      editArrayAppend,
		Precondition: `Only for source assembly when routing needs Coverage and the request carries no "coverage" parameter; one {"name":"coverage","resource":…} ` +
			"element is appended.",
		Authority: "The provider's own system: its Coverage for the bound patient.",
		Disclosure: "When routing needs source Coverage for a questionnaire package request, the provider's gateway appends the " +
			"patient's Coverage from the provider's own system; the rest of the request is sent unchanged.",
	},
	{
		ID:        relay.EditDTRPatientObtain,
		Name:      "dtr-patient-obtain",
		Legs:      []string{"dtr-questionnaire-fetch"},
		Role:      relay.RoleRequester,
		Direction: relay.DirectionRequest,
		Paths:     []string{"$.parameter"},
		Kind:      editArrayAppend,
		Precondition: "Only for explicit source assembly, when the request carries no Patient resource for the bound " +
			"patient anywhere in its parameters, and only when the provider's own system holds the patient " +
			"under the id the request names; one {\"name\":\"referenced\",\"resource\":<Patient>} element is appended.",
		Authority: "The provider's own system: its Patient record for the bound patient.",
		Disclosure: "When source Patient assembly is explicitly requested for a questionnaire package request carrying no Patient, " +
			"the provider's gateway appends the patient's own Patient record from " +
			"the provider's system as a referenced resource; the rest of the request is sent unchanged.",
	},
}

// relayEditByID returns the registered edit with the given id.
func relayEditByID(id relay.EditID) (relayEdit, bool) {
	for _, e := range relayEdits {
		if e.ID == id {
			return e, true
		}
	}
	return relayEdit{}, false
}
