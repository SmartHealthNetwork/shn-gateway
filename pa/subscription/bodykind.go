package subscription

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/samply/golang-fhir-models/fhir-models/fhir"
)

// BodyKind is what a body is, decided by its structure alone and never by the
// media type it is labelled with (ClassifyBody).
type BodyKind int

const (
	// BodyEmpty is a body of no bytes, or of whitespace only (space, tab,
	// CR and LF), after an optional UTF-8 byte order mark.
	BodyEmpty BodyKind = iota + 1
	// BodyNotFHIR is a body that is neither FHIR JSON nor FHIR XML: an HTML
	// page, plain text, a JSON value that holds no resource, XML whose root
	// is in no namespace or another and that holds no element in the FHIR
	// namespace.
	BodyNotFHIR
	// BodyFHIRJSON is a JSON object with a resourceType member, its name
	// compared folded, as this package's scans compare names.
	BodyFHIRJSON
	// BodyFHIRXML is an XML document whose root element is in the FHIR
	// namespace (FHIRNamespace), or whose root is outside it but that holds
	// an element in it (resource type "").
	BodyFHIRXML
)

func (k BodyKind) String() string {
	switch k {
	case BodyEmpty:
		return "empty"
	case BodyNotFHIR:
		return "not-fhir"
	case BodyFHIRJSON:
		return "fhir-json"
	case BodyFHIRXML:
		return "fhir-xml"
	}
	return "unknown"
}

// FHIRNamespace is the XML namespace of every FHIR resource element.
const FHIRNamespace = "http://hl7.org/fhir"

// ErrBodyUnreadable is a body ClassifyBody takes for FHIR but cannot read one
// way only (ClassifyBody lists each case): JSON that is not one well-formed,
// unambiguous value within the depth bound, or that a run of scalars puts
// before FHIR JSON; XML whose FHIR root is not well-formed or nests past the
// depth bound, whose root is a FHIR resource type's name outside the
// namespace, whose DOCTYPE holds an internal subset or names a DTD it cannot
// read, whose declared encoding does not read ASCII as ASCII, or that may
// declare the FHIR namespace where it cannot be read one way only; and any
// body in UTF-16 or UTF-32, by a byte order mark or a NUL byte. Its kind is
// the one it is taken for, so a caller refuses it as that kind.
var ErrBodyUnreadable = errors.New("subscription: the body is taken for FHIR but cannot be read one way only")

// fhirResourceTypes are FHIR R4's resource types by name, as the FHIR model
// library the gateway builds FHIR with registers them (fhir.ResourceType, the
// codes of http://hl7.org/fhir/ValueSet/resource-types, generated from the R4
// 4.0.1 definitions), read from it and never typed here. Its codes are
// contiguous from 0; the first that has none ends them.
var fhirResourceTypes = func() map[string]bool {
	m := map[string]bool{}
	for t := fhir.ResourceType(0); t.Code() != "<unknown>"; t++ {
		m[t.Code()] = true
	}
	return m
}()

// ebcdicXMLDeclaration is "<?xm" in EBCDIC, as XML's own encoding detection
// reads it.
var ebcdicXMLDeclaration = []byte{0x4C, 0x6F, 0xA7, 0x94}

// utf8BOM is the UTF-8 byte order mark a lenient reader skips.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// wideBOMs are the UTF-32 and UTF-16 byte order marks, UTF-32's first since
// UTF-32LE's begins with UTF-16LE's: a body that begins with one cannot be
// judged without decoding it, and a reader that decodes it may take it for a
// resource.
var wideBOMs = [][]byte{{0x00, 0x00, 0xFE, 0xFF}, {0xFF, 0xFE, 0x00, 0x00}, {0xFE, 0xFF}, {0xFF, 0xFE}}

