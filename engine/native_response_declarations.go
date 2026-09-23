package engine

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"
)

// NativeResponseDeclarations is an immutable set of independently established
// response contracts, bound to exact operation and selected endpoint URLs. Its
// zero value supplies no response declaration. Receive capabilities and request
// endpoint versions never establish an output contract.
type NativeResponseDeclarations struct{ bindings map[string]map[string]string }

// ParseNativeResponseDeclarations parses a bounded JSON array of operation,
// endpoint and contractVersion strings. It validates configuration only, never
// payloads or support for a contract line. Errors do not include supplied values.
func ParseNativeResponseDeclarations(raw string) (NativeResponseDeclarations, error) {
	fail := func() (NativeResponseDeclarations, error) {
		return NativeResponseDeclarations{}, errors.New("native response declarations: invalid configuration")
	}
	if len(raw) > 32768 || !utf8.ValidString(raw) {
		return fail()
	}
	if strings.TrimSpace(raw) == "" {
		return NativeResponseDeclarations{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('[') {
		return fail()
	}
	bindings := map[string]map[string]string{}
	count := 0
	for dec.More() {
		count++
		if count > 32 {
			return fail()
		}
		token, err = dec.Token()
		if err != nil || token != json.Delim('{') {
			return fail()
		}
		fields := map[string]string{}
		for dec.More() {
			token, err = dec.Token()
			if err != nil {
				return fail()
			}
			key, ok := token.(string)
			if !ok {
				return fail()
			}
			if key != "operation" && key != "endpoint" && key != "contractVersion" {
				return fail()
			}
			if _, exists := fields[key]; exists {
				return fail()
			}
			token, err = dec.Token()
			if err != nil {
				return fail()
			}
			value, ok := token.(string)
			if !ok {
				return fail()
			}
			fields[key] = value
		}
		token, err = dec.Token()
		if err != nil || token != json.Delim('}') || len(fields) != 3 {
			return fail()
		}
		op, endpoint, version := fields["operation"], fields["endpoint"], fields["contractVersion"]
		contract := nativeResponseOperationContract(op)
		if contract == "" || !validNativeContractToken(version, contract) || len(endpoint) > 2048 {
			return fail()
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || strings.Contains(endpoint, "#") {
			return fail()
		}
		if bindings[op] == nil {
			bindings[op] = map[string]string{}
		}
		if _, exists := bindings[op][endpoint]; exists {
			return fail()
		}
		bindings[op][endpoint] = version
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim(']') {
		return fail()
	}
	if _, err = dec.Token(); err != io.EOF {
		return fail()
	}
	if count == 0 {
		return NativeResponseDeclarations{}, nil
	}
	return NativeResponseDeclarations{bindings: bindings}, nil
}

// WithNativeResponseDeclarations configures output declarations independently
// of accepted request representations. A binding never selects a route. Without
// an exact operation/URL binding, the selected answer has no output declaration.
func WithNativeResponseDeclarations(d NativeResponseDeclarations) NativeOption {
	return func(n *nativeResponder) { n.responseDeclarations = d }
}

func nativeResponseOperationContract(op string) string {
	switch op {
	case "pas-submit", "pas-update-submit", "pas-inquire":
		return "pa.pas"
	case "questionnaire-package", "next-question":
		return "pa.dtr"
	case "crd-order-select", "crd-order-dispatch":
		return "pa.crd"
	}
	return ""
}
func nativeResponseOperation(leg, path string) string {
	switch leg {
	case "pas-claim":
		return "pas-submit"
	case "pas-claim-update":
		return "pas-update-submit"
	case "pas-claim-inquire":
		return "pas-inquire"
	case "crd-order-select", "crd-order-dispatch":
		return leg
	case "dtr-questionnaire-fetch":
		path, _, _ = strings.Cut(path, "?")
		switch path {
		case "/Questionnaire/$questionnaire-package":
			return "questionnaire-package"
		case "/Questionnaire/$next-question":
			return "next-question"
		}
	}
	return ""
}
