package relay

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The inputs the created Task's Location (E-16) is made from (HeaderInputs):
// the base it is written under, and the CDex Task location that follows it.
// The edit's row declares them (Edit.HeaderInputs); the workstream that makes
// the edit decides which base, and the core checks the value against both.
const (
	TaskLocationInputBase = "base"
	TaskLocationInputTail = "tail"
)

// headerVerifier is the core's check of a header edit's value: the names of
// the inputs it is made from, space-separated, and the check of the value
// against them. The names are a string, which cannot be written through, so
// a read of the map (a copy of the entry) shares nothing a write could reach;
// names returns a slice of its own.
type headerVerifier struct {
	inputs string
	verify func(value string, in HeaderInputs) error
}

// names returns the names of the inputs v's check reads, in order, in a slice
// of the caller's own.
func (v headerVerifier) names() []string { return strings.Fields(v.inputs) }

// headerVerifiers are the core's checks of each header edit, by its id: a
// closed map, as adaptiveEdits is. A header edit is made only with its check
// here (ValidateEdits, HeaderEdited); no row and no caller supplies one.
var headerVerifiers = map[EditID]headerVerifier{
	EditCDexTaskLocationRekey: {inputs: TaskLocationInputBase + " " + TaskLocationInputTail, verify: verifyTaskLocation},
}

// verifyTaskLocation is E-16's check: value is exactly the base input, "/",
// then the tail input, where the tail is a CDex Task location
// (shnsdk.ParseCDexTaskLocation: "Task/{id}" or "Task/{id}/_history/{vid}")
// spelled as it reads back, and the base is an absolute http or https URL with
// a host and no user information, query, fragment, trailing slash or dot
// segment, in ASCII. Neither may hold whitespace or a control character, which
// url.Parse lets through.
func verifyTaskLocation(value string, in HeaderInputs) error {
	base, tail := in[TaskLocationInputBase], in[TaskLocationInputTail]
	if err := checkLocationBase(base); err != nil {
		return err
	}
	loc, err := shnsdk.ParseCDexTaskLocation(tail)
	if err != nil {
		return fmt.Errorf("the tail is not a Task location: %v", err)
	}
	if loc.String() != tail {
		return errors.New("the tail is not spelled as a Task location reads back")
	}
	if value != base+"/"+tail {
		return errors.New("the value is not the base, then the Task location")
	}
	return nil
}

// URLCharacter reports whether c is in RFC 3986's set of URL characters:
// unreserved, reserved, or the "%" of a percent-encoding. A backslash, a
// quote, "<", ">", "{", "}", "|", "^" and a backtick are not: url.Parse lets
// them through in a path, and a client parsing by WHATWG's rules reads a
// backslash as "/". The payer gateway's boot rule (cdexroute.ProviderBase)
// refuses a base by this same function, so boot and each exchange agree.
func URLCharacter(c byte) bool { return strings.IndexByte(urlCharacters, c) >= 0 }

// urlCharacters are RFC 3986's unreserved and reserved characters, and "%".
const urlCharacters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" + "-._~" + ":/?#[]@" + "!$&'()*+,;=" + "%"

// checkLocationBase reports why base cannot begin a Location: it must be an
// absolute http or https URL with a host, no user information, query or
// fragment (no "?" or "#" at all), no trailing slash, no "." or ".." path
// segment (spelled or escaped), and only ASCII, with no whitespace, control
// character or other character outside RFC 3986's set (URLCharacter). A
// per-provider base, as built, has none of these.
func checkLocationBase(base string) error {
	if strings.IndexFunc(base, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("the base holds whitespace or a control character")
	}
	for i := 0; i < len(base); i++ {
		if base[i] >= utf8.RuneSelf {
			return errors.New("the base holds a character that is not ASCII")
		}
		if !URLCharacter(base[i]) {
			return errors.New("the base holds a character a URL cannot carry")
		}
	}
	if strings.ContainsAny(base, "?#") {
		return errors.New("the base carries a query or fragment")
	}
	if strings.HasSuffix(base, "/") {
		return errors.New("the base ends in a slash")
	}
	u, err := url.Parse(base)
	switch {
	case err != nil:
		return errors.New("the base is not a URL")
	case (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Opaque != "":
		return errors.New("the base is not an absolute http or https URL")
	case u.User != nil:
		return errors.New("the base carries user information")
	}
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if v, err := url.PathUnescape(seg); err != nil || v == "." || v == ".." {
			return errors.New("the base's path holds a dot segment, or one that cannot be read")
		}
	}
	return nil
}