// ClassifyBody is the one structural reading of a body every check on a
// subscribe leg shares, the error answer's, the 2xx answer's and the request's,
// exported for every topic's lane. It reads the body after an optional UTF-8
// byte order mark and any leading whitespace, as a lenient reader takes it:
//   - nothing left: BodyEmpty;
//   - '<': BodyFHIRXML when it is an XML document whose root element is in the
//     FHIR namespace, resourceType the root's local name; BodyNotFHIR when no
//     root element can be read (an HTML page that is not well-formed XML) or
//     the root is outside the FHIR namespace with a name that is no FHIR
//     resource type (html, patient, a SOAP Envelope) and no element in the
//     FHIR namespace below or after it; BodyFHIRXML with resourceType "" when
//     such a root holds an element in the FHIR namespace (the namespace is
//     FHIR XML's marker, as resourceType is JSON's, and a name alone never
//     is), and with ErrBodyUnreadable when it declares the FHIR namespace
//     where no reader can be relied on to read it one way: in a document that
//     is not well-formed, or inside a comment, a processing instruction, a
//     directive or a CDATA section that holds markup (declaresFHIRNamespace:
//     no element is in the namespace without a declaration); and BodyFHIRXML with
//     ErrBodyUnreadable when a root in the FHIR namespace starts a document
//     that is not well-formed, or nests past the depth bound, and when a
//     DOCTYPE before the root holds an internal subset ([…]), which a reader
//     that applies it may use to put the root in the FHIR namespace
//     (resourceType ""), or names a DTD other than the W3C's HTML and XHTML
//     ones (knownDTD) in a document that declares the FHIR namespace
//     anywhere (declaresFHIRNamespace), which a reader that fetches the DTD
//     may use the same way (resourceType ""), and when the root's local name is a FHIR R4 resource
//     type, case-exact (fhirResourceTypes), outside the FHIR namespace (in
//     none, another, or under a prefix bound to none), which a reader that
//     ignores namespaces takes for FHIR (resourceType that name). Only element
//     names are looked at; no FHIR XML is parsed;
//   - anything else is read as JSON values in sequence (classifyJSON): the
//     first value's kind, BodyFHIRJSON when it is an object with a
//     resourceType member (name compared folded; resourceType its string
//     value, "" when it is not a string) or an object or array holding a
//     resource anywhere (a contained member, or an object with a resourceType
//     below it: holdsResource; resourceType ""), and BodyNotFHIR otherwise
//     (a scalar, a proxy's {"error":"bad gateway"}, plain text, a status
//     line such as "404 Not Found"); BodyFHIRJSON with ErrBodyUnreadable when
//     a value cannot be read one way only (a repeated member name, nesting
//     past the depth bound however deep), when FHIR JSON follows the first
//     value, or when text that is not a JSON value begins with '{'. What
//     follows the values, when it is not a JSON value, is text, never judged.
//
// A body that begins with a UTF-16 or UTF-32 byte order mark, or holds a NUL
// byte anywhere (JSON and XML text in UTF-8 hold none; UTF-16 and UTF-32
// text, which a JSON or XML reader detects without a byte order mark, holds
// them), is not read at all: it is BodyFHIRJSON with ErrBodyUnreadable, since
// what it holds is unknown and unknown is never taken for empty or for not
// FHIR.
//
// A body's media type is never read. Its cost is linear in its length, and
// its nesting is bounded by the scanner's depth bound.
func ClassifyBody(body []byte) (kind BodyKind, resourceType string, err error) {
	for _, bom := range wideBOMs {
		if bytes.HasPrefix(body, bom) {
			return BodyFHIRJSON, "", ErrBodyUnreadable
		}
	}
	if bytes.IndexByte(body, 0) >= 0 {
		return BodyFHIRJSON, "", ErrBodyUnreadable
	}
	// An XML declaration in EBCDIC ("<?xm"), which an XML reader detects
	// without a byte order mark: what follows is unknown in these bytes.
	if bytes.HasPrefix(body, ebcdicXMLDeclaration) {
		return BodyFHIRXML, "", ErrBodyUnreadable
	}
	b := bytes.TrimLeft(bytes.TrimPrefix(body, utf8BOM), " \t\r\n")
	switch {
	case len(b) == 0:
		return BodyEmpty, "", nil
	case b[0] == '<':
		x, err := readXML(b)
		switch {
		case x.subset, x.encoding:
			return BodyFHIRXML, "", ErrBodyUnreadable
		case x.dtd && declaresFHIRNamespace(b):
			// An external DTD can default an xmlns attribute onto an
			// element for a reader that fetches it: beside a declaration of
			// the FHIR namespace, what such a reader sees cannot be read one
			// way only. A document that declares the namespace nowhere is
			// judged as if it named no DTD.
			return BodyFHIRXML, "", ErrBodyUnreadable
		case x.outside:
			return BodyFHIRXML, x.root, ErrBodyUnreadable
		case x.fhir && err != nil:
			return BodyFHIRXML, x.root, ErrBodyUnreadable
		case x.fhir:
			return BodyFHIRXML, x.root, nil
		case x.fhirBelow:
			return BodyFHIRXML, "", nil
		case x.hidden, err != nil && declaresFHIRNamespace(b):
			return BodyFHIRXML, "", ErrBodyUnreadable
		}
		return BodyNotFHIR, "", nil
	}
	return classifyJSON(b)
}

