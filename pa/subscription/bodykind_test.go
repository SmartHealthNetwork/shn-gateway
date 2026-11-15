package subscription

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ClassifyBody decides by structure alone: each row's body is classified
// the same whatever it would be labelled.
func TestClassifyBody(t *testing.T) {
	const bom = "\xEF\xBB\xBF"
	deepJSON := `{"resourceType":"Patient","a":` + strings.Repeat("[", maxDepth+1) + strings.Repeat("]", maxDepth+1) + `}`
	deepXML := `<Patient xmlns="http://hl7.org/fhir">` + strings.Repeat("<a>", maxDepth+1) + strings.Repeat("</a>", maxDepth+1) + `</Patient>`
	deepOther := `<html>` + strings.Repeat("<a>", 10*maxDepth) + `</html>`
	// Past encoding/json's own nesting limit (10000), which a decoder's
	// failure must not turn into text.
	pastDecoder := strings.Repeat("[", 10001) + patientJSON + strings.Repeat("]", 10001)
	const fhirPatientXML = `<Patient xmlns="http://hl7.org/fhir"><name><family value="Doe"/></name></Patient>`
	for _, c := range []struct {
		name, body string
		kind       BodyKind
		rt         string
		unreadable bool
	}{
		{"no bytes", "", BodyEmpty, "", false},
		{"a UTF-16LE byte order mark", "\xFF\xFE<\x00P\x00", BodyFHIRJSON, "", true},
		{"a UTF-16BE byte order mark", "\xFE\xFF\x00<\x00P", BodyFHIRJSON, "", true},
		{"a UTF-32LE byte order mark", "\xFF\xFE\x00\x00{\x00\x00\x00", BodyFHIRJSON, "", true},
		{"a UTF-32BE byte order mark", "\x00\x00\xFE\xFF\x00\x00\x00{", BodyFHIRJSON, "", true},
		{"a UTF-16 byte order mark alone", "\xFF\xFE", BodyFHIRJSON, "", true},
		{"UTF-16LE JSON without a byte order mark", "{\x00\"\x00r\x00", BodyFHIRJSON, "", true},
		{"UTF-16BE JSON without a byte order mark", "\x00{\x00\"\x00r", BodyFHIRJSON, "", true},
		{"UTF-16LE XML without a byte order mark", "<\x00?\x00x\x00", BodyFHIRJSON, "", true},
		{"UTF-16BE XML without a byte order mark", "\x00<\x00?\x00x", BodyFHIRJSON, "", true},
		{"UTF-32LE JSON without a byte order mark", "{\x00\x00\x00\"\x00\x00\x00", BodyFHIRJSON, "", true},
		{"UTF-32BE XML without a byte order mark", "\x00\x00\x00<\x00\x00\x00?", BodyFHIRJSON, "", true},
		{"a NUL late in plain text", "upstream down" + strings.Repeat(" ", 4096) + "\x00", BodyFHIRJSON, "", true},
		{"whitespace only", " \t\r\n", BodyEmpty, "", false},
		{"a byte order mark only", bom, BodyEmpty, "", false},
		{"a byte order mark and whitespace", bom + "\n ", BodyEmpty, "", false},
		{"a FHIR JSON object", patientJSON, BodyFHIRJSON, "Patient", false},
		{"leading whitespace", "\r\n\t " + patientJSON, BodyFHIRJSON, "Patient", false},
		{"a byte order mark", bom + patientJSON, BodyFHIRJSON, "Patient", false},
		{"a resourceType in another case", `{"RESOURCETYPE":"Patient"}`, BodyFHIRJSON, "Patient", false},
		{"a resourceType that is not a string", `{"resourceType":5}`, BodyFHIRJSON, "", false},
		{"a JSON object with no resourceType", `{"error":"x"}`, BodyNotFHIR, "", false},
		{"a JSON array holding a resource", "[" + patientJSON + "]", BodyFHIRJSON, "", false},
		{"a JSON array holding a resource deep", `[{"a":[{"b":` + patientJSON + `}]}]`, BodyFHIRJSON, "", false},
		{"a JSON array holding none", `[{"error":"x"},1,"a"]`, BodyNotFHIR, "", false},
		{"text in brackets", "[error] upstream timed out", BodyNotFHIR, "", false},
		// A JSON object or array, then more: the value is judged as it is
		// alone, and what follows it by the rule a run of scalars follows
		// (below): FHIR JSON or JSON that cannot be read after it is
		// unreadable, and anything else after it is text, never judged.
		{"a JSON array holding a resource, then text", "[" + patientJSON + "] trailing", BodyFHIRJSON, "", false},
		{"a JSON array holding a resource, then another value", "[" + patientJSON + "] [1]", BodyFHIRJSON, "", false},
		{"a JSON array holding none, then text", "[1,2] trailing", BodyNotFHIR, "", false},
		{"a JSON array of a status code, then its text", "[404] Not Found", BodyNotFHIR, "", false},
		{"a JSON error, then a PHP warning", phpWarningAfterJSON, BodyNotFHIR, "", false},
		{"an OperationOutcome, then a PHP warning", answerOutcome + phpWarning, BodyFHIRJSON, "OperationOutcome", false},
		{"a JSON error, then another, then text", `{"error":"x"} {"error":"y"} [1] done`, BodyNotFHIR, "", false},
		{"a JSON error, then a resource", `{"error":"x"} ` + patientJSON, BodyFHIRJSON, "", true},
		{"a JSON error, then an array holding a resource", `{"error":"x"}[` + patientJSON + `]`, BodyFHIRJSON, "", true},
		{"a JSON error, then a scalar, then a resource", `{"error":"x"} 1 ` + patientJSON, BodyFHIRJSON, "", true},
		{"a JSON error, then JSON that is not closed", `{"error":"x"} {"resourceType":"Patient"`, BodyFHIRJSON, "", true},
		{"a JSON error, then JSON read two ways", `{"error":"x"} {"a":1,"A":2}`, BodyFHIRJSON, "", true},
		{"a JSON error, then JSON nested past the bound", `{"error":"x"} ` + strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2), BodyFHIRJSON, "", true},
		{"an OperationOutcome, then a Patient", answerOutcome + patientJSON, BodyFHIRJSON, "", true},
		{"a JSON error, then bracketed text", `{"error":"x"} [error] upstream timed out`, BodyNotFHIR, "", false},
		{"a JSON error, then FHIR XML", `{"error":"x"}<Patient xmlns="http://hl7.org/fhir"/>`, BodyNotFHIR, "", false},
		{"a JSON array holding none, then one holding a resource", "[{}] [" + patientJSON + "]", BodyFHIRJSON, "", true},
		{"a JSON array, then whitespace", "[1,2] \r\n\t", BodyNotFHIR, "", false},
		{"a JSON array read two ways", `[{"a":1,"a":` + patientJSON + `}]`, BodyFHIRJSON, "", true},
		{"a JSON array read two ways, then text", `[{"a":1,"A":2}] x`, BodyFHIRJSON, "", true},
		{"a JSON array nested past the bound", strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2), BodyFHIRJSON, "", true},
		{"a JSON object holding a resource", `{"error":"x","patient":` + patientJSON + `}`, BodyFHIRJSON, "", false},
		{"a JSON object holding a contained member", `{"error":"x","contained":[{"id":"a"}]}`, BodyFHIRJSON, "", false},
		{"a JSON object holding a resource in another case", `{"error":{"RESOURCETYPE":"Patient"}}`, BodyFHIRJSON, "", false},
		{"a JSON string", `"Patient"`, BodyNotFHIR, "", false},
		{"a JSON number", `42`, BodyNotFHIR, "", false},
		{"a JSON null", `null`, BodyNotFHIR, "", false},
		{"truncated JSON", `{"resourceType":"Patient"`, BodyFHIRJSON, "", true},
		{"a repeated member", `{"resourceType":"OperationOutcome","resourceType":"Patient"}`, BodyFHIRJSON, "", true},
		{"a member repeated in another case", `{"resourceType":"OperationOutcome","ResourceType":"Patient"}`, BodyFHIRJSON, "", true},
		{"JSON then text", patientJSON + "x", BodyFHIRJSON, "Patient", false},
		{"JSON nested past the bound", deepJSON, BodyFHIRJSON, "", true},
		{"not UTF-8", "{\"resourceType\":\"P\xffatient\"}", BodyFHIRJSON, "", true},
		{"plain text", "upstream down", BodyNotFHIR, "", false},
		{"plain text that mentions a resource", `resourceType: Patient`, BodyNotFHIR, "", false},
		{"HTML", "<!DOCTYPE html><html><body><p>down<br></body></html>", BodyNotFHIR, "", false},
		{"XHTML", `<html xmlns="http://www.w3.org/1999/xhtml"><body/></html>`, BodyNotFHIR, "", false},
		{"an html DOCTYPE", "<!DOCTYPE html>\n<html><body>x</body></html>", BodyNotFHIR, "", false},
		{"an XHTML DOCTYPE with its public id", `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd"><html xmlns="http://www.w3.org/1999/xhtml"/>`, BodyNotFHIR, "", false},
		{"a DOCTYPE fixing the FHIR namespace", `<!DOCTYPE Patient [<!ATTLIST Patient xmlns CDATA #FIXED "http://hl7.org/fhir">]><Patient><name><family value="Doe"/></name></Patient>`, BodyFHIRXML, "", true},
		{"a DOCTYPE declaring an entity", `<!DOCTYPE x [<!ENTITY ns "http://hl7.org/fhir">]><Patient xmlns="&ns;"/>`, BodyFHIRXML, "", true},
		{"a doctype in lower case with a subset", `<!doctype x [<!ENTITY a "b">]><x/>`, BodyFHIRXML, "", true},
		{"another directive holding a bracket", `<!ELEMENT a (b)*[x]><html><body>x</body></html>`, BodyNotFHIR, "", false},
		{"FHIR XML", `<Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient>`, BodyFHIRXML, "Patient", false},
		{"FHIR XML by a prefix", `<f:Patient xmlns:f="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", false},
		{"FHIR XML after a declaration", `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", false},
		{"FHIR XML declaring another encoding", `<?xml version="1.0" encoding="ISO-8859-1"?><Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", false},
		{"FHIR XML after a processing instruction", `<?xml-stylesheet href="a.xsl"?><Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", false},
		{"FHIR XML after a DOCTYPE", `<!DOCTYPE Patient><Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", false},
		{"FHIR XML after a comment", `<!-- a --><Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", false},
		{"FHIR XML after a byte order mark", bom + `<Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", false},
		{"FHIR XML with a comment after the root", `<Patient xmlns="http://hl7.org/fhir"/><!-- a -->` + "\n", BodyFHIRXML, "Patient", false},
		{"a FHIR resource type's root in no namespace", `<Patient><id value="p1"/></Patient>`, BodyFHIRXML, "Patient", true},
		{"a FHIR resource type's root in no namespace after a declaration", `<?xml version="1.0"?><Bundle><entry/></Bundle>`, BodyFHIRXML, "Bundle", true},
		{"a FHIR resource type's root in no namespace, not well-formed", `<OperationOutcome><issue>`, BodyFHIRXML, "OperationOutcome", true},
		{"a FHIR resource type's name in lower case, in no namespace", `<patient><id value="p1"/></patient>`, BodyNotFHIR, "", false},
		{"html in no namespace", `<html><body>x</body></html>`, BodyNotFHIR, "", false},
		{"another name in no namespace", `<error><message>down</message></error>`, BodyNotFHIR, "", false},
		{"a FHIR resource type's root under an unbound prefix", `<f:Patient/>`, BodyFHIRXML, "Patient", true},
		{"a FHIR resource type's root in another namespace", `<Patient xmlns="urn:x"><id value="p1"/></Patient>`, BodyFHIRXML, "Patient", true},
		{"a FHIR resource type's root under a DOCTYPE naming it", `<!DOCTYPE f:Patient SYSTEM "http://example.org/p.dtd"><f:Patient><id value="p1"/></f:Patient>`, BodyFHIRXML, "Patient", true},
		{"a FHIR resource type's root under a prefix bound elsewhere", `<f:Bundle xmlns:f="http://example.org/fhir"/>`, BodyFHIRXML, "Bundle", true},
		{"XML in another namespace", `<record xmlns="http://example.org/fhir"><id value="p1"/></record>`, BodyNotFHIR, "", false},
		{"XHTML", `<html xmlns="http://www.w3.org/1999/xhtml"><body><p>502</p></body></html>`, BodyNotFHIR, "", false},
		{"a SOAP Envelope", `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><soap:Fault><faultstring>down</faultstring></soap:Fault></soap:Body></soap:Envelope>`, BodyNotFHIR, "", false},
		{"a FHIR root, namespaced, unchanged", `<Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient>`, BodyFHIRXML, "Patient", false},
		{"a FHIR resource type's root in a namespace sharing the FHIR one's spelling", `<Patient xmlns="http://hl7.org/fhir/"/>`, BodyFHIRXML, "Patient", true},
		{"another root in a namespace sharing the FHIR one's spelling", `<record xmlns="http://hl7.org/fhir/"/>`, BodyNotFHIR, "", false},
		{"text before the root", `x<Patient xmlns="http://hl7.org/fhir"/>`, BodyNotFHIR, "", false},
		{"text after a comment, before the root", `<!-- c -->x<Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "", true},
		{"FHIR XML not closed", `<Patient xmlns="http://hl7.org/fhir"><name>`, BodyFHIRXML, "Patient", true},
		{"FHIR XML closed wrong", `<Patient xmlns="http://hl7.org/fhir"><name></id></Patient>`, BodyFHIRXML, "Patient", true},
		{"FHIR XML then a second root", `<Patient xmlns="http://hl7.org/fhir"/><Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", true},
		{"FHIR XML then text", `<Patient xmlns="http://hl7.org/fhir"/>x`, BodyFHIRXML, "Patient", true},
		{"FHIR XML nested past the bound", deepXML, BodyFHIRXML, "Patient", true},
		{"FHIR XML with an unknown entity", `<Patient xmlns="http://hl7.org/fhir">&x;</Patient>`, BodyFHIRXML, "Patient", true},
		{"deep XML not in the FHIR namespace", deepOther, BodyNotFHIR, "", false},
		// Nesting past the bound is unreadable however deep it goes.
		{"a JSON array holding a resource past the decoder's own nesting limit", pastDecoder, BodyFHIRJSON, "", true},
		{"a JSON array nested past the decoder's own limit, holding none", strings.Repeat("[", 10001) + strings.Repeat("]", 10001), BodyFHIRJSON, "", true},
		{"bracketed proxy text with a stray quote", `[error] upstream said "down`, BodyNotFHIR, "", false},
		// Under a root outside the FHIR namespace, an element in it is FHIR
		// inside a body that is not: by the namespace alone, never a name.
		{"a FHIR Patient wrapped in an error element", `<error>` + fhirPatientXML + `</error>`, BodyFHIRXML, "", false},
		{"a SOAP Body holding a FHIR Patient", `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` + fhirPatientXML + `</soap:Body></soap:Envelope>`, BodyFHIRXML, "", false},
		{"a FHIR element by a prefix the root binds", `<error xmlns:f="http://hl7.org/fhir"><f:name><f:family value="Doe"/></f:name></error>`, BodyFHIRXML, "", false},
		{"a FHIR Patient after HTML's unclosed elements and entities", `<html><body><p>502&nbsp;bad gateway<br><p>retry` + fhirPatientXML + `</body></html>`, BodyFHIRXML, "", true},
		// A document that is not well-formed is read no further than where
		// it stops being so: one that declares the FHIR namespace anywhere
		// cannot be read one way only, and is refused as unreadable.
		{"a FHIR Patient after an end tag that closes nothing", `<html><body><p>502</p></b>` + fhirPatientXML + `</body></html>`, BodyFHIRXML, "", true},
		{"a FHIR Patient after a bare < in an error's text", `<error><message>id < 0</message>` + fhirPatientXML + `</error>`, BodyFHIRXML, "", true},
		{"a FHIR Patient after a stray end tag", `<html><body><p>Not found</p></p>` + fhirPatientXML + `</body></html>`, BodyFHIRXML, "", true},
		{"a FHIR Patient after a bare < in a page's text", `<html><body>1 < 2 ` + fhirPatientXML + `</body></html>`, BodyFHIRXML, "", true},
		{"a FHIR Patient after a script's comparison", `<html><script>if (a<b) {}</script>` + fhirPatientXML + `</html>`, BodyFHIRXML, "", true},
		{"a FHIR Patient after <= in text", `<html><p>a <= b</p>` + fhirPatientXML + `</html>`, BodyFHIRXML, "", true},
		{"a FHIR Patient after a comment holding --", `<html><!-- -- -->` + fhirPatientXML + `</html>`, BodyFHIRXML, "", true},
		{"a FHIR element by a prefix the root binds, after a bare <", `<error xmlns:f="http://hl7.org/fhir"><m>1 < 2</m><f:name><f:family value="Doe"/></f:name></error>`, BodyFHIRXML, "", true},
		{"a FHIR element by its own declaration, entity-escaped, after a bare <", `<error><m>1 < 2</m><Patient xmlns="http&#x3A;//hl7.org/fhir"/></error>`, BodyFHIRXML, "", true},
		{"markup no reader can read throughout, naming no FHIR namespace", `<html>` + strings.Repeat("< ", 5000) + `</html>`, BodyNotFHIR, "", false},
		// The FHIR namespace declared where only some readers read it, or
		// in a document Go's reader stops on early.
		{"a default FHIR namespace on a SOAP root, then a bare <", `<soap:Envelope xmlns:soap="urn:x" xmlns="http://hl7.org/fhir"><soap:Body>1 < 2<Patient><id value="p1"/></Patient></soap:Body></soap:Envelope>`, BodyFHIRXML, "", true},
		{"a default FHIR namespace on an inner element, then a bare <", `<e:error xmlns:e="urn:x"><e:w xmlns="http://hl7.org/fhir">a < b<Patient/></e:w></e:error>`, BodyFHIRXML, "", true},
		{"a FHIR prefix bound on a start tag that does not parse", `<error><x xmlns:f="http://hl7.org/fhir" < ><f:Patient><f:id value="p1"/></f:Patient></error>`, BodyFHIRXML, "", true},
		{"a FHIR Patient inside a directive", `<error><!x "> <Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient> "></error>`, BodyFHIRXML, "", true},
		{"a FHIR Patient inside a processing instruction", `<error><?x > <Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient> ?></error>`, BodyFHIRXML, "", true},
		{"a FHIR Patient inside a CDATA section", `<error><![CDATA[ > <Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient> ]]></error>`, BodyFHIRXML, "", true},
		{"a FHIR Patient inside a comment", `<error><!-- > <Patient xmlns="http://hl7.org/fhir"/> --></error>`, BodyFHIRXML, "", true},
		{"a FHIR Patient inside a directive before the root", `<!x "> <Patient xmlns='http://hl7.org/fhir'/> "><error/>`, BodyFHIRXML, "", true},
		{"a FHIR root under an XML 1.1 declaration", `<?xml version="1.1"?><Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient>`, BodyFHIRXML, "", true},
		{"a stray end tag before a FHIR root", `</x><Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient>`, BodyFHIRXML, "", true},
		{"a FHIR namespace spelled by character references in a broken page", `<html><p>1 < 2</p><Patient xmlns="http&#x3A;&#x2F;&#x2F;hl7.org&#x2F;fhir"/></html>`, BodyFHIRXML, "", true},
		// What a reader that recovers from errors may still read as FHIR: a
		// repeated attribute (no conforming reader reads one, and recovering
		// readers disagree on which holds), a stray quote, a declaration
		// right after a quoted value, a reference padded with zeros, an
		// unquoted value.
		{"a FHIR namespace beside another on one element", `<error><Patient xmlns="http://hl7.org/fhir" xmlns="urn:x"/></error>`, BodyFHIRXML, "", true},
		{"a FHIR namespace beside another on the root", `<foo xmlns="http://hl7.org/fhir" xmlns="urn:x"><Patient/></foo>`, BodyFHIRXML, "", true},
		{"a FHIR prefix bound twice", `<foo xmlns:f="http://hl7.org/fhir" xmlns:f="urn:x"><f:Patient/></foo>`, BodyFHIRXML, "", true},
		{"a repeated attribute under a FHIR root", `<Patient xmlns="http://hl7.org/fhir"><id value="a" value="b"/></Patient>`, BodyFHIRXML, "Patient", true},
		{"a stray quote before a FHIR Patient", `<x>if a<b's then<Patient xmlns="http://hl7.org/fhir"/></x>`, BodyFHIRXML, "", true},
		{"a quote in a comment before a FHIR Patient", `<x>&bogus;<!-- <i ' --><Patient xmlns="http://hl7.org/fhir"/></x>`, BodyFHIRXML, "", true},
		{"a reference padded with zeros", `<x>&bogus;<Patient xmlns="&#x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000068;ttp://hl7.org/fhir"/></x>`, BodyFHIRXML, "", true},
		{"a declaration right after a quoted value", `<x>&bogus;<Patient a="b"xmlns="http://hl7.org/fhir"/></x>`, BodyFHIRXML, "", true},
		{"an unquoted declaration in a broken page", `<x>1 < 2<Patient xmlns=http://hl7.org/fhir></x>`, BodyFHIRXML, "", true},
		// An element named xmlns, which readers place differently.
		{"an element named xmlns in the FHIR namespace", `<error><xmlns xmlns="http://hl7.org/fhir">Doe</xmlns></error>`, BodyFHIRXML, "", true},
		{"a root named xmlns in the FHIR namespace", `<xmlns xmlns="http://hl7.org/fhir">Doe</xmlns>`, BodyFHIRXML, "", true},
		{"an element named xmlns, declaring nothing", `<error><xmlns>Doe</xmlns></error>`, BodyNotFHIR, "", false},
		// An external DTD other than the W3C's HTML and XHTML ones, which only
		// a reader that fetches it applies: it refuses a body only beside a
		// declaration of the FHIR namespace (declaresFHIRNamespace); a page
		// that declares none is judged as if it named no DTD.
		{"a DOCTYPE naming a DTD of its own", `<!DOCTYPE error SYSTEM "fhir.dtd"><error><name value="Doe"/></error>`, BodyNotFHIR, "", false},
		{"a DOCTYPE naming another public DTD", `<!DOCTYPE error PUBLIC "-//Example//DTD Error//EN" "http://example.org/error.dtd"><error/>`, BodyNotFHIR, "", false},
		{"a DOCTYPE naming the W3C's HTML public id and another system id", `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://example.org/x.dtd"><html xmlns="http://www.w3.org/1999/xhtml"/>`, BodyNotFHIR, "", false},
		{"Drupal's XHTML+RDFa error page", drupalErrorPage, BodyNotFHIR, "", false},
		{"an XHTML page naming its DTD by a relative system id", relativeDTDPage, BodyNotFHIR, "", false},
		{"a FHIR root under a DTD of its own", `<!DOCTYPE Patient SYSTEM "fhir.dtd"><Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient>`, BodyFHIRXML, "", true},
		{"a FHIR OperationOutcome under a DTD of its own", `<!DOCTYPE OperationOutcome SYSTEM "http://example.org/oo.dtd"><OperationOutcome xmlns="http://hl7.org/fhir"><issue/></OperationOutcome>`, BodyFHIRXML, "", true},
		{"a FHIR element under an error naming a DTD of its own", `<!DOCTYPE error SYSTEM "e.dtd"><error><Patient xmlns="http://hl7.org/fhir"/></error>`, BodyFHIRXML, "", true},
		{"a FHIR prefix in a broken page naming a DTD of its own", `<!DOCTYPE html SYSTEM "x.dtd"><html xmlns:f="http://hl7.org/fhir"><p>1 < 2</p></html>`, BodyFHIRXML, "", true},
		{"Apache httpd's error page, an IETF public id alone", "<!DOCTYPE HTML PUBLIC \"-//IETF//DTD HTML 2.0//EN\">\n<html><head>\n<title>404 Not Found</title>\n</head><body>\n<h1>Not Found</h1>\n<p>The requested URL was not found on this server.</p>\n<hr>\n<address>Apache/2.4.58 Server at ehr.example Port 443</address>\n</body></html>\n", BodyNotFHIR, "", false},
		{"mod_proxy's 502 page", "<!DOCTYPE HTML PUBLIC \"-//IETF//DTD HTML 2.0//EN\">\n<html><head>\n<title>502 Proxy Error</title>\n</head><body>\n<h1>Proxy Error</h1>\n<p>The proxy server received an invalid\nresponse from an upstream server.<br />\n</p>\n</body></html>\n", BodyNotFHIR, "", false},
		{"HTML5's legacy-compat doctype", `<!DOCTYPE html SYSTEM "about:legacy-compat"><html><body>502</body></html>`, BodyNotFHIR, "", false},
		{"a lower-case HTML 4 doctype", `<!doctype html public "-//w3c//dtd html 4.01//en"><html><body>502<br></body></html>`, BodyNotFHIR, "", false},
		{"another public id with a system id of its own", `<!DOCTYPE error PUBLIC "-//IETF//DTD HTML 2.0//EN" "http://example.org/x.dtd"><error/>`, BodyNotFHIR, "", false},
		{"an HTML 4 page with its public id alone", `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.01//EN"><html><body>502<br></body></html>`, BodyNotFHIR, "", false},
		{"an XHTML 1.1 page with its DTD", `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.1//EN" "http://www.w3.org/TR/xhtml11/DTD/xhtml11.dtd"><html xmlns="http://www.w3.org/1999/xhtml"><body>502</body></html>`, BodyNotFHIR, "", false},
		// A declared encoding that does not read ASCII as ASCII, or one the
		// registry does not name: unknown. Every other is read as its bytes
		// show its markup (xmlEncodingLabels).
		{"a document declared in UTF-7", `<?xml version="1.0" encoding="UTF-7"?><x>+ADw-Patient xmlns=+ACI-http://hl7.org/fhir+ACI-/+AD4-</x>`, BodyFHIRXML, "", true},
		{"a document declared in UTF-16, in UTF-8's bytes", `<?xml version="1.0" encoding="UTF-16"?><error>down</error>`, BodyFHIRXML, "", true},
		{"a document declared in UTF-32", `<?xml version="1.0" encoding="UTF-32"?><error>down</error>`, BodyFHIRXML, "", true},
		{"a document declared in ISO-2022-JP", `<?xml version="1.0" encoding="ISO-2022-JP"?><error>down</error>`, BodyFHIRXML, "", true},
		{"a document declared in ISO-2022-KR", `<?xml version="1.0" encoding="ISO-2022-KR"?><error>down</error>`, BodyFHIRXML, "", true},
		{"a document declared in HZ", `<?xml version="1.0" encoding="HZ-GB-2312"?><error>down</error>`, BodyFHIRXML, "", true},
		{"a document declared in an EBCDIC code page", `<?xml version="1.0" encoding="IBM037"?><error>down</error>`, BodyFHIRXML, "", true},
		{"a document declared in x-user-defined", `<?xml version="1.0" encoding="x-user-defined"?><error>down</error>`, BodyFHIRXML, "", true},
		{"a document declared in an encoding no registry names", `<?xml version="1.0" encoding="x-example"?><error>down</error>`, BodyFHIRXML, "", true},
		{"a document declared in ISO-8859-2", latin2Error, BodyNotFHIR, "", false},
		{"a document declared in windows-1250", `<?xml version="1.0" encoding="windows-1250"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in KOI8-R", `<?xml version="1.0" encoding="KOI8-R"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in Shift_JIS", "<?xml version=\"1.0\" encoding=\"Shift_JIS\"?><error>\x83\x47\x83\x89\x81\x5b</error>", BodyNotFHIR, "", false},
		{"a document declared by Java's name for Shift_JIS", `<?xml version="1.0" encoding="MS932"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in EUC-JP", `<?xml version="1.0" encoding="EUC-JP"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in EUC-KR", `<?xml version="1.0" encoding="EUC-KR"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in GBK", `<?xml version="1.0" encoding="GBK"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in GB18030", `<?xml version="1.0" encoding="GB18030"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in GB2312", `<?xml version="1.0" encoding="GB2312"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in Big5", `<?xml version="1.0" encoding="Big5"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in US-ASCII", `<?xml version="1.0" encoding="US-ASCII"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a FHIR root declared in ISO-8859-2", `<?xml version="1.0" encoding="ISO-8859-2"?><Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", false},
		{"a FHIR element under an error declared in Shift_JIS", `<?xml version="1.0" encoding="Shift_JIS"?><error><Patient xmlns="http://hl7.org/fhir"/></error>`, BodyFHIRXML, "", false},
		// A name holding a character beyond ASCII is never read for its
		// structure in such an encoding: prefixes that differ only there
		// would read as one, so an inner declaration of one could hide
		// another's FHIR namespace. Beside a declaration of that namespace
		// the document is unreadable; a page that declares none is carried.
		{"a FHIR prefix shadowed by a prefix differing beyond ASCII, in Latin-1", "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><html xmlns:p\xe9=\"http://hl7.org/fhir\"><p\xe9:Patient xmlns:p\xe8=\"urn:x\"/></html>", BodyFHIRXML, "", true},
		{"a FHIR prefix shadowed on an intermediate element, in Latin-1", "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><html xmlns:p\xe9=\"http://hl7.org/fhir\"><body xmlns:p\xe8=\"urn:x\"><p\xe9:Foo/></body></html>", BodyFHIRXML, "", true},
		{"a FHIR prefix beyond ASCII shadowed by an ASCII prefix", "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><html xmlns:p\xe8=\"http://hl7.org/fhir\"><p\xe8:Patient xmlns:p_=\"urn:x\"/></html>", BodyFHIRXML, "", true},
		{"a FHIR prefix shadowed by a prefix differing beyond ASCII, in Shift_JIS", "<?xml version=\"1.0\" encoding=\"Shift_JIS\"?><html xmlns:p\x88\x9f=\"http://hl7.org/fhir\"><p\x88\x9f:Patient xmlns:p\x88\xa0=\"urn:x\"/></html>", BodyFHIRXML, "", true},
		{"an error with element names beyond ASCII, in Shift_JIS", "<?xml version=\"1.0\" encoding=\"Shift_JIS\"?><\x83\x47\x83\x89\x81\x5b><\x83\x81\x83\x62\x83\x5a\x81\x5b\x83\x57>not found</\x83\x81\x83\x62\x83\x5a\x81\x5b\x83\x57></\x83\x47\x83\x89\x81\x5b>", BodyNotFHIR, "", false},
		{"an error with an element name beyond ASCII, in Latin-1", "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><erreur><d\xe9tail>introuvable</d\xe9tail></erreur>", BodyNotFHIR, "", false},
		// A DOCTYPE's literal is read as a literal: a "[" a Shift_JIS
		// character holds inside one opens no internal subset.
		{"a DOCTYPE literal holding a Shift_JIS trail byte read as [", "<?xml version=\"1.0\" encoding=\"Shift_JIS\"?><!DOCTYPE error SYSTEM \"\x83\x47\x83\x89\x81\x5b.dtd\"><error>down</error>", BodyNotFHIR, "", false},
		{"an internal subset after a literal holding [", `<!DOCTYPE x SYSTEM "a[b.dtd" [<!ENTITY a "b">]><x/>`, BodyFHIRXML, "", true},
		{"a document declared in EBCDIC", "\x4C\x6F\xA7\x94\x93\x40\xA5\x85", BodyFHIRXML, "", true},
		{"a document declared in Latin-1", `<?xml version="1.0" encoding="ISO-8859-1"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a document declared in windows-1252", `<?xml version="1.0" encoding="windows-1252"?><error>down</error>`, BodyNotFHIR, "", false},
		{"a FHIR root declared in Latin-1", `<?xml version="1.0" encoding="ISO-8859-1"?><Patient xmlns="http://hl7.org/fhir"/>`, BodyFHIRXML, "Patient", false},
		// The carried side: pages a reader stumbles on, and documents that
		// mention the FHIR namespace without declaring it.
		{"a broken page linking to the FHIR specification", `<html><body><p>1 < 2</p><a href="http://hl7.org/fhir/R4/">FHIR</a></body></html>`, BodyNotFHIR, "", false},
		{"a comment naming the FHIR namespace in text", `<error><!-- see http://hl7.org/fhir --><m>down</m></error>`, BodyNotFHIR, "", false},
		{"a comment declaring the namespace with no markup inside", `<error><!-- xmlns="http://hl7.org/fhir" --><m>down</m></error>`, BodyNotFHIR, "", false},
		{"escaped text that reads as a FHIR element", `<error><m>&lt;Patient xmlns="http://hl7.org/fhir"/&gt;</m></error>`, BodyNotFHIR, "", false},
		{"escaped markup naming the FHIR namespace in a broken page", `<html><p>1 < 2</p><p>&lt;Patient xmlns="http://hl7.org/fhir"/&gt;</p></html>`, BodyNotFHIR, "", false},
		{"the namespace declaration's text in a broken page", `<html><p>1 < 2</p><p>write xmlns="http://hl7.org/fhir" on the root</p></html>`, BodyNotFHIR, "", false},
		{"a quoted > before the declaration in a broken page", `<html>1 < 2<x title="a>b" xmlns="http://hl7.org/fhir"/></html>`, BodyFHIRXML, "", true},
		{"a namespace that only starts like the FHIR one, in a broken page", `<html>1 < 2<x xmlns="http://hl7.org/fhirX"/></html>`, BodyNotFHIR, "", false},
		{"a page with a bare < and no FHIR", `<html><body>1 < 2</body></html>`, BodyNotFHIR, "", false},
		{"a page with a script and no FHIR", `<html><script>if (a<b && c) {}</script><p>502</p></html>`, BodyNotFHIR, "", false},
		{"a page with stray end tags and no FHIR", `<html><body><p>Not found</p></p></b></body></html>`, BodyNotFHIR, "", false},
		{"a page that ends in markup no reader can read", `<html><body>down <`, BodyNotFHIR, "", false},
		{"a prefix bound elsewhere, after a bare <", `<error xmlns:f="urn:x"><m>1 < 2</m><f:Patient/></error>`, BodyNotFHIR, "", false},
		{"a FHIR Patient after the root", `<error/>` + fhirPatientXML, BodyFHIRXML, "", false},
		{"a FHIR Patient deep under XHTML", `<html xmlns="http://www.w3.org/1999/xhtml">` + strings.Repeat("<div>", 10*maxDepth) + fhirPatientXML + strings.Repeat("</div>", 10*maxDepth) + `</html>`, BodyFHIRXML, "", false},
		{"an error naming an Endpoint in no namespace", `<Error><Code>PermanentRedirect</Code><Message>use the endpoint</Message><Endpoint>bucket.s3.example</Endpoint></Error>`, BodyNotFHIR, "", false},
		{"a Patient element in no namespace under an error", `<error><Patient><name>Doe</name></Patient></error>`, BodyNotFHIR, "", false},
		{"a Patient element in another namespace under an error", `<error><Patient xmlns="urn:x"/></error>`, BodyNotFHIR, "", false},
		// A run of JSON scalars, then FHIR JSON: unreadable; anything else
		// after the run is text, never judged.
		{"a number, then a resource", `1 ` + patientJSON, BodyFHIRJSON, "", true},
		{"null, then a resource", `null` + patientJSON, BodyFHIRJSON, "", true},
		{"a string, then a resource", `"x" ` + patientJSON, BodyFHIRJSON, "", true},
		{"true, then an array holding a resource", `true [` + patientJSON + `]`, BodyFHIRJSON, "", true},
		{"two numbers, then a resource", "1\n2 " + patientJSON, BodyFHIRJSON, "", true},
		{"a number, then truncated FHIR JSON", `1 {"resourceType":"Patient"`, BodyFHIRJSON, "", true},
		{"a status line", "404 Not Found", BodyNotFHIR, "", false},
		{"a number, then JSON naming no resource", `404 {"error":"x"}`, BodyNotFHIR, "", false},
		{"a number, then bracketed text", "502 [error] upstream timed out", BodyNotFHIR, "", false},
		{"text, then a resource", "Error: " + patientJSON, BodyNotFHIR, "", false},
		{"a number, text, then a resource", "404 Not Found " + patientJSON, BodyNotFHIR, "", false},
	} {
		kind, rt, err := ClassifyBody([]byte(c.body))
		if kind != c.kind || rt != c.rt || (err != nil) != c.unreadable || (err != nil && !errors.Is(err, ErrBodyUnreadable)) {
			t.Errorf("%s: %v %q %v, want %v %q unreadable %v", c.name, kind, rt, err, c.kind, c.rt, c.unreadable)
		}
	}
}

// A huge body is classified in time linear in its length, and a deep one is
// bounded: neither costs more than reading it.
// An xmlns value is read as an XML reader reads it: decimal and hex character
// references and the predefined entities, so no spelling of the FHIR
// namespace hides its declaration, and a reference to another character is
// another namespace. (Decimal references are built, not written, so the
// published module's text names no "#" and digits.)
func TestDeclaresFHIRNamespace_ReadsReferences(t *testing.T) {
	dec := func(c rune) string { return "&#" + strconv.Itoa(int(c)) + ";" }
	for _, v := range []string{
		"http://hl7.org/fhir",
		"http" + dec(':') + "//hl7.org/fhir",
		"&#x68;ttp://hl7.org" + dec('/') + "fhir",
		"http:&#x2F;&#x2f;hl7.org/fhir",
	} {
		if !declaresFHIRNamespace([]byte(`<x xmlns="` + v + `"/>`)) {
			t.Errorf("%q: not read as the FHIR namespace", v)
		}
	}
	for _, v := range []string{
		"http://hl7.org/fhir/",
		"http" + dec(';') + "//hl7.org/fhir",
		"http:&amp;#x2F;/hl7.org/fhir",
		"http://hl7.org/fhi&#x72",
	} {
		if declaresFHIRNamespace([]byte(`<x xmlns="` + v + `"/>`)) {
			t.Errorf("%q: read as the FHIR namespace", v)
		}
	}
}

func TestClassifyBody_Bounds(t *testing.T) {
	big := strings.Repeat("a", 32<<20)
	for name, body := range map[string]string{
		"a huge JSON member":    `{"resourceType":"Patient","text":"` + big + `"}`,
		"a huge XML attribute":  `<Patient xmlns="http://hl7.org/fhir"><id value="` + big + `"/></Patient>`,
		"huge text":             big,
		"JSON nested very deep": `{"resourceType":"Patient","a":` + strings.Repeat("[", 1<<20) + strings.Repeat("]", 1<<20) + `}`,
		"XML nested very deep":  `<Patient xmlns="http://hl7.org/fhir">` + strings.Repeat("<a>", 1<<20) + strings.Repeat("</a>", 1<<20) + `</Patient>`,
		"XML outside the FHIR namespace nested very deep":       `<html>` + strings.Repeat("<a>", 1<<20) + strings.Repeat("</a>", 1<<20) + `</html>`,
		"a JSON array nested very deep":                         strings.Repeat("[", 1<<20) + strings.Repeat("]", 1<<20),
		"many JSON scalars":                                     strings.Repeat("1 ", 8<<20),
		"XML outside the FHIR namespace, unreadable throughout": `<html>` + strings.Repeat("< ", 8<<20) + `</html>`,
		"a broken page of near declarations":                    `<html>&x;` + strings.Repeat(`<a xmlns="http://hl7.org/fhi">`, 1<<20),
		"a broken page with a reference of many zeros":          `<html>&x;<a xmlns="&#x` + strings.Repeat("0", 16<<20) + `68;ttp://hl7.org/fhir">`,
		"XML outside the FHIR namespace, unreadable often":      `<html>` + strings.Repeat("<p>1 < 2</p>", 2<<20) + `</html>`,
		"a huge string after a scalar":                          `1 "` + big + `"`,
		// Bodies of many small JSON values, each read as a value of its
		// own, at the most any body on the leg can be (an answer or a
		// request envelope, 8 MiB): the most values a body can hold.
		"many JSON objects":                 strings.Repeat(`{"error":"x"} `, shnsdk.MaxResponseBytes/14),
		"many empty JSON objects":           strings.Repeat(`{}`, shnsdk.MaxResponseBytes/2),
		"many JSON arrays, then a resource": strings.Repeat("[1]", shnsdk.MaxResponseBytes/3) + patientJSON,
	} {
		start := time.Now()
		_, _, _ = ClassifyBody([]byte(body))
		if d := time.Since(start); d > 20*time.Second {
			t.Errorf("%s: classified in %s", name, d)
		}
	}
}

// A create's or an update's request body is read by its structure alone:
// FHIR XML is the unreadable Subscription (415, fidelity), FHIR JSON of
// another type is refused on authority (403), anything else not one JSON
// Subscription on fidelity (415 or 400).
func TestReadSubscription(t *testing.T) {
	if s, r := ReadSubscription([]byte(baseSubscription)); r != nil || s == nil {
		t.Fatalf("a Subscription: %+v", r)
	}
	if _, r := ReadSubscription([]byte(" \n" + baseSubscription)); r != nil {
		t.Fatalf("a Subscription after whitespace: %+v", r)
	}
	for _, c := range []struct {
		name, body string
		status     int
		primitive  string
	}{
		{"an XML Subscription", `<Subscription xmlns="http://hl7.org/fhir"><status value="requested"/></Subscription>`, http.StatusUnsupportedMediaType, recording.RefusalFidelity},
		{"FHIR XML not well-formed", `<Subscription xmlns="http://hl7.org/fhir">`, http.StatusUnsupportedMediaType, recording.RefusalFidelity},
		{"a JSON Patient", patientJSON, http.StatusForbidden, recording.RefusalAuthority},
		{"a JSON Bundle", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + baseSubscription + `}]}`, http.StatusForbidden, recording.RefusalAuthority},
		{"a JSON Patient after a byte order mark", "\xEF\xBB\xBF" + patientJSON, http.StatusForbidden, recording.RefusalAuthority},
		{"a resourceType that is not a string", `{"resourceType":["Subscription"]}`, http.StatusForbidden, recording.RefusalAuthority},
		{"plain text", "a Subscription", http.StatusUnsupportedMediaType, recording.RefusalFidelity},
		{"HTML", "<html><body>x</body></html>", http.StatusUnsupportedMediaType, recording.RefusalFidelity},
		{"a Subscription root in no namespace", "<Subscription/>", http.StatusUnsupportedMediaType, recording.RefusalFidelity},
		{"a Patient root in no namespace", "<Patient/>", http.StatusUnsupportedMediaType, recording.RefusalFidelity},
		{"no body", "", http.StatusBadRequest, recording.RefusalFidelity},
		{"whitespace only", " \n", http.StatusBadRequest, recording.RefusalFidelity},
		{"JSON naming no resource type", `{"status":"requested"}`, http.StatusBadRequest, recording.RefusalFidelity},
		{"a JSON array holding a Subscription", "[" + baseSubscription + "]", http.StatusForbidden, recording.RefusalAuthority},
		{"a JSON object holding a Patient", `{"error":"x","patient":` + patientJSON + `}`, http.StatusForbidden, recording.RefusalAuthority},
		{"an XML Patient", `<Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient>`, http.StatusForbidden, recording.RefusalAuthority},
		{"a JSON array holding none", `[{"status":"requested"}]`, http.StatusBadRequest, recording.RefusalFidelity},
		{"UTF-16LE JSON without a byte order mark", "{\x00\"\x00", http.StatusBadRequest, recording.RefusalFidelity},
		{"a DOCTYPE with an internal subset", `<!DOCTYPE Subscription [<!ATTLIST Subscription xmlns CDATA #FIXED "http://hl7.org/fhir">]><Subscription/>`, http.StatusUnsupportedMediaType, recording.RefusalFidelity},
		{"truncated JSON", `{"resourceType":"Subscription"`, http.StatusBadRequest, recording.RefusalFidelity},
		{"a Subscription read two ways", `{"resourceType":"Subscription","resourceType":"Patient"}`, http.StatusBadRequest, recording.RefusalFidelity},
		{"a Subscription after a byte order mark", "\xEF\xBB\xBF" + baseSubscription, http.StatusBadRequest, recording.RefusalFidelity},
		{"a resourceType in another case", `{"ResourceType":"Subscription"}`, http.StatusBadRequest, recording.RefusalFidelity},
		{"a FHIR Patient wrapped in an XML element", `<request><Patient xmlns="http://hl7.org/fhir"><id value="p1"/></Patient></request>`, http.StatusForbidden, recording.RefusalAuthority},
		{"a number, then a Subscription", `1 ` + baseSubscription, http.StatusBadRequest, recording.RefusalFidelity},
		// A JSON object or array followed by anything is not one JSON
		// Subscription: refused as before, whatever follows it.
		{"a Subscription, then text", baseSubscription + " x", http.StatusBadRequest, recording.RefusalFidelity},
		{"a Subscription, then a PHP warning", baseSubscription + phpWarning, http.StatusBadRequest, recording.RefusalFidelity},
		{"a JSON Patient, then text", patientJSON + " x", http.StatusBadRequest, recording.RefusalFidelity},
		{"a JSON array holding a Subscription, then text", "[" + baseSubscription + "] x", http.StatusBadRequest, recording.RefusalFidelity},
		{"JSON naming no resource type, then text", `{"status":"requested"} x`, http.StatusBadRequest, recording.RefusalFidelity},
		{"a number, then a JSON object, then text", `1 {"a":1} x`, http.StatusBadRequest, recording.RefusalFidelity},
		{"a number, then a JSON array, then text", `1 [{"a":1}] x`, http.StatusBadRequest, recording.RefusalFidelity},
		{"a number, then two JSON objects", `1 {"a":1} {"b":2}`, http.StatusBadRequest, recording.RefusalFidelity},
		{"a number, then a JSON object", `1 {"a":1}`, http.StatusUnsupportedMediaType, recording.RefusalFidelity},
	} {
		_, r := ReadSubscription([]byte(c.body))
		wantRefusal(t, c.name, r, c.status, c.primitive)
	}
}

// utf16 is s encoded as UTF-16 with its byte order mark, little-endian or
// big-endian.
func utf16(s string, little bool) []byte {
	out := []byte{0xFE, 0xFF}
	if little {
		out = []byte{0xFF, 0xFE}
	}
	for _, r := range s {
		if little {
			out = append(out, byte(r), byte(r>>8))
		} else {
			out = append(out, byte(r>>8), byte(r))
		}
	}
	return out
}

// A body in UTF-16 is never judged without decoding: it is refused as an
// error answer, a 2xx and a request body, whatever it holds.
func TestUTF16FailsClosed(t *testing.T) {
	const xmlPatient = `<Patient xmlns="http://hl7.org/fhir"><name><family value="Doe"/></name></Patient>`
	for _, little := range []bool{true, false} {
		for _, body := range [][]byte{utf16(xmlPatient, little), utf16(patientJSON, little), utf16(answerOutcome, little)} {
			wantRefusal(t, "an error answer in UTF-16", CheckErrorAnswer(body), http.StatusBadGateway, recording.RefusalAuthority)
			for _, in := range []shnsdk.SubscriptionInteraction{shnsdk.SubscriptionRead, shnsdk.SubscriptionCreate, shnsdk.SubscriptionDelete} {
				wantRefusal(t, "a 2xx in UTF-16", CheckSubscribeAnswer(body, in, "sub-1", taskShape), http.StatusBadGateway, recording.RefusalAuthority)
			}
		}
		_, r := ReadSubscription(utf16(baseSubscription, little))
		wantRefusal(t, "a request body in UTF-16", r, http.StatusBadRequest, recording.RefusalFidelity)
	}
}

// A 2xx answer that is no resource itself but holds one is refused, on every
// interaction: the network delivers no resource it did not judge.
func TestCheckSubscribeAnswer_RefusesAResourceInsideAnything(t *testing.T) {
	for _, body := range []string{"[" + patientJSON + "]", `{"error":"x","patient":` + patientJSON + `}`, "\x00{\x00\"\x00r", `<!DOCTYPE Patient [<!ATTLIST Patient xmlns CDATA #FIXED "http://hl7.org/fhir">]><Patient/>`} {
		for _, in := range []shnsdk.SubscriptionInteraction{shnsdk.SubscriptionRead, shnsdk.SubscriptionCreate, shnsdk.SubscriptionUpdate, shnsdk.SubscriptionDelete, shnsdk.SubscriptionSearch, shnsdk.SubscriptionStatus} {
			wantRefusal(t, string(in)+" "+body, CheckSubscribeAnswer([]byte(body), in, "sub-1", taskShape), http.StatusBadGateway, recording.RefusalAuthority)
		}
	}
}

// The FHIR R4 resource types are the FHIR model library's registry, read
// from it: 148 codes of http://hl7.org/fhir/ValueSet/resource-types at R4
// 4.0.1, the abstract Resource and DomainResource among them. A library
// upgrade that changes the list fails here, so the change is seen.
func TestFHIRResourceTypesAreTheLibrarysR4List(t *testing.T) {
	if len(fhirResourceTypes) != 148 {
		t.Fatalf("%d FHIR resource types, want R4's 148", len(fhirResourceTypes))
	}
	for _, name := range []string{"Patient", "Subscription", "OperationOutcome", "Bundle", "Parameters", "EffectEvidenceSynthesis", "Resource", "DomainResource"} {
		if !fhirResourceTypes[name] {
			t.Errorf("%s is not a FHIR resource type", name)
		}
	}
	for _, name := range []string{"SubscriptionTopic", "patient", "html", "<unknown>", ""} {
		if fhirResourceTypes[name] {
			t.Errorf("%q is taken for an R4 resource type", name)
		}
	}
}

// Error answers a provider's system or a proxy before it may send, as
// written: an error in JSON followed by a PHP warning the server printed
// after it, Drupal 7's page naming the XHTML+RDFa DTD (outside the W3C's
// TR tree), an XHTML page naming its DTD by a relative system id, and an
// XML error declared in ISO-8859-2.
const (
	phpWarning          = "<br />\n<b>Warning</b>:  Cannot modify header information - headers already sent in <b>/var/www/proxy.php</b> on line <b>12</b><br />\n"
	phpWarningAfterJSON = `{"error":"not found"}` + phpWarning
	drupalErrorPage     = "<!DOCTYPE html PUBLIC \"-//W3C//DTD XHTML+RDFa 1.0//EN\"\n  \"http://www.w3.org/MarkUp/DTD/xhtml-rdfa-1.dtd\">\n<html xmlns=\"http://www.w3.org/1999/xhtml\" xml:lang=\"en\" version=\"XHTML+RDFa 1.0\" dir=\"ltr\">\n<head><title>Page not found | Patient portal</title></head>\n<body><p>The requested page could not be found.<br></p></body>\n</html>\n"
	relativeDTDPage     = "<!DOCTYPE html PUBLIC \"-//W3C//DTD XHTML 1.0 Strict//EN\" \"DTD/xhtml1-strict.dtd\">\n<html xmlns=\"http://www.w3.org/1999/xhtml\"><body><p>Not Found</p></body></html>"
	latin2Error         = "<?xml version=\"1.0\" encoding=\"ISO-8859-2\"?><error><code>404</code><message>Nie znaleziono zasobu \xb3</message></error>"
)

// A DOCTYPE's internal subset is a "[" outside its literals; a literal that
// is not closed cannot be read, and is taken for one.
func TestInternalSubset(t *testing.T) {
	for d, want := range map[string]bool{
		`DOCTYPE html`:                                 false,
		`DOCTYPE x SYSTEM "a[b.dtd"`:                   false,
		`DOCTYPE x PUBLIC '-//X[//EN' "y.dtd"`:         false,
		`DOCTYPE x [<!ENTITY a "b">]`:                  true,
		`DOCTYPE x SYSTEM "a[b.dtd" [<!ENTITY a "b">]`: true,
		`DOCTYPE x SYSTEM "a.dtd`:                      true,
		`DOCTYPE x SYSTEM 'a.dtd" [`:                   true,
	} {
		if got := internalSubset([]byte(d)); got != want {
			t.Errorf("%q: %v, want %v", d, got, want)
		}
	}
}
