// Package subscription holds what both gateways share about a topic
// Subscription that crosses the network: the keys a gateway seals a
// subscriber's channel under and issues a notification address's credential
// with, the notification address, the topic interface each topic implements,
// the filter matcher and the closed shape, the ownership test and the search
// rules, the edits E-18 to E-21 as library functions, and the notification
// route's structural checks.
//
// It reads and writes no exchange: the engine calls it, records what it
// returns, and registers the edits.
package subscription

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The version tag every label of this package opens with. A later scheme
// takes a new tag, so no value made under one is valid under the other.
const labelVersion = "shn-subscription/v1"

// The HKDF info labels, each followed by the holder id: the SIV's
// MAC key and cipher key, the header seal's GCM key, and the credential key
// are each derived under a label of their own, so no key is ever used for two
// purposes or by two holders.
const (
	infoSIVMac     = labelVersion + "/siv-mac/"
	infoSIVEnc     = labelVersion + "/siv-enc/"
	infoHeaderGCM  = labelVersion + "/header-gcm/"
	infoCredential = labelVersion + "/credential/"
)

// The labels that open each MAC's or AEAD's input, so a value of one kind is
// never valid as another.
const (
	kidLabel        = labelVersion + "/kid"
	endpointADLabel = labelVersion + "/endpoint-ad"
	headersADLabel  = labelVersion + "/headers-ad"
	credentialLabel = labelVersion + "/credential"
)

// Sizes.
const (
	// KeyIDLen is the length of a key id: 11 base64url characters (66 bits)
	// of SHA-256 over the kid label and the identity's public key.
	KeyIDLen = 11
	// sivLen is the SIV's synthetic IV: HMAC-SHA-256 truncated to 128 bits,
	// used whole as AES-CTR's initial counter block.
	sivLen = 16
	// gcmNonceLen is the header seal's random nonce, 96 bits.
	gcmNonceLen = 12
	// gcmTagLen is AES-GCM's tag.
	gcmTagLen = 16
	// credentialTagLen is a credential's tag: HMAC-SHA-256 truncated to 128
	// bits, in base64url without padding.
	credentialTagLen = 22
	// maxSealedLen bounds a sealed value read, so a hostile value costs no
	// more than this to refuse.
	maxSealedLen = 64 << 10
)

var b64 = base64.RawURLEncoding.Strict()

// ErrKeyRetired is a value made under a key that is neither this gateway's
// current key nor its configured predecessor. It is
// fail-closed: the caller records a not-made or refusal reason.
var ErrKeyRetired = errors.New("subscription: the value's key is retired")

// ErrSealInvalid is a sealed value that does not open: malformed, truncated,
// altered, or sealed for other associated data.
var ErrSealInvalid = errors.New("subscription: the sealed value does not open")

// ErrCredentialInvalid is a notification credential that does not verify.
var ErrCredentialInvalid = errors.New("subscription: the credential does not verify")

// keySet is one identity's derived keys and its key id.
type keySet struct {
	kid                 string
	mac, enc, gcm, cred []byte
}

// Keys is a gateway's subscription keys: the current identity's,
// and, when the predecessor setting is configured, the predecessor's for a
// rotation's overlap. Its bytes never leave the package: it formats as
// redacted, so it cannot reach a log, a record or an error.
type Keys struct {
	cur, pred *keySet
}

// String is redacted (the keys never leave the gateway).
func (Keys) String() string { return "subscription.Keys{redacted}" }

// GoString is redacted.
func (Keys) GoString() string { return "subscription.Keys{redacted}" }

// Format is redacted for every verb.
func (Keys) Format(f fmt.State, _ rune) { fmt.Fprint(f, "subscription.Keys{redacted}") }

