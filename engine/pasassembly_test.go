package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const assemblyRealPending = `{"resourceType":"Bundle","meta":{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-response-bundle"]},"identifier":{"system":"http://example.org/SUBMITTER_TRANSACTION_IDENTIFIER","value":"26e95525-758c-487c-a377-3f159acac6c8"},"type":"collection","timestamp":"2026-09-10T16:08:47.765+00:00","entry":[{"fullUrl":"http://localhost:8081/fhir/ClaimResponse/1770","resource":{"resourceType":"ClaimResponse","id":"1770","meta":{"versionId":"2","lastUpdated":"2026-09-10T16:08:47.769+00:00","profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"],"tag":[{"system":"http://example.org/fhir/us/davinci-pas/internal-tags","code":"pended-resolution","display":"Pended Resolution"}]},"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers","extension":[{"url":"applicationSenderCode","valueString":"1234567893"},{"url":"applicationReceiverCode","valueString":"8189991234"}]},{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-administrationReferenceNumber","valueString":"AUTH-PEND0001"}],"identifier":[{"system":"http://example.org/PATIENT_EVENT_TRACE_NUMBER","value":"2ca2e10c-e924-4e12-9d69-5b71e4e91c31"}],"status":"active","type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional","display":"Professional"}]},"use":"preauthorization","patient":{"reference":"Patient/SubscriberExample"},"created":"2026-09-10T16:08:47+00:00","insurer":{"reference":"Organization/example"},"requestor":{"reference":"Organization/UMOExample"},"request":{"reference":"Claim/1768"},"outcome":"queued","item":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber","valueIdentifier":{"system":"http://example.org/ITEM_TRACE_NUMBER","value":"prior-auth-required-trace"}}],"itemSequence":1,"adjudication":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A4","display":"Pending"}]}}]}],"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted","display":"Submitted Amount"}]}}]}]}},{"fullUrl":"urn:uuid:b2882a29-4ac4-4b3b-a8d2-8b7288c5a175","resource":{"resourceType":"Task","id":"b2882a29-4ac4-4b3b-a8d2-8b7288c5a175","meta":{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-task"]},"identifier":[{"system":"urn:ietf:rfc:3986","value":"urn:uuid:b2882a29-4ac4-4b3b-a8d2-8b7288c5a175"}],"status":"requested","intent":"order","code":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"attachment-request-questionnaire","display":"Questionnaire Attachment Request"}]},"for":{"reference":"http://localhost:8081/fhir/Patient/SubscriberExample"},"requester":{"reference":"http://localhost:8081/fhir/Organization/example","identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}},"owner":{"reference":"http://localhost:8081/fhir/Organization/example","identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}},"reasonCode":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"priorAuthorization","display":"Prior Authorization Information Request"}]},"reasonReference":{"reference":"http://localhost:8081/fhir/Claim/1768"},"input":[{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"payer-url","display":"Payer URL"}]},"valueUrl":"http://localhost:8081/fhir"},{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-paLineNumber","valueInteger":1}],"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"questionnaires-needed","display":"Questionnaires Needed"}]},"valueIdentifier":{"system":"urn:ietf:rfc:3986","value":"http://example.org/fhir/Questionnaire/HomeOxygenDispatch"}}]}},{"fullUrl":"http://localhost:8081/fhir/Patient/SubscriberExample","resource":{"resourceType":"Patient","id":"SubscriberExample","meta":{"versionId":"1","lastUpdated":"2026-09-10T16:08:39.549+00:00","profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-subscriber"]},"language":"en","text":{"status":"generated","div":"<div xml:lang=\"en\" xmlns=\"http://www.w3.org/1999/xhtml\" lang=\"en\"><p class=\"res-header-id\"><b>Generated Narrative: Patient SubscriberExample</b></p><a name=\"SubscriberExample\"> </a><a name=\"hcSubscriberExample\"> </a><div style=\"display: inline-block; background-color: #d9e0e7; padding: 6px; margin: 4px; border: 1px solid #8da1b4; border-radius: 5px; line-height: 60%\"><p style=\"margin-bottom: 0px\">Language: en</p><p style=\"margin-bottom: 0px\">Profile: <a href=\"StructureDefinition-profile-subscriber.html\">PAS Subscriber Patient</a></p></div><p style=\"border: 1px #661aff solid; background-color: #e6e6ff; padding: 10px;\">JOE SMITH  Male, DoB Unknown ( http://example.org/MIN:12345678901)</p><hr/><table class=\"grid\"><tr><td style=\"background-color: #f3f5da\" title=\"A patient's military status.\"><a href=\"StructureDefinition-extension-militaryStatus.html\">Military Status</a></td><td colspan=\"3\"><span title=\"Codes:{https://codesystem.x12.org/005010/584 RU}\">RU</span></td></tr></table></div>"},"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-militaryStatus","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/584","code":"RU"}]}}],"identifier":[{"system":"http://example.org/MIN","value":"12345678901"}],"name":[{"family":"SMITH","given":["JOE"]}],"gender":"male"}},{"fullUrl":"http://localhost:8081/fhir/Organization/example","resource":{"resourceType":"Organization","id":"example","meta":{"versionId":"1","lastUpdated":"2026-09-10T16:08:39.313+00:00"},"language":"en","text":{"status":"generated","div":"<div xml:lang=\"en\" xmlns=\"http://www.w3.org/1999/xhtml\" lang=\"en\"><p class=\"res-header-id\"><b>Generated Narrative: Organization example</b></p><a name=\"example\"> </a><a name=\"hcexample\"> </a><div style=\"display: inline-block; background-color: #d9e0e7; padding: 6px; margin: 4px; border: 1px solid #8da1b4; border-radius: 5px; line-height: 60%\"><p style=\"margin-bottom: 0px\">Language: en</p></div><p><b>identifier</b>: <a href=\"http://terminology.hl7.org/6.2.0/NamingSystem-npi.html\" title=\"National Provider Identifier\">United States National Provider Identifier</a>/1234567893</p><p><b>active</b>: true</p><p><b>name</b>: University Medical Center</p><p><b>telecom</b>: <a href=\"tel:+15552343523\">+1 555 234 3523</a>, <a href=\"mailto:info@acme.org\">info@acme.org</a></p><p><b>address</b>: Galapagosweg 91 San Francisco CA 94107 US (work)</p></div>"},"identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}],"active":true,"name":"University Medical Center","telecom":[{"system":"phone","value":"+1 555 234 3523","use":"work"},{"system":"email","value":"info@acme.org","use":"work"}],"address":[{"use":"work","line":["Galapagosweg 91"],"city":"San Francisco","state":"CA","postalCode":"94107","country":"US"}]}},{"fullUrl":"http://localhost:8081/fhir/Organization/UMOExample","resource":{"resourceType":"Organization","id":"UMOExample","meta":{"versionId":"1","lastUpdated":"2026-09-10T16:08:39.543+00:00","profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-requestor"]},"language":"en","text":{"status":"generated","div":"<div xml:lang=\"en\" xmlns=\"http://www.w3.org/1999/xhtml\" lang=\"en\"><p class=\"res-header-id\"><b>Generated Narrative: Organization UMOExample</b></p><a name=\"UMOExample\"> </a><a name=\"hcUMOExample\"> </a><div style=\"display: inline-block; background-color: #d9e0e7; padding: 6px; margin: 4px; border: 1px solid #8da1b4; border-radius: 5px; line-height: 60%\"><p style=\"margin-bottom: 0px\">Language: en</p><p style=\"margin-bottom: 0px\">Profile: <a href=\"StructureDefinition-profile-requestor.html\">PAS Requestor Organization</a></p></div><p><b>identifier</b>: <a href=\"http://terminology.hl7.org/5.3.0/NamingSystem-npi.html\" title=\"National Provider Identifier\">United States National Provider Identifier</a>/8189991234</p><p><b>active</b>: true</p><p><b>type</b>: <span title=\"Codes:{https://codesystem.x12.org/005010/98 X3}\">X3</span></p><p><b>name</b>: DR. JOE SMITH CORPORATION</p><p><b>address</b>: 111 1ST STREET SAN DIEGO CA 92101 US </p></div>"},"identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"8189991234"}],"active":true,"type":[{"coding":[{"system":"https://codesystem.x12.org/005010/98","code":"X3"}]}],"name":"DR. JOE SMITH CORPORATION","address":[{"line":["111 1ST STREET"],"city":"SAN DIEGO","state":"CA","postalCode":"92101","country":"US"}]}},{"fullUrl":"http://localhost:8081/fhir/Claim/1768","resource":{"resourceType":"Claim","id":"1768","meta":{"versionId":"1","lastUpdated":"2026-09-10T16:08:47.759+00:00","profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim"]},"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers","extension":[{"url":"applicationSenderCode","valueString":"8189991234"},{"url":"applicationReceiverCode","valueString":"1234567893"}]}],"identifier":[{"system":"http://example.org/PATIENT_EVENT_TRACE_NUMBER","value":"67ef3222-0c7b-4332-b976-42e9a5f0c79b"}],"status":"active","type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional","display":"Professional"}]},"use":"preauthorization","patient":{"reference":"Patient/SubscriberExample"},"created":"2026-06-19T21:54:38+00:00","insurer":{"reference":"Organization/example"},"provider":{"reference":"Organization/UMOExample"},"priority":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/processpriority","code":"normal","display":"Normal"}]},"careTeam":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-careTeamClaimScope","valueBoolean":true}],"sequence":1,"provider":{"reference":"http://example.org/fhir/PractitionerRole/ReferralPractitionerRoleExample"}}],"insurance":[{"sequence":1,"focal":true,"coverage":{"reference":"Coverage/InsuranceExample"}}],"item":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/1322","code":"I","display":"Initial"}]}},{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceItemRequestType","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/1525","code":"HS","display":"Health Services Review"}]}},{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber","valueIdentifier":{"system":"http://example.org/ITEM_TRACE_NUMBER","value":"prior-auth-required-trace"}},{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-requestedService","valueReference":{"reference":"http://example.org/fhir/ServiceRequest/prior-auth-required-service-request"}}],"sequence":1,"careTeamSequence":[1],"category":{"coding":[{"system":"https://codesystem.x12.org/005010/1365","code":"3","display":"Consultation"}]},"productOrService":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0424"}]},"locationCodeableConcept":{"coding":[{"system":"https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set","code":"11","display":"Office"}]}}]}},{"fullUrl":"http://example.org/fhir/PractitionerRole/ReferralPractitionerRoleExample","resource":{"resourceType":"PractitionerRole","id":"ReferralPractitionerRoleExample","meta":{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-practitionerrole"]},"practitioner":{"reference":"http://example.org/fhir/Practitioner/ReferralPractitionerExample"},"telecom":[{"system":"phone","value":"4029993456"}]}},{"fullUrl":"http://localhost:8081/fhir/Coverage/InsuranceExample","resource":{"resourceType":"Coverage","id":"InsuranceExample","meta":{"versionId":"2","lastUpdated":"2026-09-10T16:08:47.523+00:00","profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-coverage"]},"identifier":[{"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v2-0203","code":"MB","display":"Member Number"},{"system":"http://terminology.hl7.org/CodeSystem/v2-0203","code":"MR","display":"Medical record number"}]},"system":"http://example.org/MIN","value":"1122334455"}],"status":"active","subscriber":{"reference":"Patient/SubscriberExample"},"subscriberId":"1122334455","beneficiary":{"reference":"Patient/SubscriberExample"},"relationship":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/subscriber-relationship","code":"self","display":"Self"}]},"period":{"start":"2026-06-19T21:54:38+00:00"},"payor":[{"reference":"Organization/example","identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}],"class":[{"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/coverage-class","code":"group","display":"Group"}]},"value":"GRP-001"},{"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/coverage-class","code":"plan","display":"Plan"}]},"value":"PLAN-001"}]}},{"fullUrl":"http://example.org/fhir/ServiceRequest/prior-auth-required-service-request","resource":{"resourceType":"ServiceRequest","id":"prior-auth-required-service-request","meta":{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-servicerequest"]},"status":"active","intent":"order","code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0424"}]},"subject":{"reference":"http://localhost:8081/fhir/Patient/SubscriberExample"}}},{"fullUrl":"http://example.org/fhir/Practitioner/ReferralPractitionerExample","resource":{"resourceType":"Practitioner","id":"ReferralPractitionerExample","meta":{"profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-practitioner"]},"identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}],"name":[{"family":"WATSON","given":["SUSAN"]}]}}]}`
const assemblyRealTerminal = `{"resourceType":"ClaimResponse","id":"1770","meta":{"versionId":"3","lastUpdated":"2026-09-10T16:08:50.783+00:00","profile":["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"]},"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers","extension":[{"url":"applicationSenderCode","valueString":"1234567893"},{"url":"applicationReceiverCode","valueString":"8189991234"}]}],"identifier":[{"system":"http://example.org/PATIENT_EVENT_TRACE_NUMBER","value":"2ca2e10c-e924-4e12-9d69-5b71e4e91c31"}],"status":"active","type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional","display":"Professional"}]},"use":"preauthorization","patient":{"reference":"Patient/SubscriberExample"},"created":"2026-09-10T16:08:47+00:00","insurer":{"reference":"Organization/example"},"requestor":{"reference":"Organization/UMOExample"},"request":{"reference":"Claim/1768"},"outcome":"complete","item":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber","valueIdentifier":{"system":"http://example.org/ITEM_TRACE_NUMBER","value":"prior-auth-required-trace"}},{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemPreAuthPeriod","valuePeriod":{"start":"2026-09-10T16:08:50+00:00","end":"2026-10-10T16:08:50+00:00"}}],"itemSequence":1,"adjudication":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A1","display":"Certified in total"}]}},{"url":"number","valueString":"AUTH-0002"}]}],"category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted","display":"Submitted Amount"}]}}]}]}`

