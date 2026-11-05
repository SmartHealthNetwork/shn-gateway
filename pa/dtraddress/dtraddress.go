// Package dtraddress holds what both gateways share about a DTR adaptive
// address the payer's gateway issues: the address tag, the key it is made
// under, the structural match of an address against a base, the clean
// remainder rule, and the forms the sealed frame headers carry them in
// (operationSubpath, operationAddressTags). It reads and writes no message;
// the gateways' edits (E-12, E-13) do.
package dtraddress

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// The two address families. A payer address names a base and a remainder; a
// fetch-server address stands for an adaptive boolean true: the payer's DTR
// endpoint, with no remainder.
const (
	FamilyPayer       = "payer"
	FamilyFetchServer = "fetch-server"
)

// TagLen is the length of an address tag: an HMAC-SHA-256 truncated to 128
// bits, in base64url without padding.
const TagLen = 22

// BoolRemainder is the remainder an operationAddressTags pair writes for an
// address the answer states as the boolean true. It is a sub-delimiter, so no
// clean remainder can be it.
const BoolRemainder = "*"

const (
	// tagLabel opens every tag's input, so a tag is never valid for another
	// purpose of the same key.
	tagLabel = "shn-dtr-address-tag/v1"
	// keyLabel is the HKDF info the tag key is derived under.
	keyLabel = "shn-dtr-address-tag-key/v1"
)