// DeriveKeys derives a gateway's subscription keys by
// HKDF-SHA-256 from each identity signing key's seed, never using the signing
// key itself as a MAC or cipher key. Four keys are derived per identity, each
// under an info label of its own holding the version tag and the holder id
// ("shn-subscription/v1/siv-mac/<holder>", "…/siv-enc/<holder>",
// "…/header-gcm/<holder>", "…/credential/<holder>"): the SIV's MAC and cipher
// keys, the header seal's GCM key, and the credential key.
//
// predecessor is nil unless the optional predecessor setting
// (IDENTITY_PREDECESSOR_DIR) is configured: the same holder's previous
// identity, kept for a rotation's overlap. Its keys are used only to open and
// verify, never to seal or issue.
//
// It refuses an empty holder, a current key that is not an ed25519 private
// key, a predecessor of the wrong size, and a predecessor whose key, or key
// id, equals the current one's.
func DeriveKeys(holder string, current, predecessor ed25519.PrivateKey) (Keys, error) {
	if holder == "" {
		return Keys{}, errors.New("subscription: no holder id")
	}
	if len(current) != ed25519.PrivateKeySize {
		return Keys{}, errors.New("subscription: no identity signing key")
	}
	cur, err := deriveSet(holder, current)
	if err != nil {
		return Keys{}, err
	}
	k := Keys{cur: cur}
	if predecessor == nil {
		return k, nil
	}
	if len(predecessor) != ed25519.PrivateKeySize {
		return Keys{}, errors.New("subscription: the predecessor is not an ed25519 private key")
	}
	if subtle.ConstantTimeCompare(predecessor.Seed(), current.Seed()) == 1 {
		return Keys{}, errors.New("subscription: the predecessor's key is the current key")
	}
	pred, err := deriveSet(holder, predecessor)
	if err != nil {
		return Keys{}, err
	}
	if pred.kid == cur.kid {
		return Keys{}, errors.New("subscription: the predecessor's key id is the current key's")
	}
	k.pred = pred
	return k, nil
}

func deriveSet(holder string, priv ed25519.PrivateKey) (*keySet, error) {
	seed := priv.Seed()
	s := &keySet{kid: keyID(priv.Public().(ed25519.PublicKey))}
	for _, d := range []struct {
		dst  *[]byte
		info string
	}{
		{&s.mac, infoSIVMac}, {&s.enc, infoSIVEnc}, {&s.gcm, infoHeaderGCM}, {&s.cred, infoCredential},
	} {
		k, err := hkdf.Key(sha256.New, seed, nil, d.info+holder, 32)
		if err != nil {
			return nil, errors.New("subscription: derive a key")
		}
		*d.dst = k
	}
	return s, nil
}

// keyID is 11 base64url characters of SHA-256 over the length-prefixed kid
// label and public key.
func keyID(pub ed25519.PublicKey) string {
	h := sha256.New()
	h.Write(lp(nil, kidLabel))
	h.Write(lp(nil, string(pub)))
	return b64.EncodeToString(h.Sum(nil))[:KeyIDLen]
}

// KeyID is the current identity's key id: 11 base64url characters from its
// public key, usable as a path segment (the notification address). It is
// "" for zero Keys.
func (k Keys) KeyID() string {
	if k.cur == nil {
		return ""
	}
	return k.cur.kid
}

// byKID chooses a key set by the key id a value names, never by trying each
// key: the current one, or the predecessor when configured. Anything else is
// ErrKeyRetired.
func (k Keys) byKID(kid string) (*keySet, error) {
	switch {
	case k.cur != nil && kid == k.cur.kid:
		return k.cur, nil
	case k.pred != nil && kid == k.pred.kid:
		return k.pred, nil
	}
	return nil, ErrKeyRetired
}

// lp appends s to dst with an unambiguous length prefix: its length as a
// 4-byte big-endian integer, then its bytes.
func lp(dst []byte, s string) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(s)))
	return append(dst, s...)
}

// Binding is what a sealed value is bound to: the subscriber's
// and the notifier's participant ids, and the topic's token ("pas",
// "cdex-task"), the same string as the notification address's topic segment.
// It is the associated data of both seals.
type Binding struct{ Subscriber, Notifier, Topic string }

// ad is a seal's associated data: the seal's own label (with the version tag),
// then each Binding field, each length-prefixed, then any extra values.
func (b Binding) ad(label string, extra ...string) []byte {
	out := lp(nil, label)
	out = lp(out, b.Subscriber)
	out = lp(out, b.Notifier)
	out = lp(out, b.Topic)
	for _, e := range extra {
		out = lp(out, e)
	}
	return out
}

// validKeyID reports whether s has a key id's form.
func validKeyID(s string) bool { return len(s) == KeyIDLen && validB64URL(s) }

// b64URLAlphabet is base64url's alphabet (RFC 4648 §5).
const b64URLAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