func TestAssembleTerminalPASBundle(t *testing.T) {
	now := time.Date(2026, 9, 10, 17, 0, 0, 0, time.UTC)
	got, err := assembleTerminalPASBundle([]byte(assemblyRealPending), []byte(assemblyRealTerminal), now)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	json.Unmarshal([]byte(assemblyRealPending), &before)
	json.Unmarshal(got, &after)
	be, ae := before["entry"].([]any), after["entry"].([]any)
	if len(be) != len(ae) {
		t.Fatal("lost entries")
	}
	for i := 1; i < len(be); i++ {
		if !reflect.DeepEqual(be[i], ae[i]) {
			t.Fatalf("changed sibling %d", i)
		}
	}
	if be[0].(map[string]any)["fullUrl"] != ae[0].(map[string]any)["fullUrl"] {
		t.Fatal("changed response fullUrl")
	}
	if after["timestamp"] != now.Format(time.RFC3339Nano) {
		t.Fatal("wrong assembly clock")
	}
	if ae[0].(map[string]any)["resource"].(map[string]any)["outcome"] != "complete" {
		t.Fatal("stale outcome")
	}
}

func TestAssembleTerminalPASRejections(t *testing.T) {
	for _, field := range []string{"id", "patient", "request"} {
		t.Run(field, func(t *testing.T) {
			var terminal map[string]any
			json.Unmarshal([]byte(assemblyRealTerminal), &terminal)
			if field == "id" {
				terminal[field] = "different"
			} else {
				terminal[field] = map[string]any{"reference": "https://foreign.invalid/fhir/Patient/SubscriberExample"}
			}
			b, _ := json.Marshal(terminal)
			if _, err := assembleTerminalPASBundle([]byte(assemblyRealPending), b, time.Now()); err == nil {
				t.Fatal("accepted changed linkage")
			}
		})
	}
	for _, row := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"duplicate response", func(b map[string]any) { e := b["entry"].([]any); b["entry"] = append(e, e[0]) }},
		{"missing target", func(b map[string]any) { b["entry"] = b["entry"].([]any)[:1] }},
		{"foreign base", func(b map[string]any) {
			e := b["entry"].([]any)[0].(map[string]any)
			e["resource"].(map[string]any)["insurer"] = map[string]any{"reference": "https://foreign.invalid/fhir/Organization/example"}
		}},
		{"nested boundary", func(b map[string]any) {
			e := b["entry"].([]any)[1].(map[string]any)
			e["resource"].(map[string]any)["contained"] = []any{map[string]any{"resourceType": "Bundle", "id": "nested"}}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			var b map[string]any
			json.Unmarshal([]byte(assemblyRealPending), &b)
			row.mutate(b)
			raw, _ := json.Marshal(b)
			if _, err := assembleTerminalPASBundle(raw, []byte(assemblyRealTerminal), time.Now()); err == nil {
				t.Fatal("accepted invalid retained graph")
			}
		})
	}
}