var (
	lineSegment  = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	tagSegment   = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	cleanSegment = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

// Key is a payer gateway's address-tag key. Its bytes never leave the
// package: it formats as redacted, so it cannot reach a log, a record or an
// error.
type Key struct{ k []byte }

// String is redacted.
func (Key) String() string { return "dtraddress.Key{redacted}" }

// GoString is redacted.
func (Key) GoString() string { return "dtraddress.Key{redacted}" }

// Format is redacted for every verb.
func (Key) Format(f fmt.State, _ rune) { fmt.Fprint(f, "dtraddress.Key{redacted}") }

// IsZero reports whether k holds no key.
func (k Key) IsZero() bool { return len(k.k) == 0 }

// DeriveKey derives the address-tag key from the gateway's identity signing
// key by HKDF-SHA-256 under a label of its own, never using the signing key
// as an HMAC key. Every replica of a gateway holds the same identity, so each
// derives the same key; a rotation of the identity (D125) gives a new key,
// under which the addresses issued before it no longer verify.
func DeriveKey(signPriv ed25519.PrivateKey) (Key, error) {
	if len(signPriv) != ed25519.PrivateKeySize {
		return Key{}, errors.New("dtraddress: no identity signing key")
	}
	k, err := hkdf.Key(sha256.New, signPriv.Seed(), nil, keyLabel, 32)
	if err != nil {
		return Key{}, errors.New("dtraddress: derive the address-tag key")
	}
	return Key{k: k}, nil
}

// Address is what a tag binds: the family, the payer's participant id, the
// participant id of the provider the address is issued to, the line, the
// base at that line and the remainder (empty for a fetch-server address).
type Address struct {
	Family, Payer, Provider, Line, Base, Remainder string
}

// Tag returns the address tag of a under k.
func (k Key) Tag(a Address) string {
	m := hmac.New(sha256.New, k.k)
	for _, f := range []string{tagLabel, a.Family, a.Payer, a.Provider, a.Line, a.Base, a.Remainder} {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(f)))
		m.Write(l[:])
		m.Write([]byte(f))
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// Verify reports whether tag is a's tag under k, in constant time. A zero key
// verifies nothing.
func (k Key) Verify(a Address, tag string) bool {
	if k.IsZero() || !ValidTag(tag) {
		return false
	}
	return hmac.Equal([]byte(k.Tag(a)), []byte(tag))
}

// ValidTag reports whether s has a tag's form: exactly TagLen base64url
// characters.
func ValidTag(s string) bool { return tagSegment.MatchString(s) }

// ValidLine reports whether s has a contract line's form (major.minor).
func ValidLine(s string) bool { return lineSegment.MatchString(s) }

// CleanRemainder reports whether r is a clean remainder: empty, or one or
// more segments, each "/" then one or more unreserved characters, none of
// them "." or "..". No percent-encoding and no reserved character can pass.
func CleanRemainder(r string) bool {
	if r == "" {
		return true
	}
	if !strings.HasPrefix(r, "/") {
		return false
	}
	for _, s := range strings.Split(r[1:], "/") {
		if s == "." || s == ".." || !cleanSegment.MatchString(s) {
			return false
		}
	}
	return true
}

// Under reports whether value is under base by the structural match both
// gateways use, and its remainder: both parse as absolute URLs; the scheme
// and host are equal case-folded and the effective port is equal; value has
// no userinfo, query or fragment; and base's path segments are a prefix of
// value's, segment by segment, byte-exact with percent-encoding kept. The
// remainder is value's path after base's, "" when there is none.
func Under(value, base string) (remainder string, ok bool) {
	v, err := url.Parse(value)
	if err != nil || !v.IsAbs() || v.Opaque != "" || v.User != nil || v.RawQuery != "" || v.ForceQuery || v.Fragment != "" || strings.Contains(value, "#") {
		return "", false
	}
	b, err := url.Parse(base)
	if err != nil || !b.IsAbs() || b.Opaque != "" || b.User != nil || b.RawQuery != "" || b.Fragment != "" {
		return "", false
	}
	if !strings.EqualFold(v.Scheme, b.Scheme) || !strings.EqualFold(v.Hostname(), b.Hostname()) || effectivePort(v) != effectivePort(b) {
		return "", false
	}
	basePath, valuePath := strings.TrimRight(b.EscapedPath(), "/"), v.EscapedPath()
	if valuePath == basePath {
		return "", true
	}
	if !strings.HasPrefix(valuePath, basePath+"/") {
		return "", false
	}
	return valuePath[len(basePath):], true
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// Spelled reports whether value is spelled exactly as base followed by a
// clean remainder, and that remainder: the form the re-key can give back
// byte for byte. base has no ending slash.
func Spelled(value, base string) (remainder string, ok bool) {
	if !strings.HasPrefix(value, base) {
		return "", false
	}
	r := value[len(base):]
	if !CleanRemainder(r) {
		return "", false
	}
	if _, under := Under(value, base); !under {
		return "", false
	}
	return r, true
}

// TagPair is one operationAddressTags pair: the remainder of an address (or
// BoolRemainder for the boolean true) and its tag.
type TagPair struct{ Remainder, Tag string }

// FormatTags writes pairs as an operationAddressTags value: each
// "<remainder>=<tag>", separated by single spaces.
func FormatTags(pairs []TagPair) string {
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.Remainder + "=" + p.Tag
	}
	return strings.Join(out, " ")
}

// ParseTags reads an operationAddressTags value. A pair whose remainder is not
// clean (and not BoolRemainder), or whose tag is not a tag's form, is
// refused, as is a remainder given twice with different tags.
func ParseTags(s string) ([]TagPair, error) {
	var out []TagPair
	seen := map[string]string{}
	for _, f := range strings.Fields(s) {
		r, tag, ok := strings.Cut(f, "=")
		if !ok || !ValidTag(tag) || (r != BoolRemainder && !CleanRemainder(r)) {
			return nil, fmt.Errorf("dtraddress: operationAddressTags pair %q is not <remainder>=<tag>", f)
		}
		if prev, dup := seen[r]; dup {
			if prev != tag {
				return nil, fmt.Errorf("dtraddress: operationAddressTags names remainder %q twice", r)
			}
			continue
		}
		seen[r] = tag
		out = append(out, TagPair{Remainder: r, Tag: tag})
	}
	return out, nil
}

// Subpath is an operationSubpath value: the family, line, tag and remainder
// of the address a round was called at.
type Subpath struct{ Family, Line, Tag, Remainder string }

// String writes s as an operationSubpath value.
func (s Subpath) String() string {
	return "/" + s.Family + "/" + s.Line + "/" + s.Tag + s.Remainder
}

// ParseSubpath reads an operationSubpath value: "/payer/<line>/<tag><remainder>"
// with a clean remainder, or "/fetch-server/<line>/<tag>".
func ParseSubpath(v string) (Subpath, error) {
	segs := strings.SplitN(v, "/", 5)
	if len(segs) < 4 || segs[0] != "" {
		return Subpath{}, fmt.Errorf("dtraddress: operationSubpath %q is not /<family>/<line>/<tag>…", v)
	}
	s := Subpath{Family: segs[1], Line: segs[2], Tag: segs[3]}
	if len(segs) == 5 {
		s.Remainder = "/" + segs[4]
	}
	switch {
	case s.Family != FamilyPayer && s.Family != FamilyFetchServer:
		return Subpath{}, fmt.Errorf("dtraddress: operationSubpath names family %q", s.Family)
	case !ValidLine(s.Line) || !ValidTag(s.Tag):
		return Subpath{}, fmt.Errorf("dtraddress: operationSubpath %q has no line or tag", v)
	case s.Family == FamilyFetchServer && s.Remainder != "":
		return Subpath{}, fmt.Errorf("dtraddress: a fetch-server operationSubpath carries no remainder")
	case !CleanRemainder(s.Remainder):
		return Subpath{}, fmt.Errorf("dtraddress: operationSubpath remainder is not clean")
	}
	return s, nil
}

// Issued is an address the provider's gateway wrote for a payer, read back
// from a value: its family, the payer's participant id (unescaped), line, tag
// and remainder.
type Issued struct{ Family, Payer, Line, Tag, Remainder string }

// FindIssued reads, from an absolute URL with no userinfo, query or fragment,
// the first occurrence in its path of the segments "dtr", a family, a payer
// id, a line and a tag, followed by the rest of the path as the remainder
// (none for fetch-server, whose tag must be the last segment). What comes
// before those segments is not read. ok is false for any other value.
func FindIssued(value string) (Issued, bool) {
	u, err := url.Parse(value)
	if err != nil || !u.IsAbs() || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") {
		return Issued{}, false
	}
	segs := strings.Split(u.EscapedPath(), "/")
	for i := 0; i+4 < len(segs); i++ {
		if segs[i] != "dtr" || (segs[i+1] != FamilyPayer && segs[i+1] != FamilyFetchServer) {
			continue
		}
		payer, err := url.PathUnescape(segs[i+2])
		if err != nil || payer == "" || !ValidLine(segs[i+3]) || !ValidTag(segs[i+4]) {
			return Issued{}, false
		}
		is := Issued{Family: segs[i+1], Payer: payer, Line: segs[i+3], Tag: segs[i+4]}
		if rest := segs[i+5:]; len(rest) > 0 {
			is.Remainder = "/" + strings.Join(rest, "/")
		}
		if is.Family == FamilyFetchServer && is.Remainder != "" {
			return Issued{}, false
		}
		return is, true
	}
	return Issued{}, false
}
