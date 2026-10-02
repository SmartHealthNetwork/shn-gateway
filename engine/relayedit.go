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
		Precondition: "Always, when the member is present. A payer never receives a route or a credential " +
			"into the provider's system.",
		Authority: "None: the members are removed, nothing is written.",
		Disclosure: "The provider's gateway removes fhirServer and fhirAuthorization from every CDS Hooks request " +
			"before it leaves the provider; the payer answers from the request and its prefetch.",
	},
	{
		ID:        relay.EditCDSPrefetchObtain,
		Name:      "cds-prefetch-obtain",
		Legs:      []string{"crd-order-dispatch", "crd-order-select"},
		Role:      relay.RoleRequester,
		Direction: relay.DirectionRequest,
		Paths:     []string{"$.prefetch", "$.prefetch.<key>"},
		Kind:      editInsertMember,
		Precondition: "Only when the provider opts in to enrichment (ENRICH_NATIVE_REQUESTS; not applied by default), " +
			"only for a prefetch key the payer's service advertises and the request left out, " +
			"and only when the provider's system names the patient by the request's own id; " +
			"$.prefetch itself is created when the request has none. A key the request carries is never changed.",
		Authority: "The provider's own system, read over the gateway's authenticated connection. The patient " +
			"is that system's bytes; a search is a searchset the gateway writes around that system's records " +
			"(each matching or included record byte for byte, under a urn:uuid entry address the gateway " +
			"assigns, with no links or addresses of that system); null when it holds nothing.",
		Disclosure: "When the provider has opted in to enrichment and a CDS Hooks request leaves out a prefetch value the " +
			"payer's service asks for, the provider's gateway adds it from the provider's own system: the records exactly as that system holds them, in a " +
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
		Precondition: "Only when the provider opts in to enrichment (ENRICH_NATIVE_REQUESTS; not applied by default), " +
			`and only when the request carries no "coverage" parameter and the provider's system names the ` +
			`patient by the request's own member id; one {"name":"coverage","resource":…} ` +
			"element is appended. Without the opt-in the Coverage is still read to choose the payer, and not appended.",
		Authority: "The provider's own system: its Coverage for the bound patient.",
		Disclosure: "When the provider has opted in to enrichment and a questionnaire package request carries no coverage, " +
			"the provider's gateway appends the patient's active Coverage from the provider's own system, when that system names " +
			"the patient by the request's member id and holds one; the rest of the " +
			"request is sent unchanged.",
	},
	{
		ID:        relay.EditDTRPatientObtain,
		Name:      "dtr-patient-obtain",
		Legs:      []string{"dtr-questionnaire-fetch"},
		Role:      relay.RoleRequester,
		Direction: relay.DirectionRequest,
		Paths:     []string{"$.parameter"},
		Kind:      editArrayAppend,
		Precondition: "Only when the provider opts in to enrichment (ENRICH_NATIVE_REQUESTS; not applied by default), " +
			"only when the request carries no Patient resource for the bound " +
			"patient anywhere in its parameters, and only when the provider's own system holds the patient " +
			"under the id the request names; one {\"name\":\"referenced\",\"resource\":<Patient>} element is appended.",
		Authority: "The provider's own system: its Patient record for the bound patient.",
		Disclosure: "When the provider has opted in to enrichment and a questionnaire package request carries no Patient, " +
			"the provider's gateway appends the patient's own Patient record from the provider's system as a referenced " +
			"resource, so a payer that does not hold the member can identify the patient the request names; the rest of the request is sent unchanged.",
	},
	{
		ID:        relay.EditEvidenceSubjectRekey,
		Name:      "evidence-subject-rekey",
		Legs:      []string{"pas-claim-update"},
		Role:      relay.RoleRequester,
		Direction: relay.DirectionRequest,
		Paths:     []string{"DiagnosticReport.subject.reference"},
		Kind:      editReplaceValue,
		Precondition: "Only on a claim update the provider's gateway builds for its own workflow, to the supplemental " +
			"report it reads from the provider's own system, and only when that report's subject.reference names the " +
			"Patient that system holds the member under and that Patient's id is not the member id. A report with no " +
			"subject.reference, or one naming any other subject, is refused. A request the provider's own client sends " +
			"is never edited this way.",
		Authority: "The provider's own system: the Patient it holds the member under. The value written is the " +
			"member's network patient reference, Patient/<member id>.",
		Disclosure: "When a provider's gateway attaches a supplemental report from the provider's own system to a claim " +
			"update it builds, and that system names the patient by its own id, the gateway changes only the report's " +
			"subject.reference, to the member's network patient, and only when the report names that patient. The " +
			"report is otherwise read exactly as the system holds it; the update bundle the gateway builds gives it its " +
			"bundle-local id and no declared profile.",
	},
	{
		ID:        relay.EditCDSCoverageCarry,
		Name:      "cds-callback-coverage-carry",
		Legs:      []string{"crd-order-dispatch", "crd-order-select"},
		Role:      relay.RoleRequester,
		Direction: relay.DirectionRequest,
		Paths:     []string{"$.prefetch", "$.prefetch.coverage"},
		Kind:      editInsertMember,
		Precondition: "Always (not an enrichment opt-in), and only when the callback strip (E-01) removed a fhirServer " +
			"this gateway read to route by: the request carries no prefetch.coverage (a key the request carries, even " +
			"null, is never changed or replaced), the provider's system of record names no patient for the member, the " +
			"fhirServer read is on (CDS_FHIR_SERVER_READ is not off) and it succeeded, and the request is routed. " +
			"$.prefetch itself is created when the request has none.",
		Authority: "The provider's own EHR server: the request's fhirServer, read with the request's own " +
			"fhirAuthorization. The value is a searchset the gateway writes around the records that read returned and " +
			"routing used: each chosen Coverage, and the payor Organization routing resolved for it, byte for byte, so " +
			"any reference a record holds (an absolute one on that server included) is carried as written. Nothing of " +
			"that server's answer outside those records is carried: no link, entry address, Bundle id or meta. Each " +
			"entry address is a urn:uuid the gateway assigns, except an included Organization a chosen Coverage names " +
			"by an absolute reference on the request's own fhirServer base, whose entry address is that reference, as " +
			"the Coverage writes it.",
		Disclosure: "When the provider's gateway removes fhirServer and fhirAuthorization from a CDS Hooks request that " +
			"carries no coverage, and routes it by the Coverage it read through that fhirServer, it adds that coverage " +
			"as prefetch.coverage: the EHR server's records exactly as it returned them (any reference they hold carried " +
			"as written), in a searchset the gateway writes with none of that server's links or entry addresses; an " +
			"included Organization a Coverage names by an absolute reference on that server has that reference as its " +
			"entry address.",
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

// RelayEditName returns the registered name of the edit id names (E-01 is
// "cds-callback-strip"), and whether the registry holds it. Staff views show
// an edit by its id and this name only.
func RelayEditName(id relay.EditID) (string, bool) {
	e, ok := relayEditByID(id)
	return e.Name, ok
}

// RelayEditReceivedBy names the party of a leg whose received bytes the edit
// id names changes, and whether the registry holds it. An edit a gateway
// makes to what it sends into the network reaches the leg's other party: a
// requester's request edit (E-01, E-02, E-04 … E-07) is received by the
// recipient. An edit a gateway makes to what it sends its own participant's
// system (the payer identity mapping, E-03, on the recipient's forward) is
// received by no other party: the role returned is 0.
func RelayEditReceivedBy(id relay.EditID) (relay.Role, bool) {
	e, ok := relayEditByID(id)
	if !ok {
		return 0, false
	}
	switch {
	case e.Role == relay.RoleRequester && e.Direction == relay.DirectionRequest:
		return relay.RoleRecipient, true
	case e.Role == relay.RoleRecipient && e.Direction == relay.DirectionResponse:
		return relay.RoleRequester, true
	}
	return 0, true
}