func assemblySmallGraph() map[string]any {
	return map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
		map[string]any{"fullUrl": "https://payer.test/fhir/ClaimResponse/cr", "resource": map[string]any{"resourceType": "ClaimResponse", "id": "cr", "patient": map[string]any{"reference": "Patient/p"}, "request": map[string]any{"reference": "Claim/c"}}},
		map[string]any{"fullUrl": "https://payer.test/fhir/Patient/p", "resource": map[string]any{"resourceType": "Patient", "id": "p", "link": []any{map[string]any{"other": map[string]any{"reference": "Patient/p"}}}}},
		map[string]any{"fullUrl": "https://payer.test/fhir/Claim/c", "resource": map[string]any{"resourceType": "Claim", "id": "c", "patient": map[string]any{"reference": "Patient/p"}}},
	}}
}
func TestPASGraphReferenceScopes(t *testing.T) {
	cases := []struct {
		name   string
		good   bool
		mutate func(map[string]any, map[string]any, map[string]any)
	}{
		{"identity cycle", true, func(b, cr, p map[string]any) {}},
		{"exact absolute", true, func(b, cr, p map[string]any) {
			cr["patient"] = map[string]any{"reference": "https://payer.test/fhir/Patient/p"}
		}},
		{"exact version", true, func(b, cr, p map[string]any) {
			p["meta"] = map[string]any{"versionId": "2"}
			cr["patient"] = map[string]any{"reference": "Patient/p/_history/2"}
		}},
		{"wrong version", false, func(b, cr, p map[string]any) {
			p["meta"] = map[string]any{"versionId": "3"}
			cr["patient"] = map[string]any{"reference": "Patient/p/_history/2"}
		}},
		{"missing version", false, func(b, cr, p map[string]any) { cr["patient"] = map[string]any{"reference": "Patient/p/_history/2"} }},
		{"exact URN", true, func(b, cr, p map[string]any) {
			b["entry"].([]any)[1].(map[string]any)["fullUrl"] = "urn:uuid:10000000-0000-4000-8000-000000000001"
			p["link"] = nil
			cr["patient"] = map[string]any{"reference": "urn:uuid:10000000-0000-4000-8000-000000000001"}
			b["entry"].([]any)[2].(map[string]any)["resource"].(map[string]any)["patient"] = cr["patient"]
		}},
		{"URN relative undefined", false, func(b, cr, p map[string]any) {
			b["entry"].([]any)[0].(map[string]any)["fullUrl"] = "urn:uuid:10000000-0000-4000-8000-000000000001"
		}},
		{"contained", true, func(b, cr, p map[string]any) {
			p["contained"] = []any{map[string]any{"resourceType": "Organization", "id": "local"}}
			p["managingOrganization"] = map[string]any{"reference": "#local"}
		}},
		{"missing contained", false, func(b, cr, p map[string]any) { p["managingOrganization"] = map[string]any{"reference": "#absent"} }},
		{"contained scope", false, func(b, cr, p map[string]any) {
			p["contained"] = []any{map[string]any{"resourceType": "Organization", "id": "local"}}
			cr["insurer"] = map[string]any{"reference": "#local"}
		}},
		{"duplicate contained", false, func(b, cr, p map[string]any) {
			p["contained"] = []any{map[string]any{"resourceType": "Organization", "id": "local"}, map[string]any{"resourceType": "Organization", "id": "local"}}
		}},
		{"conflicting fullUrl identity", false, func(b, cr, p map[string]any) { p["id"] = "other" }},
		{"duplicate entry", false, func(b, cr, p map[string]any) { b["entry"] = append(b["entry"].([]any), b["entry"].([]any)[1]) }},
		{"no CR", false, func(b, cr, p map[string]any) { b["entry"] = b["entry"].([]any)[1:] }},
		{"rooted relative", false, func(b, cr, p map[string]any) { cr["patient"] = map[string]any{"reference": "/Patient/p"} }},
		{"query", false, func(b, cr, p map[string]any) { cr["patient"] = map[string]any{"reference": "Patient/p?x=1"} }},
		{"fragment", false, func(b, cr, p map[string]any) { cr["patient"] = map[string]any{"reference": "Patient/p#x"} }},
		{"new terminal target", false, func(b, cr, p map[string]any) { cr["insurer"] = map[string]any{"reference": "Organization/new"} }},
		{"deep JSON", false, func(b, cr, p map[string]any) {
			var child any = "leaf"
			for i := 0; i < 70; i++ {
				child = map[string]any{"extension": child}
			}
			p["extension"] = child
		}},
		{"edge budget", false, func(b, cr, p map[string]any) {
			refs := make([]any, pasGraphMaxReferences+1)
			for i := range refs {
				refs[i] = map[string]any{"reference": "Patient/p"}
			}
			p["link"] = refs
		}},
		{"resource budget", false, func(b, cr, p map[string]any) {
			for i := 0; i < pasGraphMaxResources; i++ {
				b["entry"] = append(b["entry"].([]any), b["entry"].([]any)[1])
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := assemblySmallGraph()
			es := b["entry"].([]any)
			cr := es[0].(map[string]any)["resource"].(map[string]any)
			p := es[1].(map[string]any)["resource"].(map[string]any)
			tc.mutate(b, cr, p)
			raw, _ := json.Marshal(b)
			err := validatePASBundleGraph(raw)
			if (err == nil) != tc.good {
				t.Fatalf("valid=%v, error=%v", tc.good, err)
			}
		})
	}
}

func TestPASAssemblyPreservesNumbers(t *testing.T) {
	original := strings.Replace(assemblyRealPending, `"type":"collection"`, `"type":"collection","extension":[{"url":"urn:test:decimal","valueDecimal":9007199254740993.125}]`, 1)
	out, err := assembleTerminalPASBundle([]byte(original), []byte(assemblyRealTerminal), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("9007199254740993.125")) {
		t.Fatal("changed retained decimal")
	}
}
func TestPASGraphRejectsAmbiguousJSON(t *testing.T) {
	raw := strings.Replace(assemblyRealPending, `"resourceType":"Bundle"`, `"resourceType":"Patient","resourceType":"Bundle"`, 1)
	if validatePASBundleGraph([]byte(raw)) == nil {
		t.Fatal("accepted duplicate JSON member")
	}
}
func TestPASGraphContainerBackReference(t *testing.T) {
	b := assemblySmallGraph()
	p := b["entry"].([]any)[1].(map[string]any)["resource"].(map[string]any)
	p["managingOrganization"] = map[string]any{"reference": "#"}
	raw, _ := json.Marshal(b)
	if validatePASBundleGraph(raw) == nil {
		t.Fatal("root resource has no containing resource")
	}
	delete(p, "managingOrganization")
	p["contained"] = []any{map[string]any{"resourceType": "Organization", "id": "local", "partOf": map[string]any{"reference": "#"}}}
	raw, _ = json.Marshal(b)
	if err := validatePASBundleGraph(raw); err != nil {
		t.Fatal(err)
	}
}

func TestPASAssemblyMalformedInputs(t *testing.T) {
	for _, raw := range []string{"null", "[]", "{}", `{"resourceType":"Bundle","type":"collection","entry":[null]}`, assemblyRealPending + ` {}`, strings.Repeat(" ", pasGraphMaxBytes+1)} {
		if err := validatePASBundleGraph([]byte(raw)); err == nil {
			t.Fatal("accepted malformed graph")
		}
	}
	for _, raw := range []string{"null", `{"resourceType":"Task"}`, `{"resourceType":"ClaimResponse"}`, strings.Repeat(" ", pasGraphMaxBytes+1)} {
		if _, err := assembleTerminalPASBundle([]byte(assemblyRealPending), []byte(raw), time.Time{}); err == nil {
			t.Fatal("accepted invalid replacement")
		}
	}
	for _, field := range []string{"patient", "request"} {
		var terminal map[string]any
		json.Unmarshal([]byte(assemblyRealTerminal), &terminal)
		delete(terminal, field)
		raw, _ := json.Marshal(terminal)
		if _, err := assembleTerminalPASBundle([]byte(assemblyRealPending), raw, time.Time{}); err == nil {
			t.Fatal("accepted missing linkage")
		}
	}
	var terminal map[string]any
	json.Unmarshal([]byte(assemblyRealTerminal), &terminal)
	terminal["insurer"] = map[string]any{"reference": "Organization/new-unresolved"}
	raw, _ := json.Marshal(terminal)
	if _, err := assembleTerminalPASBundle([]byte(assemblyRealPending), raw, time.Time{}); err == nil {
		t.Fatal("accepted newly unresolved terminal reference")
	}
}

func TestPASGraphResourceAndIdentityGuards(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{"missing fullUrl", func(e, r map[string]any) { delete(e, "fullUrl") }},
		{"relative fullUrl", func(e, r map[string]any) { e["fullUrl"] = "Patient/p" }},
		{"versioned fullUrl", func(e, r map[string]any) { e["fullUrl"] = "https://payer.test/fhir/Patient/p/_history/2" }},
		{"missing id", func(e, r map[string]any) { delete(r, "id") }},
		{"missing type", func(e, r map[string]any) { delete(r, "resourceType") }},
		{"invalid id", func(e, r map[string]any) { r["id"] = "p/invalid" }},
		{"malformed reference", func(e, r map[string]any) { r["managingOrganization"] = map[string]any{"reference": 42} }},
		{"empty reference", func(e, r map[string]any) { r["managingOrganization"] = map[string]any{"reference": ""} }},
		{"non-array contained", func(e, r map[string]any) { r["contained"] = true }},
		{"invalid contained", func(e, r map[string]any) { r["contained"] = []any{true} }},
		{"anonymous contained", func(e, r map[string]any) { r["contained"] = []any{map[string]any{"resourceType": "Organization"}} }},
		{"nested containment", func(e, r map[string]any) {
			r["contained"] = []any{map[string]any{"resourceType": "Organization", "id": "a", "contained": []any{map[string]any{"resourceType": "Organization", "id": "b"}}}}
		}},
		{"nested Parameters", func(e, r map[string]any) {
			r["contained"] = []any{map[string]any{"resourceType": "Parameters", "id": "a"}}
		}},
		{"contained resource budget", func(e, r map[string]any) {
			rs := []any{}
			for i := 0; i < pasGraphMaxResources; i++ {
				rs = append(rs, map[string]any{"resourceType": "Organization", "id": fmt.Sprint(i)})
			}
			r["contained"] = rs
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := assemblySmallGraph()
			e := b["entry"].([]any)[1].(map[string]any)
			r := e["resource"].(map[string]any)
			tc.mutate(e, r)
			raw, _ := json.Marshal(b)
			if err := validatePASBundleGraph(raw); err == nil {
				t.Fatal("accepted malformed identity")
			}
		})
	}
}