// classifyJSON classifies b, a body that begins with neither '<' nor
// nothing, as the run of JSON values a decoder reads from its start, ending
// at its end or at the first text that is not a JSON value, which is never
// judged ("404 Not Found" is the number 404, then text):
//   - the body's kind and resource type are its first value's, judged as
//     that value alone (classifyJSONValue): a scalar (a string, a number,
//     true, false or null) is not FHIR, nor is an object or array holding
//     no resource;
//   - any value after the first that is FHIR JSON, or that cannot be read
//     one way only, makes the body FHIR JSON with ErrBodyUnreadable, since a
//     reader that decodes values in sequence may take it for a resource, as
//     does a first value that cannot be read so: "1 {"resourceType":"Patient"}"
//     and "[{}] [{"resourceType":"Patient"}]" are unreadable, while
//     "404 {"error":"x"}", "[404] Not Found" and an error object followed by
//     a server's warning in HTML are not FHIR, and an OperationOutcome
//     followed by such a warning is judged as that OperationOutcome;
//   - text that begins with '{' and decodes as no JSON value is FHIR JSON's
//     own first byte, which cannot be read: unreadable; text that begins
//     with '[' is unreadable when it nests past the depth bound however deep
//     (nestsPastBound: a decoder's own limit, encoding/json's 10000, is no
//     reason to read a body as text), and otherwise text, never judged
//     ("[error] upstream timed out").
//
// One decoder reads the run, and each value is scanned once, so its cost is
// linear in b's length.
func classifyJSON(b []byte) (BodyKind, string, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	kind, rt := BodyNotFHIR, ""
	for first := true; ; first = false {
		rest := bytes.TrimLeft(b[d.InputOffset():], " \t\r\n")
		if len(rest) == 0 {
			return kind, rt, nil
		}
		start := len(b) - len(rest)
		if d.Decode(&skipJSONValue{}) != nil {
			if rest[0] == '{' || (rest[0] == '[' && nestsPastBound(rest)) {
				return BodyFHIRJSON, "", ErrBodyUnreadable
			}
			return kind, rt, nil
		}
		vk, vrt, err := classifyJSONValue(b[start:d.InputOffset()])
		switch {
		case err != nil, vk == BodyFHIRJSON && !first:
			return BodyFHIRJSON, "", ErrBodyUnreadable
		case first:
			kind, rt = vk, vrt
		}
	}
}

// skipJSONValue is a value a decoder reads and checks as JSON, keeping
// nothing: classifyJSON reads the value's bytes from the body itself.
type skipJSONValue struct{}

