// Package paedits holds the Prior Authorization workstream's registered relay
// edits (E-01 … E-14): the only changes a gateway may make to a participant's
// message it relays on a PA leg, and the transform it makes to a PAS message
// it built itself. The gateway gathers them into its registry through the PA
// workstream; a reader of recorded exchanges names an edit by them
// (relay.EditName) without importing the gateway engine.
package paedits

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// Rows returns the PA workstream's registered edits, in id order: every edit
// the network registers is on a PA leg today. Each call returns a new slice.
func Rows() []relay.Edit {
	return []relay.Edit{
		{
			ID:        relay.EditCDSCallbackStrip,
			Name:      "cds-callback-strip",
			Legs:      []string{"crd-order-dispatch", "crd-order-select"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionRequest,
			Paths:     []string{"$.fhirServer", "$.fhirAuthorization"},
			Kind:      relay.EditKindRemoveMember,
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
			Kind:      relay.EditKindInsertMember,
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
			Kind: relay.EditKindReplaceValue,
			Precondition: "Only when the payer gateway's identity mapping is configured (it is off by default), " +
				"and only string tokens that differ from the mapped identity. Every payor of a Coverage counts " +
				"(Coverage.payor carries no ordering meaning): each payor element naming one of the payer's own " +
				"identities is mapped, and no other; a Coverage none of whose payors names one is refused as " +
				"another payer's, never skipped (judged by payor[0], as is a reference that resolves to no " +
				"resource or to several); Coverages naming different payers are refused. The referenced Organization, found in the Coverage's contained resources, the Bundle, or " +
				"the sibling resources of the Parameters or prefetch, is named by the first of its identifiers, in " +
				"document order, that is one of the payer's own network identities, wherever it sits among them " +
				"(only that identifier is mapped); when none is, by its first identifier with a non-empty system " +
				"and value, which is refused as another payer's. A claim's insurer is resolved the same way (an unresolved or " +
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
			Kind:      relay.EditKindArrayAppend,
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
			Kind:      relay.EditKindArrayAppend,
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
			Legs:      []string{"pas-claim", "pas-claim-update"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionRequest,
			Paths: []string{
				"DiagnosticReport.subject.reference",
				"Condition.subject.reference",
				"Observation.subject.reference",
				"ClinicalImpression.subject.reference",
				"Goal.subject.reference",
			},
			Kind: relay.EditKindReplaceValue,
			Precondition: "Only on a prior-authorization request the provider's gateway builds for its own workflow, to a " +
				"clinical record it reads from the provider's own system for that request: the supplemental report a claim " +
				"update attaches, and, on the provider-data origination profile, each Condition, Observation, " +
				"ClinicalImpression, Goal or DiagnosticReport the submission or update references. Only when that record's " +
				"own subject.reference is exactly the Patient that system holds the member under, and that Patient's id is " +
				"not the member id. A record with no subject.reference, or one naming any other subject, is refused, and so " +
				"is one a signature covers. A record contained in another is not edited. A request the provider's own " +
				"client sends is never edited this way.",
			Authority: "The provider's own system: the Patient it holds the member under. The value written is the " +
				"member's network patient reference, Patient/<member id>.",
			Disclosure: "When a provider's gateway carries a clinical record from the provider's own system in a " +
				"prior-authorization request it builds (a claim update's supplemental report, or the evidence a " +
				"provider-data request references), and that system names the patient by its own id, the gateway changes " +
				"only the record's subject.reference, to the member's network patient, and only when the record names that " +
				"patient. The record is otherwise read exactly as the system holds it. In a claim update the supplemental " +
				"report is then given its bundle-local id and no declared profile by the update bundle the gateway builds; " +
				"provider-data evidence keeps the id and profile the system holds it under.",
		},
		{
			ID:        relay.EditCDSCoverageCarry,
			Name:      "cds-callback-coverage-carry",
			Legs:      []string{"crd-order-dispatch", "crd-order-select"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionRequest,
			Paths:     []string{"$.prefetch", "$.prefetch.coverage"},
			Kind:      relay.EditKindInsertMember,
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
		{
			ID:        relay.EditDTRAnswerLineNames,
			Name:      "dtr-answer-line-names",
			Legs:      []string{"dtr-questionnaire-fetch"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionResponse,
			Paths:     []string{"$.parameter[<i>].name", "$.parameter[<i>].resource.entry[<j>].resource (a QuestionnaireResponse)"},
			Kind:      relay.EditKindReplaceValue,
			Precondition: "Always (not an enrichment opt-in), and only when the requesting gateway declares exactly one DTR " +
				"line, every line-specific parameter name in the payer's answer belongs to the other DTR name family " +
				"(PackageBundle and operationOutcome: DTR 2.0.1 and 2.1.0; packagebundle: DTR 2.2.0), and the answer " +
				"validates at its own line. The answer must then validate at the requester's line, or it is refused at " +
				"every level, as it is when a registered pa.dtr step refuses. outcome, every line's OperationDefinition " +
				"name, and return are never renamed.",
			Authority: "The DTR IG's own output Parameters profiles: each name is the one the requester's line gives the " +
				"same parameter. Each package QuestionnaireResponse is the output of the registered pa.dtr compatibility " +
				"steps (the compat manifest's chain between the two lines) for the payer's own response, each step " +
				"reported under its own module with its loss report. Nothing is read from any participant.",
			Disclosure: "When a payer's questionnaire package answer names its package or outcome parameter as another DTR " +
				"line does than the one the provider's gateway declares, the gateway renames those parameters to the " +
				"provider's line's names (PackageBundle and packagebundle, operationOutcome and outcome), and carries each " +
				"QuestionnaireResponse in the package across the lines through the network's registered DTR steps (the " +
				"Coverage context's place and the intended-use code system); every other byte is the payer's. An answer " +
				"that cannot be valid at the provider's line is refused, never filled. The payer's ETag is left out of the " +
				"answer so changed, since it named the payer's bytes.",
			DropsAnswerHeaders: []string{relay.ETagHeader},
		},
		{
			ID:        relay.EditRegisteredTransform,
			Name:      "registered-transform",
			Legs:      []string{"pas-claim", "pas-claim-update"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionRequest,
			Paths:     []string{"$ (the whole Bundle)"},
			Kind:      relay.EditKindRegisteredTransform,
			Precondition: "Only on a prior-authorization submission or update the gateway built itself, on behalf of a " +
				"participant whose system does not build it, at one of the gateway's own lines, when the payer declares " +
				"only another line, this gateway translates between IG lines, and the compat manifest's registered pa.pas " +
				"steps chain the two. The steps are run again on the message as built and must make the same bytes twice; " +
				"the result must then validate at the payer's line, at every level, or nothing is sent.",
			Authority: "The registered pa.pas compatibility steps between the two lines, each reported under its own module " +
				"with its loss report. The message is the builder's, from the participant's own records; nothing is read from " +
				"any other participant.",
			Disclosure: "When the provider's gateway builds a prior-authorization submission or update for its participant " +
				"and the payer declares another PAS line, the gateway may carry it to the payer's line through the network's " +
				"registered PAS steps. The message stays the builder's, recorded as built by that builder and then edited by " +
				"this transform; it must validate at the payer's line, or it is refused, never sent.",
		},
		{
			ID:        relay.EditPASPendReturnRekey,
			Name:      "pas-pend-return-rekey",
			Legs:      []string{"pas-claim", "pas-claim-inquire", "pas-claim-update"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionResponse,
			Paths:     []string{"Task.input[i].valueUrl (the input whose type is PASTempCodes#payer-url)"},
			Kind:      relay.EditKindReplaceValue,
			Precondition: "Always (not an opt-in), on a payer's answer the provider's gateway relays to its own system, for each " +
				"Task in it with a payer-url input written as a valueUrl, decided by structure (the input's type coding, " +
				"never text). Made even where an in-payload signature covers the payer-url; that signature then no longer " +
				"verifies in the provider's copy, and the forward says so. A payer-url in any other form is carried as sent " +
				"and the exchange records that it was not re-keyed.",
			Authority: "The provider gateway's own network return path for the payer the answer was routed to and came from: " +
				"a fixed address under its Da Vinci ingress base, rebuilt from that payer's network identity. Nothing is " +
				"read from the answer and nothing is stored per request.",
			Disclosure: "When a payer's prior-authorization answer carries a Task whose payer-url names where to send " +
				"documentation, the provider's gateway replaces that address with its own return path for the same payer, " +
				"so the documentation the provider sends there reaches that payer through the network. The forward to the " +
				"provider's system names the edit in a header, and names any signature that covered the address. The payer's " +
				"ETag is left out of that forward, since it named the payer's bytes.",
			DropsAnswerHeaders: []string{relay.ETagHeader},
		},
		{
			ID:        relay.EditCDexTaskReturnRekey,
			Name:      "cdex-task-return-rekey",
			Legs:      []string{"cdex-attachment-request"},
			Role:      relay.RoleRecipient,
			Direction: relay.DirectionRequest,
			Paths:     []string{"Task.input[i].valueUrl (the one input whose type is PASTempCodes#payer-url)"},
			Kind:      relay.EditKindReplaceValue,
			Precondition: "Always (not an opt-in), on a payer's attachment-request Task the provider's gateway carries to its " +
				"own system, when the Task has exactly one payer-url input and it is written as a valueUrl, decided by " +
				"structure (the input's type coding, never text), and the payer that sent it takes the documentation " +
				"submission. Made even where an in-payload signature covers the payer-url; that signature then no longer " +
				"verifies in the provider's copy, and the forward says so. A payer-url in any other form, several payer-url " +
				"inputs, or a payer that does not take the submission, is carried as sent and the exchange records that it " +
				"was not re-keyed.",
			Authority: "The provider gateway's own network return path for the authenticated payer that sent the Task: a " +
				"fixed address under its Da Vinci ingress base, rebuilt from that payer's network identity. Nothing is " +
				"read from the Task and nothing is stored per request.",
			Disclosure: "When a payer's attachment-request Task names, in its payer-url, where to send the documentation, the " +
				"provider's gateway replaces that address with its own return path for the same payer, so the " +
				"documentation the provider sends there reaches that payer through the network. The forward to the " +
				"provider's system names the edit in a header, and names any signature that covered the address.",
		},
		{
			ID:        relay.EditDTRAdaptiveAddressRekey,
			Name:      "dtr-adaptive-address-rekey",
			Legs:      []string{"dtr-questionnaire-fetch"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionResponse,
			Paths: []string{"Questionnaire.extension[i].valueUrl (the extension whose url is sdc-questionnaire-questionnaireAdaptive), " +
				"or its valueBoolean true, on any Questionnaire in the answer"},
			Kind: relay.EditKindReplaceValue,
			Precondition: "Always (not an opt-in), on a questionnaire-fetch answer (a package or a next-question answer) the " +
				"provider's gateway relays to its own DTR client, toward a payer whose gateway restores the address, for " +
				"each questionnaireAdaptive address the payer's gateway stated and tagged in the sealed answer, decided by " +
				"structure. Only the payer's base is replaced; the rest of the address is kept byte for byte. A true is made " +
				"explicit as the gateway's address for the payer's own DTR endpoint. Made even where an in-payload signature " +
				"covers the address; that signature then no longer verifies in the client's copy, and the answer says so. " +
				"Any address it cannot re-key is carried as sent and the exchange records why.",
			Authority: "The provider gateway's own per-payer DTR address, under its Da Vinci ingress base, carrying the address " +
				"tag the payer's gateway issued for that address and this provider. Nothing is stored per request.",
			Disclosure: "When a payer's questionnaire names the server for its adaptive next-question calls, the provider's " +
				"gateway replaces the payer's base in that address with its own address for the same payer, so the DTR " +
				"client's next-question calls reach that payer through the network. A questionnaire that marks itself " +
				"adaptive with true gets that address written out. The answer to the client names the edit in a header, " +
				"and names any signature that covered the address. The payer's ETag is left out, since it named the " +
				"payer's bytes.",
			DropsAnswerHeaders: []string{relay.ETagHeader},
		},
		{
			ID:   relay.EditDTRAdaptiveAddressRestore,
			Name: "dtr-adaptive-address-restore",
			Legs: []string{"dtr-questionnaire-fetch", "cdex-attachment-submit", "pas-claim", "pas-claim-update",
				"pas-claim-inquire"},
			Role:      relay.RoleRecipient,
			Direction: relay.DirectionRequest,
			Paths: []string{"Questionnaire.extension[i].valueUrl (the extension whose url is sdc-questionnaire-questionnaireAdaptive), " +
				"or its valueBoolean true written back, on any Questionnaire the request carries"},
			Kind: relay.EditKindReplaceValue,
			Precondition: "Always (not an opt-in), on a request the payer's gateway forwards to its own system, for each " +
				"questionnaireAdaptive address carrying an address tag this payer's gateway issued for the provider that " +
				"sent the request, and only when that tag verifies. Made even where an in-payload signature covers the " +
				"address; that signature then no longer verifies in the payer's copy, and the forward says so. Any other " +
				"address is carried as sent, and one this gateway recognises but cannot restore is recorded.",
			Authority: "The address the payer itself issued: its base at the address's line, as its gateway states it, and " +
				"the rest of the address kept byte for byte, or the true it wrote. Nothing is stored per request.",
			Disclosure: "When a request carries back a questionnaire address the provider's gateway re-keyed, the payer's " +
				"gateway restores the address the payer wrote, or the true it wrote, before forwarding the request to the " +
				"payer's system, so the payer receives its own address. The forward names the edit in a header, and names any signature that " +
				"covered the address.",
		},
		{
			ID:        relay.EditFacilityReportSubjectRekey,
			Name:      "facility-report-subject-rekey",
			Legs:      []string{"pas-claim-update"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionRequest,
			Paths:     []string{"DiagnosticReport.subject.reference"},
			Kind:      relay.EditKindReplaceValue,
			Precondition: "Only on a prior-authorization update the provider's gateway builds for its own workflow, to the " +
				"facility's report it read on the federated-query leg and attaches as the update's evidence, and only once " +
				"that report, and the Provenance that attributes it, are shown to be about the Claim's patient: the report's " +
				"subject names the member directly, or through a Patient the facility carried with the member identifier. " +
				"A report about anyone else is refused, with no edit made. A report whose subject.reference is exactly the " +
				"Claim's patient reference, or whose subject names the member by identifier alone, is carried as sent. A report a " +
				"signature covers, in the report or in the facility's Provenance that attributes it, is refused. A request " +
				"the provider's own client sends is never edited this way.",
			Authority: "The facility's own answer, read on the federated-query leg, and the Claim the gateway builds: the " +
				"value written is the Claim's patient reference.",
			Disclosure: "When a provider's gateway attaches a facility's report to a prior-authorization update it builds, " +
				"and that report's subject.reference is not exactly the Claim's patient reference (it names the facility's " +
				"own Patient, or the member in another form), the gateway changes only that reference, to the Claim's " +
				"patient, after checking the report is about that patient. Every other " +
				"byte of the report is the facility's, the display, identifier and type of its subject included.",
		},
	}
}