func TestPASGraphRetainedMetadataAndExactURIs(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"bundle reference", func(b map[string]any) {
			b["signature"] = map[string]any{"who": map[string]any{"reference": "https://foreign.test/Practitioner/missing"}}
		}},
		{"entry reference", func(b map[string]any) {
			b["entry"].([]any)[0].(map[string]any)["extension"] = []any{map[string]any{"url": "urn:test", "valueReference": map[string]any{"reference": "Patient/p"}}}
		}},
		{"encoded slash", func(b map[string]any) {
			b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)["patient"] = map[string]any{"reference": "Patient%2Fp"}
		}},
		{"empty query", func(b map[string]any) {
			b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)["patient"] = map[string]any{"reference": "Patient/p?"}
		}},
		{"contained type absent", func(b map[string]any) {
			r := b["entry"].([]any)[0].(map[string]any)["resource"].(map[string]any)
			r["contained"] = []any{map[string]any{"id": "local"}}
			r["insurer"] = map[string]any{"reference": "#local"}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			b := assemblySmallGraph()
			row.mutate(b)
			raw, _ := json.Marshal(b)
			if validatePASBundleGraph(raw) == nil {
				t.Fatal("accepted invalid retained reference")
			}
		})
	}
}

func TestPASGraphRejectsUnsafePollingIDs(t *testing.T) {
	for _, id := range []string{"encoded%2Fpath", "back\\slash", "..", ".", strings.Repeat("x", 65), "space id"} {
		t.Run(id, func(t *testing.T) {
			b := assemblySmallGraph()
			e := b["entry"].([]any)[0].(map[string]any)
			e["fullUrl"] = "urn:uuid:10000000-0000-4000-8000-000000000001"
			e["resource"].(map[string]any)["id"] = id
			e["resource"].(map[string]any)["patient"] = map[string]any{"reference": "https://payer.test/fhir/Patient/p"}
			e["resource"].(map[string]any)["request"] = map[string]any{"reference": "https://payer.test/fhir/Claim/c"}
			raw, _ := json.Marshal(b)
			if validatePASBundleGraph(raw) == nil {
				t.Fatal("accepted unsafe resource identity")
			}
		})
	}
}