func (*skipJSONValue) UnmarshalJSON([]byte) error { return nil }

// classifyJSONValue classifies raw, one JSON value a decoder read: an object
// with a resourceType member (name compared folded) is FHIR JSON of that
// type ("" when it is not a string); an object or array that holds a
// resource anywhere (a contained member, or an object with a resourceType
// below it: holdsResource) is FHIR JSON with resourceType ""; anything else
// is not FHIR; and a value this network cannot read one way only (a repeated
// member name, bytes that are not UTF-8, nesting past the depth bound) is
// ErrBodyUnreadable.
func classifyJSONValue(raw []byte) (BodyKind, string, error) {
	if raw[0] != '{' && raw[0] != '[' {
		return BodyNotFHIR, "", nil
	}
	root, err := scanDecodedJSON(raw)
	if err != nil {
		return BodyFHIRJSON, "", ErrBodyUnreadable
	}
	if rt := root.memberFolded("resourceType"); rt != nil {
		s, _ := rt.stringOf()
		return BodyFHIRJSON, s, nil
	}
	if holdsResource(root, true) {
		return BodyFHIRJSON, "", nil
	}
	return BodyNotFHIR, "", nil
}

// firstJSONValue is the first JSON value in body, after an optional UTF-8
// byte order mark and whitespace: the value whose kind ClassifyBody gives a
// body of JSON values. It is body itself when no value decodes from it.
func firstJSONValue(body []byte) []byte {
	b := bytes.TrimPrefix(body, utf8BOM)
	var raw json.RawMessage
	if json.NewDecoder(bytes.NewReader(b)).Decode(&raw) != nil {
		return b
	}
	return raw
}

// jsonValueThenMore reports whether body, after an optional UTF-8 byte
// order mark and whitespace, is a JSON object or array, after any run of
// JSON scalars, followed by anything but whitespace: never one JSON value,
// whatever its values are. A request body and a 2xx answer that is one are
// refused as not one JSON object (ReadSubscription, AnswerSubscriptions).
func jsonValueThenMore(body []byte) bool {
	b := bytes.TrimPrefix(body, utf8BOM)
	d := json.NewDecoder(bytes.NewReader(b))
	for {
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return false
		}
		if raw[0] == '{' || raw[0] == '[' {
			return len(bytes.TrimLeft(b[d.InputOffset():], " \t\r\n")) > 0
		}
	}
}

