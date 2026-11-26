// Package paedits holds the Prior Authorization workstream's registered relay
// edits (E-01 … E-14, E-16 … E-21 and E-23, apart from the retired E-03;
// E-15 is not used): the only changes a gateway may make to a participant's message it
// relays on a PA leg, and the transform it makes to a PAS message it built
// itself. The gateway gathers them into its registry through the PA
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
				"ServiceRequest.subject.reference",
				"DeviceRequest.subject.reference",
				"MedicationRequest.subject.reference",
				"NutritionOrder.patient.reference",
				"Encounter.subject.reference",
			},
			Kind: relay.EditKindReplaceValue,
			Precondition: "Only on a prior-authorization request the provider's gateway builds for its own workflow, to a " +
				"clinical record it reads from the provider's own system for that request: the supplemental report a claim " +
				"update attaches, and, on the provider-data origination profile, each Condition, Observation, " +
				"ClinicalImpression, Goal or DiagnosticReport the submission or update references, and each ServiceRequest, " +
				"DeviceRequest, MedicationRequest, NutritionOrder or Encounter the provider's questionnaire response names in " +
				"a qr-context. Only at the element each type names its patient by (a NutritionOrder's patient, every other " +
				"type's subject), and only when that element's reference is exactly the Patient that system holds the member " +
				"under, and that Patient's id is not the member id. A record with no such reference, or one naming any other " +
				"subject, is refused, and so is one a signature covers. A record contained in another is not edited. A request the provider's own " +
				"client sends is never edited this way.",
			Authority: "The provider's own system: the Patient it holds the member under. The value written is the " +
				"member's network patient reference, Patient/<member id>.",
			Disclosure: "When a provider's gateway carries a clinical record from the provider's own system in a " +
				"prior-authorization request it builds (a claim update's supplemental report, the evidence a " +
				"provider-data request references, or a record its questionnaire response names as a context), and that " +
				"system names the patient by its own id, the gateway changes only the reference naming the record's patient " +
				"(subject, or a NutritionOrder's patient), to the member's network patient, and only when the record names " +
				"that patient. The record is otherwise read exactly as the system holds it. In a claim update the supplemental " +
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
		{
			ID:           relay.EditCDexTaskLocationRekey,
			Name:         "cdex-task-location-rekey",
			Legs:         []string{"cdex-attachment-request"},
			Role:         relay.RoleRequester,
			Direction:    relay.DirectionResponse,
			Kind:         relay.EditKindSetHeader,
			Header:       "Location",
			HeaderInputs: []string{relay.TaskLocationInputBase, relay.TaskLocationInputTail},
			Precondition: "Always (not an opt-in), on a 2xx answer to a payer's attachment-request Task that the payer's " +
				"gateway relays to its own system, when the provider's gateway declares that it frames the Task's location " +
				"(the cdexloc request frame) and framed it for this answer: the tail of the Location the provider's system " +
				"answered, Task/{id} or Task/{id}/_history/{vid}, framed only when that Location lies under the provider " +
				"gateway's own Task endpoint by the segment-boundary match. A provider whose gateway does not declare it, " +
				"an answer with no tail framed, or a tail that cannot be read as a Task id with an optional version, is " +
				"carried without a Location and the exchange records why.",
			Authority: "The payer gateway's own base for the provider the Task was routed to, rebuilt from its ingress base " +
				"and that provider's network identity, then the tail the provider's gateway framed from its own system's " +
				"Location, read as a Task id and an optional version id. Nothing is stored per request.",
			Disclosure: "When a payer's system creates an attachment-request Task, the payer's gateway answers it with a " +
				"Location naming the Task at the gateway's base for the provider the Task went to, as a FHIR create's " +
				"answer names the created resource. The answer names the edit in a header. The body is the provider's, " +
				"unchanged, so its ETag is kept.",
		},
		{
			ID:        relay.EditCDexTaskNotificationAddressRekey,
			Name:      "cdex-task-notification-address-rekey",
			Legs:      []string{"cdex-task-signal"},
			Role:      relay.RoleRecipient,
			Direction: relay.DirectionRequest,
			Paths: []string{"Bundle.link[i].url", "Bundle.entry[i].fullUrl (not a urn:uuid)", "Bundle.entry[i].request.url",
				"Bundle.entry[i].response.location", "Bundle.entry[i].link[j].url",
				"Bundle.entry[0].resource.parameter[i].valueReference.reference (the parameter named subscription)",
				"Bundle.entry[0].resource.parameter[i].part[j].valueReference.reference (the part named focus of the parameter named notification-event)"},
			Kind: relay.EditKindReplaceValue,
			Precondition: "Always (not an opt-in), on a provider's CDex Task notification the payer's gateway delivers to its " +
				"own system's notification endpoint, for each absolute http or https address in those places that lies under " +
				"the base the provider's gateway stated in the sealed request by the segment-boundary match (scheme, host and " +
				"port, then path segment by segment). The value written is the payer gateway's base for the provider, then the " +
				"address's path below the stated base in its escaped form, percent-encoding kept, then its query as sent. An " +
				"address whose path below the base holds a dot segment, raw or percent-encoded, or a segment that decodes to " +
				"one holding a slash or a backslash, is not under it. Both gateways refuse a notification outside the closed SubscriptionStatus " +
				"Bundle shape (a signed one among them, so the edit is never made on signed content on this leg), and one holding an " +
				"address into the provider's own server (the stated base's scheme, host and port) that is not under the stated base. " +
				"A relative reference is carried as sent: it already " +
				"resolves against the payer gateway's base for the provider, where the payer subscribed. An address on another " +
				"server, every address of a notification whose request states no base, and every address when the " +
				"payer's gateway has no base for the provider, are carried as sent, and the exchange records why. Both gateways refuse, " +
				"on routing, a notification holding an http or https address in those places that is not an absolute URL naming " +
				"its server plainly, the same server to every URL reader, which could not be re-keyed one way only.",
			Authority: "The payer gateway's own base for the provider that sent the notification, rebuilt from its ingress base " +
				"and that provider's network identity, and the provider's FHIR base its gateway stated. Nothing is stored " +
				"per Subscription or notification.",
			Disclosure: "When a provider's system notifies a payer of a change to its CDex Tasks, the payer's gateway writes " +
				"each address into the provider's FHIR server that the notification names (the Task it is about, the " +
				"Subscription, the Bundle's links, and each entry's addresses) at the gateway's own base for that provider, where the payer's " +
				"system reads the Task and its Subscription through the network. The delivery names the edit in a header.",
		},
		{
			ID:        relay.EditSubscriptionHeaderSeal,
			Name:      "subscription-header-seal",
			Legs:      []string{"cdex-task-subscribe", "pas-subscription-subscribe"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionRequest,
			Paths:     []string{"Subscription.channel.endpoint", "Subscription.channel.header"},
			Kind:      relay.EditKindRemoveMember,
			Precondition: "Always (not an opt-in), on a Subscription create or update the subscriber's system makes at its " +
				"gateway's per-counterpart base, once the gateway has read it and checked its channel.endpoint: the whole " +
				"channel is sealed. channel.endpoint and channel.header are removed from the body the gateway sends, and " +
				"travel only sealed, in the request frame, under a key only the subscriber's gateway holds; an absent header " +
				"is sealed as an empty set. Made even where an in-payload signature covers the channel; that signature then " +
				"no longer verifies at the notifier, whose gateway says so on its forward. A Subscription the gateway cannot " +
				"read or seal is refused, never sent, and the exchange records why the edit was not made.",
			Authority: "The Subscription's own channel.endpoint and channel.header, as the subscriber's system wrote them, " +
				"sealed under the subscriber gateway's own key with the subscriber, the notifier and the topic bound to the " +
				"seal. Nothing is stored per Subscription.",
			Disclosure: "When a subscriber's system creates or updates a Subscription, its gateway removes the Subscription's " +
				"channel.endpoint and channel.header before the Subscription crosses the network, and sends both sealed so " +
				"that only that gateway can read them: neither the subscriber's endpoint nor its header values reach the " +
				"notifier's system in clear. The notifier's gateway writes its own notification address there, and its " +
				"forward names this edit.",
		},
		{
			ID:        relay.EditSubscriptionEndpointRekey,
			Name:      "subscription-endpoint-rekey",
			Legs:      []string{"cdex-task-subscribe", "pas-subscription-subscribe"},
			Role:      relay.RoleRecipient,
			Direction: relay.DirectionRequest,
			Paths:     []string{"Subscription.channel.endpoint", "Subscription.channel.header"},
			Kind:      relay.EditKindReplaceValue,
			Precondition: "Always (not an opt-in), on a payer's Subscription create or update the provider's gateway forwards " +
				"to its own system, once the Subscription is within the shape the network carries, when the payer's gateway " +
				"framed the Subscription's sealed endpoint and sealed headers. The endpoint is written, added when the body " +
				"states none (the payer's gateway sends it only sealed) and written over one it states, and the header set " +
				"replaced, or added when the Subscription has none. Made even where an in-payload signature covers the " +
				"channel; that signature then no longer verifies in the provider's copy, and the forward says so. A " +
				"Subscription the gateway cannot read, or a request without the sealed values or with values that are not " +
				"sealed values, is refused, never carried, and the exchange records why the edit was not made.",
			Authority: "The provider gateway's own notification address for the authenticated payer and the topic, under its " +
				"Da Vinci ingress base, naming the endpoint the payer's gateway sealed; a credential for that address the " +
				"provider gateway makes under its own key; and the payer's sealed headers as its gateway framed them. Nothing " +
				"is read from the Subscription and nothing is stored per Subscription.",
			Disclosure: "When a payer's system creates or updates a Subscription to the changes of its Tasks at a provider, " +
				"the provider's gateway writes its own notification address into channel.endpoint and two values into " +
				"channel.header: its credential for that address, and the payer's own header values sealed so that only the " +
				"payer's gateway can read them. The provider's system then delivers its notifications to the provider's " +
				"gateway, which carries them to the payer. The forward names the edit in a header, and names any signature " +
				"that covered the channel.",
		},
		{
			ID:        relay.EditSubscriptionEndpointRestore,
			Name:      "subscription-endpoint-restore",
			Legs:      []string{"cdex-task-subscribe", "pas-subscription-subscribe"},
			Role:      relay.RoleRequester,
			Direction: relay.DirectionResponse,
			Paths: []string{"Subscription.channel.endpoint", "Subscription.channel.header",
				"Bundle.entry[i].resource.channel.endpoint (a Subscription)", "Bundle.entry[i].resource.channel.header (a Subscription)",
				"Bundle.entry[i].fullUrl", "Bundle.link[i].url"},
			Kind: relay.EditKindReplaceValue,
			Precondition: "Always (not an opt-in), on a notifier's 2xx answer the subscriber's gateway relays to its own " +
				"system: a Subscription, or a searchset or page of them. A Subscription's channel is restored when its " +
				"channel.endpoint ends in a notification address for this subscriber and the topic, and its sealed endpoint " +
				"and sealed headers open under the subscriber gateway's key for the notifier that answered; channel.header " +
				"is then removed when the subscriber wrote none. Each fullUrl and paging link under the base the notifier's " +
				"gateway stated is re-keyed to the per-counterpart base, and a paging link's url search parameter, which the " +
				"notifier's server echoes as the notification addresses E-21 wrote, is restored to the subscriber's own " +
				"endpoint when each of those addresses leads to that one endpoint: on the answer to a search by url, an " +
				"address that ends in a notification address for this subscriber and the topic, with a key id and sealed " +
				"value that are one the subscriber's gateway sealed that search's endpoint to, leads to the endpoint the " +
				"subscriber's system searched by, whatever Subscriptions the answer holds; an address that is the " +
				"channel.endpoint of a Subscription in the same answer that this edit restored leads to that " +
				"Subscription's endpoint. Every other parameter keeps its bytes, and the core checks it. Any url " +
				"parameter of a link, with or without a modifier, that names a notification address or cannot be read, " +
				"and is not restored, is carried as written, and the exchange records it. Made even where an in-payload " +
				"signature covers what it changes; that signature then no longer verifies in the subscriber's copy, and " +
				"the answer says so. A " +
				"Subscription whose address does not end so, or whose seals do not open (another notifier's, tampered, or " +
				"made under a key past its rotation's overlap), is carried as the notifier sent it, and the exchange " +
				"records why.",
			Authority: "The sealed values the subscriber's own gateway made, opened under its own key with the subscriber, the " +
				"notifier the Hub authenticated and the topic bound to the seal; for a search's link, the endpoint the " +
				"subscriber's system searched by and the values its gateway sealed it to, read from the request that " +
				"gateway framed, never from the answer; and the per-counterpart base, rebuilt from " +
				"the gateway's ingress base and the counterpart's network identity. Nothing is stored per Subscription.",
			Disclosure: "When a notifier answers a subscriber's Subscription interaction with Subscriptions, the subscriber's " +
				"gateway puts back the channel.endpoint and channel.header the subscriber wrote in place of the notifier " +
				"gateway's notification address and credential, so the subscriber reads back its own Subscription as it " +
				"would directly, and writes the notifier's addresses in a search's entries and paging links at the base the " +
				"subscriber addresses, a paging link's url back to the subscriber's own endpoint, so following the link " +
				"repeats the subscriber's search. The answer names the edit in a header, and names any signature that covered what " +
				"changed. The notifier's ETag is left out, since it named the notifier's bytes.",
			DropsAnswerHeaders: []string{relay.ETagHeader},
		},
		{
			ID:        relay.EditSubscriptionURLSearchRekey,
			Name:      "subscription-url-search-rekey",
			Legs:      []string{"cdex-task-subscribe", "pas-subscription-subscribe"},
			Role:      relay.RoleRecipient,
			Direction: relay.DirectionRequest,
			Paths:     []string{"the url search parameter of GET [base]/Subscription"},
			Kind:      relay.EditKindRewriteQuery,
			Precondition: "Always (not an opt-in), on a payer's Subscription search the provider's gateway forwards to " +
				"its own system, which the payer's gateway frames with no url and the searched endpoint sealed (two sealed " +
				"values during the payer's key rotation), once the search with its url is within the parameters the network " +
				"carries. The url parameter is written first; every other parameter is carried byte for byte as framed. A " +
				"search without the sealed values, or with values that are not sealed values, is refused, never carried, " +
				"and the exchange records why the edit was not made.",
			Authority: "The notification address the subscription-endpoint-rekey edit would write for the authenticated " +
				"payer, the topic and each sealed endpoint, rebuilt from the provider gateway's ingress base. Nothing is " +
				"stored per search.",
			Disclosure: "When a payer's system searches a provider's Subscriptions by url, the provider's gateway writes " +
				"the url its system is searched by: its own notification address for the same endpoint, which reaches it " +
				"only sealed, so the search finds the Subscriptions that payer made, and only those. The forward names " +
				"the edit in a header.",
		},
		{
			ID:        relay.EditPASNotifiedReturnRekey,
			Name:      "pas-notified-return-rekey",
			Legs:      []string{"pas-subscription-notify"},
			Role:      relay.RoleRecipient,
			Direction: relay.DirectionRequest,
			Paths:     []string{"Task.input[i].valueUrl (the input whose type is PASTempCodes#payer-url)"},
			Kind:      relay.EditKindReplaceValue,
			Precondition: "Always (not an opt-in), on a payer's PAS notification with content the provider's gateway delivers " +
				"to its own system's notification endpoint, for each Task in the notified decision with a payer-url input " +
				"written as a valueUrl, decided by structure (the input's type coding, never text), when the payer that sent " +
				"it takes the documentation submission. Made even where an in-payload signature covers the payer-url; that " +
				"signature then no longer verifies in the provider's copy, and the delivery says so. A payer-url in any other " +
				"form, or a payer that does not take the submission, is carried as sent and the exchange records that it was " +
				"not re-keyed.",
			Authority: "The provider gateway's own network return path for the authenticated payer that sent the notification: " +
				"a fixed address under its Da Vinci ingress base, rebuilt from that payer's network identity. Nothing is " +
				"read from the notification and nothing is stored per Subscription or notification.",
			Disclosure: "When a payer's notification of a prior-authorization decision carries a Task whose payer-url names " +
				"where to send documentation, the provider's gateway replaces that address with its own return path for the " +
				"same payer, as it does on the payer's answer, so the documentation the provider sends there reaches that " +
				"payer through the network. The delivery names the edit in a header, and names any signature that covered " +
				"the address.",
		},
		{
			ID:           relay.EditSubscriptionLocationRekey,
			Name:         "subscription-location-rekey",
			Legs:         []string{"cdex-task-subscribe", "pas-subscription-subscribe"},
			Role:         relay.RoleRequester,
			Direction:    relay.DirectionResponse,
			Kind:         relay.EditKindSetHeader,
			Header:       "Location",
			HeaderInputs: []string{relay.SubscriptionLocationInputBase, relay.SubscriptionLocationInputTail},
			Precondition: "Always (not an opt-in), on a 2xx answer to a subscriber's Subscription create or update that the " +
				"subscriber's gateway relays to its own system, when the notifier's gateway framed the Subscription's " +
				"location (subscriptionLocation): the tail of the Location the notifier's system answered, " +
				"Subscription/{id} or Subscription/{id}/_history/{vid}, framed only when that Location lies under the " +
				"notifier's own base by the segment-boundary match. An answer with no location framed is carried without a " +
				"Location; one whose framed value cannot be read as a Subscription location is carried without one too, and " +
				"the exchange records why.",
			Authority: "The subscriber gateway's own per-counterpart base, rebuilt from its ingress base and the counterpart's " +
				"network identity, then the tail the notifier's gateway framed. Nothing is stored per request.",
			Disclosure: "When a subscriber's system creates or updates a Subscription, its gateway answers with a Location " +
				"naming the Subscription at the gateway's base for the counterpart, as a FHIR create's or update's answer " +
				"names the resource, never at the notifier's own server. The answer names the edit in a header.",
		},
	}
}