func b64URLChar(c byte) bool { return strings.IndexByte(b64URLAlphabet, c) >= 0 }

// validB64URL reports whether s is non-empty and every character is
// base64url's.
func validB64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		if !b64URLChar(s[i]) {
			return false
		}
	}
	return s != ""
}

// splitSealed reads "<kid>.<base64url>" into the key id and the decoded
// bytes, checking the form and the length bound before anything is decoded.
// Every character of the body must be base64url's: Go's decoder skips a CR
// or LF even in strict mode, so a value with one inserted would otherwise
// open as the value without it.
func splitSealed(s string) (string, []byte, bool) {
	if len(s) > maxSealedLen {
		return "", nil, false
	}
	kid, body, ok := strings.Cut(s, ".")
	if !ok || !validKeyID(kid) || !validB64URL(body) {
		return "", nil, false
	}
	raw, err := b64.DecodeString(body)
	if err != nil {
		return "", nil, false
	}
	return kid, raw, true
}

// siv is the synthetic IV: HMAC-SHA-256 under k_mac over the length-prefixed
// associated data and endpoint, truncated to 16 bytes.
func (s *keySet) siv(ad []byte, endpoint string) []byte {
	m := hmac.New(sha256.New, s.mac)
	m.Write(lp(nil, string(ad)))
	m.Write(lp(nil, endpoint))
	return m.Sum(nil)[:sivLen]
}

// ctr is AES-256-CTR under k_enc with the whole 16-byte IV as the initial
// counter block.
func (s *keySet) ctr(iv, in []byte) []byte {
	block, err := aes.NewCipher(s.enc)
	if err != nil {
		panic("subscription: a derived key is not an AES-256 key") // unreachable: derived keys are 32 bytes
	}
	out := make([]byte, len(in))
	cipher.NewCTR(block, iv).XORKeyStream(out, in)
	return out
}

// SealEndpoint seals a Subscription's channel.endpoint, its exact bytes, under
// the current key. It is a deterministic AEAD, the generic SIV
// composition: IV = HMAC-SHA-256(k_mac, LP(ad) ‖ LP(endpoint)) truncated to
// 16 bytes, where ad length-prefixes a label with the version tag and b's
// three fields; then AES-256-CTR(k_enc) over the endpoint with the whole IV as
// the counter block. One endpoint always seals to one value under one key and
// Binding, so a search by url can name it.
//
// The value is "<kid>.<base64url(IV ‖ ciphertext)>". It is "" for zero Keys,
// an empty endpoint, or one whose sealed value would be longer than an open
// reads (64 KiB): nothing is sealed that could not be opened.
func (k Keys) SealEndpoint(b Binding, endpoint string) (sealed string) {
	if k.cur == nil || endpoint == "" || !fitsSealed(sivLen+len(endpoint)) {
		return ""
	}
	return k.cur.sealEndpoint(b, endpoint)
}

// fitsSealed reports whether a sealed value of rawLen decoded bytes is within
// the length an open reads.
func fitsSealed(rawLen int) bool { return KeyIDLen+1+b64.EncodedLen(rawLen) <= maxSealedLen }

// validSealedEndpoint reports whether s has a sealed endpoint's form: a key
// id, then base64url of more than the IV.
func validSealedEndpoint(s string) bool {
	_, raw, ok := splitSealed(s)
	return ok && len(raw) > sivLen
}

// validSealedHeaders reports whether s has a sealed header set's form: a key
// id, then base64url of at least the nonce and the tag.
func validSealedHeaders(s string) bool {
	_, raw, ok := splitSealed(s)
	return ok && len(raw) >= gcmNonceLen+gcmTagLen
}

func (s *keySet) sealEndpoint(b Binding, endpoint string) string {
	iv := s.siv(b.ad(endpointADLabel), endpoint)
	raw := append(append([]byte(nil), iv...), s.ctr(iv, []byte(endpoint))...)
	return s.kid + "." + b64.EncodeToString(raw)
}