// nestsPastBound reports whether b, read as JSON text, nests arrays and
// objects past the depth bound anywhere: brackets and braces outside strings
// are counted, a string's escapes skipped. It reads b once.
func nestsPastBound(b []byte) bool {
	depth, inString := 0, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inString && c == '\\':
			i++
		case inString && c == '"':
			inString = false
		case inString:
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			if depth++; depth > maxDepth {
				return true
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return false
}

// xmlBody is what readXML reads of an XML document: whether a DOCTYPE before
// its root holds an internal subset, whether its root has a FHIR resource
// type's name outside the FHIR namespace, whether its root element is in the
// FHIR namespace, the root's local name, and whether an element below the
// root holds a resource: one named contained, in any case (the FHIR XML form
// of a contained resource), or named, case-exact, as a FHIR resource type;
// and, under a root outside the FHIR namespace, whether any element is in
// the FHIR namespace (fhirBelow), and whether markup another reader may read
// as elements (a comment, a processing instruction, a directive, a CDATA
// section) declares the FHIR namespace (hidden).
type xmlBody struct {
	subset    bool
	outside   bool
	fhir      bool
	root      string
	resource  bool
	fhirBelow bool
	hidden    bool
	encoding  bool
	dtd       bool
	// bundleType is the value of the root's own type child; notAck is set
	// when the document is no acknowledgement Bundle's shape (xmlAckBundle):
	// a resource anywhere but the one OperationOutcome of an entry's
	// response's outcome, or an entry holding more than its response.
	bundleType string
	notAck     bool
}

// namedXMLNS reports whether e is an element named xmlns with no prefix,
// which the namespaces spec reserves and readers resolve differently (Go's
// leaves it in no namespace, others in the default one).
func namedXMLNS(e xml.StartElement) bool { return e.Name.Local == "xmlns" && e.Name.Space == "" }

// knownDTD reports whether a DOCTYPE directive d names no DTD a reader can
// fetch, or only the W3C's HTML or XHTML DTDs: no external id ("DOCTYPE
// html"); a public id alone, with no system literal (an HTML page's, such as
// Apache httpd's "-//IETF//DTD HTML 2.0//EN": an HTML reader fetches no DTD,
// and no XML reader reads such a document); SYSTEM "about:legacy-compat"
// (HTML5's, which names nothing to fetch); or a system id under
// http(s)://www.w3.org/TR/. Any other system id is a DTD this network cannot
// read without fetching it, which may put an element in the FHIR namespace
// for a reader that does: ClassifyBody refuses it beside a declaration of
// that namespace alone.
func knownDTD(d []byte) bool {
	f := bytes.Fields(d)
	if len(f) < 3 {
		return true
	}
	var quoted [][]byte
	for rest := d; ; {
		i := bytes.IndexAny(rest, `"'`)
		if i < 0 {
			break
		}
		j := bytes.IndexByte(rest[i+1:], rest[i])
		if j < 0 {
			return false
		}
		quoted, rest = append(quoted, rest[i+1:i+1+j]), rest[i+2+j:]
	}
	switch kw := strings.ToUpper(string(f[2])); {
	case kw == "PUBLIC" && len(quoted) == 1:
		return true
	case kw == "PUBLIC" && len(quoted) == 2:
		return w3cSystemID(string(quoted[1]))
	case kw == "SYSTEM" && len(quoted) == 1:
		return w3cSystemID(string(quoted[0]))
	}
	return false
}

// w3cSystemID reports whether s is a system id naming the W3C's HTML or
// XHTML DTDs (under the W3C's TR tree) or HTML5's about:legacy-compat.
func w3cSystemID(s string) bool {
	return s == "about:legacy-compat" || strings.HasPrefix(s, "http://www.w3.org/TR/") || strings.HasPrefix(s, "https://www.w3.org/TR/")
}

// internalSubset reports whether a DOCTYPE directive d holds an internal
// subset: a "[" outside its quoted literals, which may hold any character
// (a Shift_JIS character's trail byte may read as "[", and a system id may
// name one). A literal that is not closed cannot be read, and is taken for
// a subset.
func internalSubset(d []byte) bool {
	for i := 0; i < len(d); i++ {
		switch c := d[i]; c {
		case '"', '\'':
			j := bytes.IndexByte(d[i+1:], c)
			if j < 0 {
				return true
			}
			i += j + 1
		case '[':
			return true
		}
	}
	return false
}

// repeatsAttribute reports whether e names one attribute twice, a document
// no conforming reader reads (and readers that recover from it disagree on
// which one holds).
func repeatsAttribute(e xml.StartElement) bool {
	seen := make(map[xml.Name]bool, len(e.Attr))
	for _, a := range e.Attr {
		if seen[a.Name] {
			return true
		}
		seen[a.Name] = true
	}
	return false
}

// readXML reads b's root element and the names of the elements below it, and
// the rest of the document only to find it well-formed (each element closed, one root,
// nothing but whitespace, comments and processing instructions after it)
// within the depth bound. Before the root it skips the XML declaration,
// processing instructions, comments, a DOCTYPE and whitespace; any other
// text, or a token that cannot be read, there means no root; the caller
// judges such a document by whether it may declare the FHIR namespace. A
// declared encoding is read only when the registry names it
// (xmlEncodingLabels; otherwise encoding), and, other than UTF-8, for its
// markup alone (markupBytes); a DOCTYPE naming a DTD other than the W3C's
// HTML and XHTML ones is noted (dtd) and read past; and an element that
// names an attribute twice is an error (repeatsAttribute). An error after a
// root in the FHIR namespace is the document's.
func readXML(b []byte) (xmlBody, error) {
	var x xmlBody
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	// A declared encoding is read only when the registry names it, each
	// one reading ASCII as ASCII, so the document's markup is where its
	// bytes show it; any other (UTF-7, EBCDIC, ISO-2022-JP, a label no
	// registry names) puts it where these bytes may not, so what the
	// document holds is unknown (encoding). The decoder asks only for a
	// label other than UTF-8's own.
	d.CharsetReader = func(label string, r io.Reader) (io.Reader, error) {
		switch name, ok := xmlEncodingLabels[strings.ToLower(strings.Trim(label, " \t\n\r\f"))]; {
		case !ok:
			x.encoding = true
			return nil, errors.New("a declared encoding the registry does not read")
		case name == "utf-8":
			return r, nil
		}
		return markupBytes{r}, nil
	}
	// markup notes a token that is markup holding markup (a comment, a
	// processing instruction, a directive or a CDATA section with a "<"
	// inside), which another reader may read as elements, and that declares
	// the FHIR namespace.
	markup := func(before int64) {
		if raw := b[before:d.InputOffset()]; len(raw) > 1 && raw[0] == '<' && bytes.IndexByte(raw[1:], '<') >= 0 && declaresFHIRNamespace(raw) {
			x.hidden = true
		}
	}
prolog:
	for {
		before := d.InputOffset()
		tok, err := d.Token()
		if err != nil {
			return x, err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if len(bytes.TrimLeft(t, " \t\r\n")) != 0 {
				return x, errors.New("text before the root")
			}
		case xml.Directive:
			if f := bytes.Fields(t); len(f) > 0 && strings.EqualFold(string(f[0]), "DOCTYPE") {
				if internalSubset(t) {
					x.subset = true
					return x, errors.New("a DOCTYPE with an internal subset")
				}
				// An external DTD can default an xmlns attribute onto an
				// element for a reader that fetches it: any but the W3C's
				// HTML and XHTML DTDs, which declare no FHIR namespace, is
				// noted, and refuses the document beside a declaration of
				// that namespace alone (ClassifyBody).
				if !knownDTD(t) {
					x.dtd = true
				}
			}
			markup(before)
		case xml.ProcInst, xml.Comment:
			markup(before)
		case xml.StartElement:
			if repeatsAttribute(t) || namedXMLNS(t) {
				return x, errors.New("an attribute named twice, or an element named xmlns")
			}
			if t.Name.Space != FHIRNamespace && fhirResourceTypes[t.Name.Local] {
				x.outside, x.root = true, t.Name.Local
				return x, errors.New("a FHIR resource type's root outside the FHIR namespace")
			}
			if t.Name.Space != FHIRNamespace {
				// A root outside the FHIR namespace: the whole rest is read
				// as strictly as a FHIR root's, for an element in the
				// namespace and markup naming it.
				for {
					before := d.InputOffset()
					tok, err := d.Token()
					if err == io.EOF {
						return x, nil
					}
					if err != nil {
						return x, err
					}
					switch t := tok.(type) {
					case xml.StartElement:
						if repeatsAttribute(t) || namedXMLNS(t) {
							return x, errors.New("an attribute named twice, or an element named xmlns")
						}
						if t.Name.Space == FHIRNamespace {
							x.fhirBelow = true
							return x, nil
						}
					case xml.CharData, xml.Comment, xml.ProcInst, xml.Directive:
						markup(before)
					}
				}
			}
			x.fhir, x.root = true, t.Name.Local
			break prolog
		default:
			return x, errors.New("not a document")
		}
	}
	// path is the names of the elements open below the root, each in the
	// FHIR namespace (one in another namespace is named so that no FHIR path
	// matches it); types counts the root's own type children.
	var path []string
	types := 0
	for depth := 1; depth > 0; {
		tok, err := d.Token()
		if err != nil {
			return x, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if repeatsAttribute(t) || namedXMLNS(t) {
				return x, errors.New("an attribute named twice, or an element named xmlns")
			}
			isResource := strings.EqualFold(t.Name.Local, "contained") || fhirResourceTypes[t.Name.Local]
			if isResource {
				x.resource = true
			}
			name := t.Name.Local
			if t.Name.Space != FHIRNamespace {
				name = "\x00" + name
			}
			switch {
			case len(path) == 0 && name == "type":
				if types++; types > 1 {
					x.notAck = true
				}
				for _, a := range t.Attr {
					if a.Name.Local == "value" && a.Name.Space == "" {
						x.bundleType = a.Value
					}
				}
			case len(path) == 1 && path[0] == "entry" && name != "response":
				x.notAck = true
			}
			if isResource && !(name == "OperationOutcome" && slices.Equal(path, []string{"entry", "response", "outcome"})) {
				x.notAck = true
			}
			path = append(path, name)
			depth++
			if depth > maxDepth {
				return x, errors.New("nested past the depth bound")
			}
		case xml.EndElement:
			if len(path) > 0 {
				path = path[:len(path)-1]
			}
			depth--
		}
	}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return x, nil
		}
		if err != nil {
			return x, err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if len(bytes.TrimLeft(t, " \t\r\n")) != 0 {
				return x, errors.New("text after the root")
			}
		case xml.ProcInst, xml.Comment:
		default:
			return x, errors.New("more than one root")
		}
	}
}