func TestPASGraphRejectsUnusableRESTBases(t *testing.T) {
	for _, full := range []string{"http:/fhir/ClaimResponse/cr", "https://payer.test/f%68ir/ClaimResponse/cr"} {
		t.Run(full, func(t *testing.T) {
			b := assemblySmallGraph()
			b["entry"].([]any)[0].(map[string]any)["fullUrl"] = full
			if strings.HasPrefix(full, "http:/fhir") {
				for _, v := range b["entry"].([]any) {
					e := v.(map[string]any)
					e["fullUrl"] = strings.Replace(e["fullUrl"].(string), "https://payer.test/fhir", "http:/fhir", 1)
				}
			}
			raw, _ := json.Marshal(b)
			if validatePASBundleGraph(raw) == nil {
				t.Fatal("accepted undefined or normalized REST base")
			}
		})
	}
}

func TestPASAssemblyRejectsRetainedBundleSignature(t *testing.T) {
	var pending map[string]any
	json.Unmarshal([]byte(assemblyRealPending), &pending)
	pending["signature"] = map[string]any{"who": map[string]any{"reference": "http://localhost:8081/fhir/Organization/example"}, "data": "c2lnbmF0dXJl"}
	raw, _ := json.Marshal(pending)
	if _, bad := validateNativePASResponse(raw); bad.Status != 0 {
		t.Fatal("signed direct graph should remain eligible for verbatim relay")
	}
	if _, err := assembleTerminalPASBundle(raw, []byte(assemblyRealTerminal), fixedClock()); err == nil {
		t.Fatal("assembly retained a signature over changed Bundle content")
	}
}