// SealEndpointForSearch is the endpoint sealed as a search by url names it:
// under the current key, and, during a rotation's overlap,
// under the predecessor's as well, so the search finds a Subscription made
// under either. It is nil when SealEndpoint would be "".
//
// Its values are for one use only: the subscriptionEndpoint frame header of a
// search by url, which E-21 turns into the url the notifier's system
// searches. A caller never writes one into a Subscription (SealOutbound seals
// with SealEndpoint, the current key alone): the predecessor's value names a
// value already issued under that key, which the predecessor never issues
// anew.
func (k Keys) SealEndpointForSearch(b Binding, endpoint string) []string {
	cur := k.SealEndpoint(b, endpoint)
	if cur == "" {
		return nil
	}
	out := []string{cur}
	if k.pred != nil {
		out = append(out, k.pred.sealEndpoint(b, endpoint))
	}
	return out
}

// OpenEndpoint opens a value SealEndpoint made, on E-20's restore and at delivery. The key
// is chosen by the value's key id alone, the current one or the configured
// predecessor; any other key id is ErrKeyRetired. The value is decrypted, the
// IV recomputed over the plaintext and b's associated data, and compared in
// constant time; a value that is malformed, truncated, altered or sealed for
// another Binding is ErrSealInvalid, and no plaintext is returned.
func (k Keys) OpenEndpoint(b Binding, sealed string) (endpoint string, err error) {
	kid, raw, ok := splitSealed(sealed)
	if !ok {
		return "", ErrSealInvalid
	}
	s, err := k.byKID(kid)
	if err != nil {
		return "", err
	}
	if len(raw) <= sivLen {
		return "", ErrSealInvalid
	}
	iv, ct := raw[:sivLen], raw[sivLen:]
	pt := s.ctr(iv, ct)
	if !hmac.Equal(iv, s.siv(b.ad(endpointADLabel), string(pt))) {
		return "", ErrSealInvalid
	}
	return string(pt), nil
}

// randReader is the header seal's nonce source; tests replace it to pin
// known-answer values.
var randReader io.Reader = rand.Reader

// encodeHeaders writes a header set unambiguously, in order: the count as a
// 4-byte big-endian integer, then each value length-prefixed.
func encodeHeaders(headers []string) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(headers)))
	for _, h := range headers {
		out = lp(out, h)
	}
	return out
}

// decodeHeaders reads encodeHeaders' form exactly: every length within the
// bytes left, and nothing after the last value. An empty set is a non-nil
// empty slice.
func decodeHeaders(b []byte) ([]string, bool) {
	if len(b) < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(b)
	b = b[4:]
	if uint64(n) > uint64(len(b))/4 {
		return nil, false
	}
	out := make([]string, 0, n)
	for range n {
		if len(b) < 4 {
			return nil, false
		}
		l := binary.BigEndian.Uint32(b)
		b = b[4:]
		if uint64(l) > uint64(len(b)) {
			return nil, false
		}
		out = append(out, string(b[:l]))
		b = b[l:]
	}
	if len(b) != 0 {
		return nil, false
	}
	return out, true
}

func (s *keySet) aead() cipher.AEAD {
	block, err := aes.NewCipher(s.gcm)
	if err != nil {
		panic("subscription: a derived key is not an AES-256 key") // unreachable: derived keys are 32 bytes
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		panic("subscription: AES-GCM is unavailable") // unreachable with the standard nonce and tag sizes
	}
	return a
}

// SealHeaders seals a Subscription's channel.header values, in order, under
// the current key, so the subscriber's secret never crosses the
// network in clear. An empty set seals too, so every Subscription carries one.
//
// It is AES-256-GCM under k_gcm with a 96-bit nonce from crypto/rand, over the
// header set encoded unambiguously (a count, then each value length-prefixed).
// The associated data length-prefixes a label with the version tag, b's three
// fields and sealedEndpoint, so the set opens only beside that endpoint. With
// random 96-bit nonces a key seals at most 2^32 messages (NIST SP 800-38D
// §8.3); a gateway seals one per Subscription create or update, many orders
// of magnitude below that between rotations.
//
// The value is "<kid>.<base64url(nonce ‖ ciphertext)>". A header set whose
// sealed value would be longer than an open reads (64 KiB) is not sealed.
func (k Keys) SealHeaders(b Binding, sealedEndpoint string, headers []string) (string, error) {
	if k.cur == nil {
		return "", errors.New("subscription: no keys")
	}
	pt := encodeHeaders(headers)
	if !fitsSealed(gcmNonceLen + len(pt) + gcmTagLen) {
		return "", errors.New("subscription: the header set is too long to seal")
	}
	nonce := make([]byte, gcmNonceLen)
	if _, err := io.ReadFull(randReader, nonce); err != nil {
		return "", errors.New("subscription: no nonce")
	}
	ct := k.cur.aead().Seal(nil, nonce, pt, b.ad(headersADLabel, sealedEndpoint))
	return k.cur.kid + "." + b64.EncodeToString(append(nonce, ct...)), nil
}