// xmlHoldsResource reports whether an XML document holds a resource below its
// root, at any depth within the depth bound: an element named contained, in
// any case, or one named, case-exact, as a FHIR resource type, judged as
// JSON's check judges a resource anywhere.
func xmlHoldsResource(x xmlBody) bool { return x.resource }

// declaresFHIRNamespace reports whether b may declare the FHIR namespace,
// http://hl7.org/fhir, as a reader that recovers from errors reads it: an
// attribute named xmlns or xmlns:<prefix> in a tag, whose value, read as an
// XML reader reads it (character references with any number of leading
// zeros, and the five predefined entities), is exactly the namespace. It is
// read for a document a conforming reader refuses, whose tags such a reader
// can only guess at, so it guesses wide: a tag opens at "<" and a name's first
// character, and ends only at a ">" outside a value quoted after "=", and an
// xmlns anywhere inside it counts, inside a value too. So a stray quote, or a
// ">" in a quoted value, hides no declaration. Text that
// only contains the namespace, a link or markup written escaped
// (&lt;Patient xmlns=…&gt;), is in no tag by either reading, and declares
// nothing. A value is quoted or not; it is read only while it still matches
// the namespace, so b is read in time linear in its length.
func declaresFHIRNamespace(b []byte) bool {
	const name = "xmlns"
	var (
		inTag   = false // in a tag ...
		afterEq = false // ... after "=", awaiting a value
		quote   byte    // ... inside a value quoted with quote
	)
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !inTag && c == '<' && i+1 < len(b) && isNameStart(b[i+1]) {
			inTag, afterEq, quote = true, false, 0
			continue
		}
		if inTag {
			switch {
			case quote != 0:
				if c == quote {
					quote = 0
				}
			case c == '=':
				afterEq = true
			case (c == '"' || c == '\'') && afterEq:
				quote, afterEq = c, false
			case c == '>':
				inTag, afterEq = false, false
			case !isXMLSpace(c):
				afterEq = false
			}
		}
		if inTag && c == 'x' && bytes.HasPrefix(b[i:], []byte(name)) && (i == 0 || !isNameChar(b[i-1])) && namespaceDeclared(b, i+len(name)) {
			return true
		}
	}
	return false
}