// OpenHeaders opens a value SealHeaders made beside sealedEndpoint, on E-20's
// restore and at delivery, choosing the key by the value's key id alone, as
// OpenEndpoint does. A value that is malformed, truncated, altered, sealed for
// another Binding or beside another sealed endpoint is ErrSealInvalid; an empty
// set opens as a non-nil empty slice.
func (k Keys) OpenHeaders(b Binding, sealedEndpoint, sealed string) ([]string, error) {
	kid, raw, ok := splitSealed(sealed)
	if !ok {
		return nil, ErrSealInvalid
	}
	s, err := k.byKID(kid)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcmNonceLen+gcmTagLen {
		return nil, ErrSealInvalid
	}
	pt, err := s.aead().Open(nil, raw[:gcmNonceLen], raw[gcmNonceLen:], b.ad(headersADLabel, sealedEndpoint))
	if err != nil {
		return nil, ErrSealInvalid
	}
	headers, ok := decodeHeaders(pt)
	if !ok {
		return nil, ErrSealInvalid
	}
	return headers, nil
}

// credentialTag is HMAC-SHA-256 under k_cred over the length-prefixed label,
// address and sealed headers, truncated to 16 bytes, in base64url.
func (s *keySet) credentialTag(address, sealedHeaders string) string {
	m := hmac.New(sha256.New, s.cred)
	m.Write(lp(nil, credentialLabel))
	m.Write(lp(nil, address))
	m.Write(lp(nil, sealedHeaders))
	return b64.EncodeToString(m.Sum(nil)[:16])
}

// Credential is the notifier gateway's own credential for a notification
// address, which E-19 writes into channel.header beside the sealed
// headers: "Bearer <kid>.<tag>", the tag a MAC under the current credential
// key over the address (a.String()) and sealedHeaders. It is "" for zero
// Keys, and for inputs that are not what E-19 writes (issuable): nothing is
// issued for an address that does not read back as itself, or beside a value
// that is not a sealed header set.
func (k Keys) Credential(a Address, sealedHeaders string) string {
	if k.cur == nil || !issuable(a, sealedHeaders) {
		return ""
	}
	return "Bearer " + k.cur.kid + "." + k.cur.credentialTag(a.String(), sealedHeaders)
}

// issuable reports whether a and sealedHeaders are what E-19 writes: a's
// sealed endpoint has a sealed endpoint's form, a reads back as itself
// through ParseAddress, and sealedHeaders has a sealed header set's form.
func issuable(a Address, sealedHeaders string) bool {
	if !validSealedEndpoint(a.SealedEndpoint) || !validSealedHeaders(sealedHeaders) {
		return false
	}
	back, ok := ParseAddress(a.String(), a.Ingress)
	return ok && back == a
}

// VerifyCredential verifies an Authorization value against the notification
// address and sealed headers it was issued for. The key is chosen by
// the value's key id alone, the current one or the configured predecessor;
// any other key id is ErrKeyRetired. The tag is compared in constant time.
// Anything else that does not verify, a missing or malformed value included,
// and an address or sealed headers Credential would issue nothing for, is
// ErrCredentialInvalid.
func (k Keys) VerifyCredential(a Address, sealedHeaders, authorization string) error {
	if !issuable(a, sealedHeaders) {
		return ErrCredentialInvalid
	}
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ErrCredentialInvalid
	}
	kid, tag, ok := strings.Cut(token, ".")
	if !ok || !validKeyID(kid) || len(tag) != credentialTagLen {
		return ErrCredentialInvalid
	}
	// A CR or LF, which the decoder skips, leaves 21 characters, which never
	// decode; the tag is then compared as the exact string.
	if _, err := b64.DecodeString(tag); err != nil {
		return ErrCredentialInvalid
	}
	s, err := k.byKID(kid)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(s.credentialTag(a.String(), sealedHeaders)), []byte(tag)) {
		return ErrCredentialInvalid
	}
	return nil
}