// namespaceDeclared reports whether what follows an attribute named xmlns at
// k in b (an optional ":<prefix>", "=", and a value, quoted or not, with
// whitespace around "=") is a value that reads as exactly the FHIR namespace.
func namespaceDeclared(b []byte, k int) bool {
	if k < len(b) && b[k] == ':' {
		for k++; k < len(b) && isNameChar(b[k]); k++ {
		}
	}
	for ; k < len(b) && isXMLSpace(b[k]); k++ {
	}
	if k >= len(b) || b[k] != '=' {
		return false
	}
	for k++; k < len(b) && isXMLSpace(b[k]); k++ {
	}
	var quote byte
	if k < len(b) && (b[k] == '"' || b[k] == '\'') {
		quote = b[k]
		k++
	}
	ns := FHIRNamespace
	for n := 0; ; n++ {
		if k >= len(b) {
			return quote == 0 && n == len(ns)
		}
		if c := b[k]; (quote != 0 && c == quote) || (quote == 0 && (isXMLSpace(c) || c == '>')) {
			return n == len(ns)
		}
		r, next := rune(b[k]), k+1
		if b[k] == '&' {
			if ref, end, ok := decodeReference(b, k); ok {
				r, next = ref, end
			}
		}
		if n >= len(ns) || r != rune(ns[n]) {
			return false
		}
		k = next
	}
}

// decodeReference reads the character reference or predefined entity at
// b[k] ("&"): the character it names and the index just past its ";". A
// numeric reference's leading zeros are skipped however many; a reference
// that names no character is not one (ok false). It reads only the
// reference's own bytes.
func decodeReference(b []byte, k int) (rune, int, bool) {
	j := k + 1
	if j < len(b) && b[j] == '#' {
		j++
		base := 10
		if j < len(b) && (b[j] == 'x') {
			base, j = 16, j+1
		}
		for ; j < len(b) && b[j] == '0'; j++ {
		}
		start := j
		for ; j < len(b) && j-start <= 8 && isRefDigit(b[j], base); j++ {
		}
		if j >= len(b) || b[j] != ';' {
			return 0, 0, false
		}
		n := uint64(0)
		if j > start {
			v, err := strconv.ParseUint(string(b[start:j]), base, 32)
			if err != nil {
				return 0, 0, false
			}
			n = v
		}
		if n == 0 || n > utf8.MaxRune {
			return 0, 0, false
		}
		return rune(n), j + 1, true
	}
	end := bytes.IndexByte(b[j:min(len(b), j+5)], ';')
	if end < 0 {
		return 0, 0, false
	}
	r, ok := xmlReference(string(b[j : j+end]))
	return r, j + end + 1, ok
}

func isRefDigit(c byte, base int) bool {
	if c >= '0' && c <= '9' {
		return true
	}
	return base == 16 && (c|0x20) >= 'a' && (c|0x20) <= 'f'
}

func isXMLSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// isNameStart reports whether c can begin an XML name as a tag writes it: a
// letter, "_" or ":", or a byte of a character beyond ASCII.
func isNameStart(c byte) bool {
	return c == '_' || c == ':' || c >= 0x80 || (c|0x20) >= 'a' && (c|0x20) <= 'z'
}

// isNameChar reports whether c can continue an XML name.
func isNameChar(c byte) bool {
	return isNameStart(c) || c == '-' || c == '.' || c >= '0' && c <= '9'
}

// xmlReference is the character a predefined entity names, ref being what
// lies between "&" and ";".
func xmlReference(ref string) (rune, bool) {
	switch ref {
	case "amp":
		return '&', true
	case "lt":
		return '<', true
	case "gt":
		return '>', true
	case "quot":
		return '"', true
	case "apos":
		return '\'', true
	}
	return 0, false
}
