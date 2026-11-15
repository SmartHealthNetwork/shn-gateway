package subscription

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The fixed inputs of the known-answer vectors.
const (
	katHolder   = "provider-1"
	katEndpoint = "https://ehr.example/api/pas/notification?ehr=https%3A%2F%2Fehr.example%2Ffhir"
)

var (
	katBinding = Binding{Subscriber: "provider-1", Notifier: "payer-1", Topic: "pas"}
	katHeaders = []string{"Authorization: Bearer s3cret", "X-Custom: a"}
)

// seedKey is the ed25519 key whose seed is the 32 bytes from first, counting
// up.
func seedKey(first byte) ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = first + byte(i)
	}
	return ed25519.NewKeyFromSeed(seed)
}

var (
	curKey  = seedKey(0)
	predKey = seedKey(32)
	altKey  = seedKey(64)
)

func mustKeys(t testing.TB, holder string, cur, pred ed25519.PrivateKey) Keys {
	t.Helper()
	k, err := DeriveKeys(holder, cur, pred)
	if err != nil {
		t.Fatalf("DeriveKeys: %v", err)
	}
	return k
}

// fixedNonces makes the header seal's nonces the bytes from first, counting
// up, for the length of a test.
func fixedNonces(t testing.TB, first byte) {
	t.Helper()
	prev := randReader
	randReader = &countingReader{next: first}
	t.Cleanup(func() { randReader = prev })
}

type countingReader struct{ next byte }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.next
		r.next++
	}
	return len(p), nil
}

// The known-answer vectors: a fixed seed, holder and inputs give exactly these
// key ids, sealed values and tags. The key ids, sealed endpoints and
// credentials were recomputed outside Go (Python's hmac and hashlib for HKDF,
// the SIV and the tags; OpenSSL for the ed25519 public keys and AES-256-CTR);
// the header seals' nonces are the bytes 0xa0, 0xa1, …. A change to a label, the length prefixes,
// the SIV composition, the GCM framing or the credential's input changes
// them.
func TestKnownAnswers(t *testing.T) {
	fixedNonces(t, 0xa0)
	k := mustKeys(t, katHolder, curKey, predKey)
	p := mustKeys(t, katHolder, predKey, nil)

	const (
		wantKID         = "ocurkBVBmv4"
		wantPredKID     = "BSbP4XLp-HK"
		wantEndpoint    = "ocurkBVBmv4.NDl-amuZ2jPykJWQ0VqMeEyx0N0SE76GGCiX2s1iW-Y8aJ3GBlTAsj5oNdLBbu85kkoW_i8MsQsednjhFC9a62UKjQ-oYkoZm_jk5uZEMx-IovZaI9wl_9L3mRUI"
		wantPredSealed  = "BSbP4XLp-HK.8K1q4rqFkJLQ7r-jL76o6JVB51dLUUhV6sh9Kppo8Z9-KuaGvnFEiVdbKgNt0knk_dobIq8TL1PK80dvjzDe_0htMLf09lFVWmhshCxyz5AUEe5ipMhb0mNrT9SJ"
		wantHeaders     = "ocurkBVBmv4.oKGio6Slpqeoqaqr2OokfwsOkpDmWSbirfpnfMhRt4BF2u-eJO-SBwpJa3DaT4hnMTZeGc2--l0QWPksG7SYITbXGUKBT1Oa499kok-44w"
		wantEmpty       = "ocurkBVBmv4.rK2ur7CxsrO0tba3OBp_2Ydcz7PoMAoQ8Yz9fjjn9_8"
		wantCredential  = "Bearer ocurkBVBmv4.IPk1HzZRdnImInVKS2FUDQ"
		wantPredCred    = "Bearer BSbP4XLp-HK.cQopSjfjQ-LVxX52t58EyA"
		wantAddressForm = "https://payer-gw.example/ingress/notify/pas/provider-1/ocurkBVBmv4/NDl-amuZ2jPykJWQ0VqMeEyx0N0SE76GGCiX2s1iW-Y8aJ3GBlTAsj5oNdLBbu85kkoW_i8MsQsednjhFC9a62UKjQ-oYkoZm_jk5uZEMx-IovZaI9wl_9L3mRUI"
	)
	sealed := k.SealEndpoint(katBinding, katEndpoint)
	headers, err := k.SealHeaders(katBinding, sealed, katHeaders)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := k.SealHeaders(katBinding, sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := Address{Ingress: "https://payer-gw.example/ingress", Topic: "pas", Subscriber: "provider-1", SealedEndpoint: sealed}
	for _, row := range []struct{ name, got, want string }{
		{"key id", k.KeyID(), wantKID},
		{"predecessor key id", p.KeyID(), wantPredKID},
		{"sealed endpoint", sealed, wantEndpoint},
		{"sealed endpoint, predecessor", p.SealEndpoint(katBinding, katEndpoint), wantPredSealed},
		{"sealed headers", headers, wantHeaders},
		{"sealed empty headers", empty, wantEmpty},
		{"address", a.String(), wantAddressForm},
		{"credential", k.Credential(a, headers), wantCredential},
		{"credential, predecessor", p.Credential(a, headers), wantPredCred},
	} {
		if row.got != row.want {
			t.Errorf("%s = %q, want %q", row.name, row.got, row.want)
		}
	}
}

// The derived keys are each under a label of their own: no two of an
// identity's four keys are equal, the same seed under another holder gives
// other keys, and a key id is the public key's alone.
func TestDeriveKeys_SeparatesKeys(t *testing.T) {
	k := mustKeys(t, katHolder, curKey, nil)
	other := mustKeys(t, "provider-2", curKey, nil)
	keys := [][]byte{k.cur.mac, k.cur.enc, k.cur.gcm, k.cur.cred, other.cur.mac, other.cur.enc, other.cur.gcm, other.cur.cred}
	for i := range keys {
		if len(keys[i]) != 32 {
			t.Fatalf("key %d is %d bytes", i, len(keys[i]))
		}
		for j := range i {
			if bytes.Equal(keys[i], keys[j]) {
				t.Fatalf("keys %d and %d are equal", j, i)
			}
		}
		if bytes.Contains(curKey, keys[i]) {
			t.Fatalf("key %d is the signing key's own bytes", i)
		}
	}
	if k.KeyID() != other.KeyID() {
		t.Fatal("a key id depends on the holder; it is the public key's alone")
	}
	// A value sealed for one holder does not open under another's keys.
	if _, err := other.OpenEndpoint(katBinding, k.SealEndpoint(katBinding, katEndpoint)); !errors.Is(err, ErrSealInvalid) {
		t.Fatalf("another holder's keys opened it: %v", err)
	}
}

// DeriveKeys refuses an empty holder, a current key that is not an ed25519
// key, a predecessor of the wrong size, and a predecessor that is the current
// key; a nil predecessor configures none.
func TestDeriveKeys_Refuses(t *testing.T) {
	for name, row := range map[string]struct {
		holder    string
		cur, pred ed25519.PrivateKey
	}{
		"an empty holder":                {"", curKey, nil},
		"no current key":                 {katHolder, nil, nil},
		"a short current key":            {katHolder, curKey[:31], nil},
		"a short predecessor":            {katHolder, curKey, predKey[:63]},
		"the current key as predecessor": {katHolder, curKey, slices.Clone(curKey)},
	} {
		if k, err := DeriveKeys(row.holder, row.cur, row.pred); err == nil {
			t.Errorf("%s: derived %v", name, k)
		}
	}
	k := mustKeys(t, katHolder, curKey, nil)
	if k.pred != nil || k.cur == nil {
		t.Fatal("a nil predecessor configured one")
	}
	if (Keys{}).KeyID() != "" {
		t.Fatal("zero Keys have a key id")
	}
}

// One endpoint and Binding seal identically, every time and under every
// derivation of the same keys; another endpoint, or another Binding in any
// field, does not.
func TestSealEndpoint_Deterministic(t *testing.T) {
	k := mustKeys(t, katHolder, curKey, predKey)
	again := mustKeys(t, katHolder, curKey, nil)
	sealed := k.SealEndpoint(katBinding, katEndpoint)
	if sealed == "" || sealed != k.SealEndpoint(katBinding, katEndpoint) || sealed != again.SealEndpoint(katBinding, katEndpoint) {
		t.Fatal("one endpoint and Binding sealed to two values")
	}
	for name, other := range map[string]string{
		"another endpoint":      k.SealEndpoint(katBinding, katEndpoint+"x"),
		"another subscriber":    k.SealEndpoint(Binding{"provider-2", "payer-1", "pas"}, katEndpoint),
		"another notifier":      k.SealEndpoint(Binding{"provider-1", "payer-2", "pas"}, katEndpoint),
		"another topic":         k.SealEndpoint(Binding{"provider-1", "payer-1", "cdex-task"}, katEndpoint),
		"fields shifted":        k.SealEndpoint(Binding{"provider-1payer-1", "", "pas"}, katEndpoint),
		"another key":           mustKeys(t, katHolder, altKey, nil).SealEndpoint(katBinding, katEndpoint),
		"the predecessor's key": mustKeys(t, katHolder, predKey, nil).SealEndpoint(katBinding, katEndpoint),
	} {
		if other == sealed {
			t.Errorf("%s sealed to the same value", name)
		}
	}
	if k.SealEndpoint(katBinding, "") != "" || (Keys{}).SealEndpoint(katBinding, katEndpoint) != "" {
		t.Fatal("an empty endpoint or zero Keys sealed something")
	}
}

// A sealed endpoint opens to its exact bytes, under its Binding only.
func TestOpenEndpoint_RoundTrip(t *testing.T) {
	k := mustKeys(t, katHolder, curKey, nil)
	for _, ep := range []string{katEndpoint, "h", "https://a.example/" + strings.Repeat("é", 300), "https://x.example/p?q=1&r=%20#"} {
		got, err := k.OpenEndpoint(katBinding, k.SealEndpoint(katBinding, ep))
		if err != nil || got != ep {
			t.Fatalf("open %q = %q, %v", ep, got, err)
		}
	}
}

// flip returns sealed with bit (0..7) of decoded byte i flipped.
func flip(t testing.TB, sealed string, i int, bit uint) string {
	t.Helper()
	kid, raw, ok := splitSealed(sealed)
	if !ok {
		t.Fatalf("not a sealed value: %q", sealed)
	}
	raw = slices.Clone(raw)
	raw[i] ^= 1 << bit
	return kid + "." + b64.EncodeToString(raw)
}

// truncated returns sealed with its decoded bytes cut to n.
func truncated(t testing.TB, sealed string, n int) string {
	t.Helper()
	kid, raw, _ := splitSealed(sealed)
	return kid + "." + b64.EncodeToString(raw[:n])
}

// strayBits returns sealed with the last base64url character's lowest bit
// flipped, a bit that encodes nothing when the decoded length is not a
// multiple of three: a lenient decoder reads the same bytes.
func strayBits(t testing.TB, sealed string) string {
	t.Helper()
	_, raw, _ := splitSealed(sealed)
	if len(raw)%3 == 0 {
		t.Fatalf("the decoded length %d has no stray bits", len(raw))
	}
	last := strings.IndexByte(b64URLAlphabet, sealed[len(sealed)-1])
	return sealed[:len(sealed)-1] + string(b64URLAlphabet[last^1])
}

// Every way a sealed endpoint can be wrong is refused, and none returns a
// plaintext.
func TestOpenEndpoint_Rejects(t *testing.T) {
	k := mustKeys(t, katHolder, curKey, nil)
	sealed := k.SealEndpoint(katBinding, katEndpoint)
	kid, raw, _ := splitSealed(sealed)
	body := sealed[len(kid)+1:]
	retired := mustKeys(t, katHolder, predKey, nil).SealEndpoint(katBinding, katEndpoint)
	rows := map[string]struct {
		b      Binding
		sealed string
		want   error
	}{
		"a flipped ciphertext bit, first":   {katBinding, flip(t, sealed, sivLen, 0), ErrSealInvalid},
		"a flipped ciphertext bit, last":    {katBinding, flip(t, sealed, len(raw)-1, 7), ErrSealInvalid},
		"a flipped IV bit, first":           {katBinding, flip(t, sealed, 0, 0), ErrSealInvalid},
		"a flipped IV bit, last":            {katBinding, flip(t, sealed, sivLen-1, 7), ErrSealInvalid},
		"another subscriber":                {Binding{"provider-2", "payer-1", "pas"}, sealed, ErrSealInvalid},
		"another notifier":                  {Binding{"provider-1", "payer-2", "pas"}, sealed, ErrSealInvalid},
		"another topic":                     {Binding{"provider-1", "payer-1", "cdex-task"}, sealed, ErrSealInvalid},
		"another key under this key id":     {katBinding, kid + "." + strings.SplitN(mustKeys(t, katHolder, altKey, nil).SealEndpoint(katBinding, katEndpoint), ".", 2)[1], ErrSealInvalid},
		"an unknown key id":                 {katBinding, "AAAAAAAAAAA." + body, ErrKeyRetired},
		"a predecessor's key id, none set":  {katBinding, retired, ErrKeyRetired},
		"empty":                             {katBinding, "", ErrSealInvalid},
		"no dot":                            {katBinding, kid + body, ErrSealInvalid},
		"no body":                           {katBinding, kid + ".", ErrSealInvalid},
		"a short key id":                    {katBinding, kid[:10] + "." + body, ErrSealInvalid},
		"a long key id":                     {katBinding, kid + "A." + body, ErrSealInvalid},
		"a key id off the alphabet":         {katBinding, kid[:10] + "+." + body, ErrSealInvalid},
		"bad base64":                        {katBinding, kid + "." + body[:len(body)-1] + "!", ErrSealInvalid},
		"base64 with padding":               {katBinding, kid + "." + body + "=", ErrSealInvalid},
		"a stray bit in the last character": {katBinding, strayBits(t, k.SealEndpoint(katBinding, katEndpoint+"x")), ErrSealInvalid},
		"an LF inside":                      {katBinding, kid + "." + body[:20] + "\n" + body[20:], ErrSealInvalid},
		"a CR inside":                       {katBinding, kid + "." + body[:20] + "\r" + body[20:], ErrSealInvalid},
		"a CRLF at the end":                 {katBinding, sealed + "\r\n", ErrSealInvalid},
		"standard base64":                   {katBinding, kid + "." + strings.NewReplacer("-", "+", "_", "/").Replace(body) + "+/", ErrSealInvalid},
		"truncated to the IV":               {katBinding, truncated(t, sealed, sivLen), ErrSealInvalid},
		"truncated inside the IV":           {katBinding, truncated(t, sealed, sivLen-1), ErrSealInvalid},
		"truncated to one byte":             {katBinding, truncated(t, sealed, 1), ErrSealInvalid},
		"truncated by one byte":             {katBinding, truncated(t, sealed, len(raw)-1), ErrSealInvalid},
		"a byte appended":                   {katBinding, kid + "." + b64.EncodeToString(append(slices.Clone(raw), 0)), ErrSealInvalid},
		"over the length bound":             {katBinding, kid + "." + strings.Repeat("A", maxSealedLen), ErrSealInvalid},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			got, err := k.OpenEndpoint(row.b, row.sealed)
			if !errors.Is(err, row.want) || got != "" {
				t.Fatalf("OpenEndpoint = %q, %v; want %v", got, err, row.want)
			}
		})
	}
	// A wrong key: the same value opened by keys that hold neither its key id
	// nor its key.
	if _, err := mustKeys(t, katHolder, altKey, nil).OpenEndpoint(katBinding, sealed); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("another identity's keys: %v, want ErrKeyRetired", err)
	}
	if _, err := (Keys{}).OpenEndpoint(katBinding, sealed); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("zero Keys: %v, want ErrKeyRetired", err)
	}
}

// Headers seal with a fresh nonce each time, open in order beside their
// endpoint only, and an empty set opens as an empty set.
func TestSealHeaders_RoundTrip(t *testing.T) {
	k := mustKeys(t, katHolder, curKey, nil)
	sealed := k.SealEndpoint(katBinding, katEndpoint)
	for _, hs := range [][]string{katHeaders, nil, {}, {""}, {"", ""}, {"a: b", "a: b"}, {strings.Repeat("x", 5000)}} {
		v, err := k.SealHeaders(katBinding, sealed, hs)
		if err != nil {
			t.Fatal(err)
		}
		got, err := k.OpenHeaders(katBinding, sealed, v)
		if err != nil || got == nil || !slices.Equal(got, hs) {
			t.Fatalf("open %q = %q, %v", hs, got, err)
		}
	}
	a, _ := k.SealHeaders(katBinding, sealed, katHeaders)
	b, _ := k.SealHeaders(katBinding, sealed, katHeaders)
	if a == b {
		t.Fatal("two seals of one header set are equal: the nonce is not fresh")
	}
	if _, err := (Keys{}).SealHeaders(katBinding, sealed, katHeaders); err == nil {
		t.Fatal("zero Keys sealed headers")
	}
	prev := randReader
	randReader = strings.NewReader("short")
	defer func() { randReader = prev }()
	if v, err := k.SealHeaders(katBinding, sealed, katHeaders); err == nil {
		t.Fatalf("sealed with no nonce: %q", v)
	}
}

// Nothing is sealed that an open could not read: an endpoint or header set
// whose sealed value would pass the 64 KiB an open reads is not sealed, and
// one at the bound seals and opens.
func TestSeal_HoldsTheOpenLimit(t *testing.T) {
	k := mustKeys(t, katHolder, curKey, predKey)
	// KeyIDLen + 1 + EncodedLen(16 + n) <= 65536 holds to n = 49127.
	at := "https://e.example/" + strings.Repeat("a", 49127-len("https://e.example/"))
	sealed := k.SealEndpoint(katBinding, at)
	if len(sealed) > maxSealedLen {
		t.Fatalf("sealed %d bytes, over the bound", len(sealed))
	}
	if ep, err := k.OpenEndpoint(katBinding, sealed); err != nil || ep != at {
		t.Fatalf("at the bound: %v", err)
	}
	if v := k.SealEndpoint(katBinding, at+"a"); v != "" {
		t.Fatalf("past the bound sealed %d bytes", len(v))
	}
	if v := k.SealEndpointForSearch(katBinding, at+"a"); v != nil {
		t.Fatal("past the bound, a search value was made")
	}
	// The header set's encoding adds 8 bytes for one value, GCM 28.
	hAt := []string{strings.Repeat("h", 49143-12-16-8)}
	v, err := k.SealHeaders(katBinding, sealed, hAt)
	if err != nil || len(v) > maxSealedLen {
		t.Fatalf("headers at the bound: %d, %v", len(v), err)
	}
	if hs, err := k.OpenHeaders(katBinding, sealed, v); err != nil || hs[0] != hAt[0] {
		t.Fatalf("headers at the bound do not open: %v", err)
	}
	if v, err := k.SealHeaders(katBinding, sealed, []string{hAt[0] + "h"}); err == nil {
		t.Fatalf("headers past the bound sealed %d bytes", len(v))
	}
}

// Every way a sealed header set can be wrong is refused.
func TestOpenHeaders_Rejects(t *testing.T) {
	k := mustKeys(t, katHolder, curKey, nil)
	sealed := k.SealEndpoint(katBinding, katEndpoint)
	v, err := k.SealHeaders(katBinding, sealed, katHeaders)
	if err != nil {
		t.Fatal(err)
	}
	kid, raw, _ := splitSealed(v)
	body := v[len(kid)+1:]
	retired, _ := mustKeys(t, katHolder, predKey, nil).SealHeaders(katBinding, sealed, katHeaders)
	rows := map[string]struct {
		b        Binding
		endpoint string
		sealed   string
		want     error
	}{
		"a flipped nonce bit":               {katBinding, sealed, flip(t, v, 0, 0), ErrSealInvalid},
		"a flipped ciphertext bit":          {katBinding, sealed, flip(t, v, gcmNonceLen, 3), ErrSealInvalid},
		"a flipped tag bit":                 {katBinding, sealed, flip(t, v, len(raw)-1, 7), ErrSealInvalid},
		"another subscriber":                {Binding{"provider-2", "payer-1", "pas"}, sealed, v, ErrSealInvalid},
		"another notifier":                  {Binding{"provider-1", "payer-2", "pas"}, sealed, v, ErrSealInvalid},
		"another topic":                     {Binding{"provider-1", "payer-1", "cdex-task"}, sealed, v, ErrSealInvalid},
		"beside another sealed endpoint":    {katBinding, k.SealEndpoint(katBinding, katEndpoint+"x"), v, ErrSealInvalid},
		"an unknown key id":                 {katBinding, sealed, "AAAAAAAAAAA." + body, ErrKeyRetired},
		"a predecessor's key id, none set":  {katBinding, sealed, retired, ErrKeyRetired},
		"empty":                             {katBinding, sealed, "", ErrSealInvalid},
		"bad base64":                        {katBinding, sealed, kid + ".!!" + body, ErrSealInvalid},
		"a stray bit in the last character": {katBinding, sealed, strayBits(t, v), ErrSealInvalid},
		"an LF inside":                      {katBinding, sealed, kid + "." + body[:20] + "\n" + body[20:], ErrSealInvalid},
		"a CR inside":                       {katBinding, sealed, kid + "." + body[:20] + "\r" + body[20:], ErrSealInvalid},
		"truncated to the nonce and tag":    {katBinding, sealed, truncated(t, v, gcmNonceLen+gcmTagLen), ErrSealInvalid},
		"truncated below the nonce and tag": {katBinding, sealed, truncated(t, v, gcmNonceLen+gcmTagLen-1), ErrSealInvalid},
		"truncated inside the nonce":        {katBinding, sealed, truncated(t, v, gcmNonceLen-1), ErrSealInvalid},
		"truncated by one byte":             {katBinding, sealed, truncated(t, v, len(raw)-1), ErrSealInvalid},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			got, err := k.OpenHeaders(row.b, row.endpoint, row.sealed)
			if !errors.Is(err, row.want) || got != nil {
				t.Fatalf("OpenHeaders = %q, %v; want %v", got, err, row.want)
			}
		})
	}
	if _, err := mustKeys(t, katHolder, altKey, nil).OpenHeaders(katBinding, sealed, v); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("another identity's keys: %v, want ErrKeyRetired", err)
	}
	// Another key under this key id: the same holder's value under another
	// identity, given the current key id.
	other, err := mustKeys(t, katHolder, altKey, nil).SealHeaders(katBinding, sealed, katHeaders)
	if err != nil {
		t.Fatal(err)
	}
	_, otherRaw, _ := splitSealed(other)
	if _, err := k.OpenHeaders(katBinding, sealed, kid+"."+b64.EncodeToString(otherRaw)); !errors.Is(err, ErrSealInvalid) {
		t.Fatalf("another key under this key id: %v, want ErrSealInvalid", err)
	}
}

// The header set's encoding is read exactly: a count over what follows, a
// length past the end, or a byte after the last value is no set.
func TestDecodeHeaders_Exact(t *testing.T) {
	good := encodeHeaders([]string{"a", "bc"})
	if got, ok := decodeHeaders(good); !ok || !slices.Equal(got, []string{"a", "bc"}) {
		t.Fatalf("decode = %q, %v", got, ok)
	}
	for name, b := range map[string][]byte{
		"empty":               nil,
		"a short count":       {0, 0, 0},
		"a count over":        {0, 0, 0, 3, 0, 0, 0, 1, 'a', 0, 0, 0, 2, 'b', 'c'},
		"a huge count":        {0xff, 0xff, 0xff, 0xff},
		"a length past":       {0, 0, 0, 1, 0, 0, 0, 9, 'a'},
		"a short length":      {0, 0, 0, 1, 0, 0},
		"a byte after":        append(slices.Clone(good), 0),
		"a value cut":         good[:len(good)-1],
		"a count of none, +1": {0, 0, 0, 0, 0},
	} {
		if got, ok := decodeHeaders(b); ok {
			t.Errorf("%s decoded to %q", name, got)
		}
	}
}

func katAddress(sealed string) Address {
	return Address{Ingress: "https://payer-gw.example/ingress", Topic: "pas", Subscriber: "provider-1", SealedEndpoint: sealed}
}

// A credential verifies for its address and sealed headers only, under the
// key its key id names.
func TestVerifyCredential(t *testing.T) {
	k := mustKeys(t, "payer-1", curKey, nil)
	sub := mustKeys(t, "provider-1", altKey, nil)
	a := katAddress(sub.SealEndpoint(katBinding, katEndpoint))
	sh, err := sub.SealHeaders(katBinding, a.SealedEndpoint, katHeaders)
	if err != nil {
		t.Fatal(err)
	}
	otherSh, err := sub.SealHeaders(katBinding, a.SealedEndpoint, katHeaders)
	if err != nil {
		t.Fatal(err)
	}
	cred := k.Credential(a, sh)
	if !strings.HasPrefix(cred, "Bearer "+k.KeyID()+".") || len(cred) != len("Bearer ")+KeyIDLen+1+credentialTagLen {
		t.Fatalf("credential %q is not Bearer <kid>.<tag>", cred)
	}
	if err := k.VerifyCredential(a, sh, cred); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := k.VerifyCredential(a, sh, "bearer "+strings.TrimPrefix(cred, "Bearer ")); err != nil {
		t.Fatalf("the scheme in another case: %v", err)
	}
	other := a
	other.Subscriber = "provider-2"
	_, tok, _ := strings.Cut(cred, " ")
	kid, tag, _ := strings.Cut(tok, ".")
	tagBytes, _ := b64.DecodeString(tag)
	tagBytes[0] ^= 1
	retired := mustKeys(t, "payer-1", predKey, nil).Credential(a, sh)
	for name, row := range map[string]struct {
		a             Address
		sealed, value string
		want          error
	}{
		"another address":                    {other, sh, cred, ErrCredentialInvalid},
		"other sealed headers":               {a, otherSh, cred, ErrCredentialInvalid},
		"a flipped tag bit":                  {a, sh, "Bearer " + kid + "." + b64.EncodeToString(tagBytes), ErrCredentialInvalid},
		"another holder's key":               {a, sh, mustKeys(t, "payer-2", curKey, nil).Credential(a, sh), ErrCredentialInvalid},
		"an unknown key id":                  {a, sh, "Bearer AAAAAAAAAAA." + tag, ErrKeyRetired},
		"a predecessor's key id, none set":   {a, sh, retired, ErrKeyRetired},
		"empty":                              {a, sh, "", ErrCredentialInvalid},
		"no scheme":                          {a, sh, tok, ErrCredentialInvalid},
		"another scheme":                     {a, sh, "Basic " + tok, ErrCredentialInvalid},
		"two spaces":                         {a, sh, "Bearer  " + tok, ErrCredentialInvalid},
		"no dot":                             {a, sh, "Bearer " + kid + tag, ErrCredentialInvalid},
		"a short tag":                        {a, sh, "Bearer " + kid + "." + tag[:21], ErrCredentialInvalid},
		"a long tag":                         {a, sh, "Bearer " + kid + "." + tag + "A", ErrCredentialInvalid},
		"a tag off the alphabet":             {a, sh, "Bearer " + kid + "." + tag[:21] + "!", ErrCredentialInvalid},
		"a tag with stray bits":              {a, sh, "Bearer " + kid + "." + tag[:21] + "B", ErrCredentialInvalid},
		"a CR in the tag":                    {a, sh, "Bearer " + kid + "." + tag[:10] + "\r" + tag[11:], ErrCredentialInvalid},
		"a CR in the tag, an unknown key id": {a, sh, "Bearer AAAAAAAAAAA." + tag[:10] + "\r" + tag[11:], ErrCredentialInvalid},
		"a CR in a tag that decodes":         {a, sh, "Bearer AAAAAAAAAAA.AAAAAAAAAA\rAAAAAAAAAAAA", ErrCredentialInvalid},
		"a short key id":                     {a, sh, "Bearer " + kid[:10] + "." + tag, ErrCredentialInvalid},
	} {
		if err := k.VerifyCredential(row.a, row.sealed, row.value); !errors.Is(err, row.want) {
			t.Errorf("%s: %v, want %v", name, err, row.want)
		}
	}
	if (Keys{}).Credential(a, sh) != "" {
		t.Fatal("zero Keys issued a credential")
	}
	// Nothing is issued or verified for inputs E-19 does not write: an
	// address that does not read back as itself, a sealed endpoint or sealed
	// headers without a sealed value's form.
	bad := map[string]struct {
		a  Address
		sh string
	}{
		"a sealed endpoint with no dot":         {katAddress("AAAAAAAAAAA"), sh},
		"a sealed endpoint of only an IV":       {katAddress("AAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA"), sh},
		"a sealed endpoint off the alphabet":    {katAddress(a.SealedEndpoint + "/x"), sh},
		"a sealed endpoint with a short key id": {katAddress(a.SealedEndpoint[1:]), sh},
		"an empty topic":                        {Address{Ingress: a.Ingress, Topic: "", Subscriber: a.Subscriber, SealedEndpoint: a.SealedEndpoint}, sh},
		"an empty subscriber":                   {Address{Ingress: a.Ingress, Topic: a.Topic, Subscriber: "", SealedEndpoint: a.SealedEndpoint}, sh},
		"an ingress with a query":               {Address{Ingress: a.Ingress + "?x=1", Topic: a.Topic, Subscriber: a.Subscriber, SealedEndpoint: a.SealedEndpoint}, sh},
		"a relative ingress":                    {Address{Ingress: "/ingress", Topic: a.Topic, Subscriber: a.Subscriber, SealedEndpoint: a.SealedEndpoint}, sh},
		"sealed headers that are no seal":       {a, "sealed-headers"},
		"sealed headers too short":              {a, "AAAAAAAAAAA.AAAA"},
		"no sealed headers":                     {a, ""},
	}
	for name, row := range bad {
		if c := k.Credential(row.a, row.sh); c != "" {
			t.Errorf("%s: issued %q", name, c)
		}
		forged := "Bearer " + k.KeyID() + "." + k.cur.credentialTag(row.a.String(), row.sh)
		if err := k.VerifyCredential(row.a, row.sh, forged); !errors.Is(err, ErrCredentialInvalid) {
			t.Errorf("%s: verified a tag over it: %v", name, err)
		}
	}
}

// The predecessor opens and verifies through a rotation's overlap, and is
// ErrKeyRetired once it is no longer configured; it never seals or issues:
// SealEndpoint, SealHeaders and Credential use the current key id even when
// the predecessor is set.
func TestPredecessor_OpensButNeverSeals(t *testing.T) {
	before := mustKeys(t, katHolder, predKey, nil)
	overlap := mustKeys(t, katHolder, curKey, predKey)
	after := mustKeys(t, katHolder, curKey, nil)

	sealed := before.SealEndpoint(katBinding, katEndpoint)
	headers, err := before.SealHeaders(katBinding, sealed, katHeaders)
	if err != nil {
		t.Fatal(err)
	}
	a := katAddress(sealed)
	cred := before.Credential(a, headers)

	if ep, err := overlap.OpenEndpoint(katBinding, sealed); err != nil || ep != katEndpoint {
		t.Fatalf("within the overlap, endpoint: %q, %v", ep, err)
	}
	if hs, err := overlap.OpenHeaders(katBinding, sealed, headers); err != nil || !slices.Equal(hs, katHeaders) {
		t.Fatalf("within the overlap, headers: %q, %v", hs, err)
	}
	if err := overlap.VerifyCredential(a, headers, cred); err != nil {
		t.Fatalf("within the overlap, credential: %v", err)
	}
	if _, err := after.OpenEndpoint(katBinding, sealed); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("past the overlap, endpoint: %v", err)
	}
	if _, err := after.OpenHeaders(katBinding, sealed, headers); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("past the overlap, headers: %v", err)
	}
	if err := after.VerifyCredential(a, headers, cred); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("past the overlap, credential: %v", err)
	}

	// Sealing and issuing use the current key, predecessor set or not.
	cur := overlap.KeyID() + "."
	if overlap.KeyID() != after.KeyID() || overlap.KeyID() == before.KeyID() {
		t.Fatal("KeyID is not the current key's")
	}
	if s := overlap.SealEndpoint(katBinding, katEndpoint); !strings.HasPrefix(s, cur) || s != after.SealEndpoint(katBinding, katEndpoint) {
		t.Fatalf("SealEndpoint used another key: %q", s)
	}
	if h, err := overlap.SealHeaders(katBinding, sealed, katHeaders); err != nil || !strings.HasPrefix(h, cur) {
		t.Fatalf("SealHeaders used another key: %q, %v", h, err)
	}
	if c := overlap.Credential(a, headers); !strings.HasPrefix(c, "Bearer "+cur) || c != after.Credential(a, headers) {
		t.Fatalf("Credential used another key: %q", c)
	}
	// A search names both during the overlap, the current first, and one
	// after it.
	both := overlap.SealEndpointForSearch(katBinding, katEndpoint)
	if !slices.Equal(both, []string{after.SealEndpoint(katBinding, katEndpoint), sealed}) {
		t.Fatalf("search values in the overlap: %q", both)
	}
	if one := after.SealEndpointForSearch(katBinding, katEndpoint); !slices.Equal(one, []string{after.SealEndpoint(katBinding, katEndpoint)}) {
		t.Fatalf("search values past the overlap: %q", one)
	}
	// The predecessor's search value opens through the overlap and is retired
	// past it.
	if ep, err := overlap.OpenEndpoint(katBinding, both[1]); err != nil || ep != katEndpoint {
		t.Fatalf("the predecessor's search value within the overlap: %q, %v", ep, err)
	}
	if _, err := after.OpenEndpoint(katBinding, both[1]); !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("the predecessor's search value past the overlap: %v, want ErrKeyRetired", err)
	}
	if none := after.SealEndpointForSearch(katBinding, ""); none != nil {
		t.Fatalf("search values of an empty endpoint: %q", none)
	}
}

// Keys format as redacted for every verb, so no key byte reaches a log, a
// record or an error.
func TestKeys_FormatRedacted(t *testing.T) {
	k := mustKeys(t, katHolder, curKey, predKey)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		for _, v := range []any{k, &k} {
			got := fmt.Sprintf(verb, v)
			if !strings.Contains(got, "redacted") {
				t.Errorf("%s of %T = %q", verb, v, got)
			}
			for _, key := range [][]byte{k.cur.mac, k.cur.enc, k.cur.gcm, k.cur.cred, k.pred.mac} {
				for _, form := range []string{string(key), fmt.Sprintf("%x", key), fmt.Sprintf("%X", key), fmt.Sprint(key)} {
					if strings.Contains(got, form) {
						t.Errorf("%s of %T leaks a key", verb, v)
					}
				}
			}
		}
	}
	if k.String() != "subscription.Keys{redacted}" || k.GoString() != "subscription.Keys{redacted}" {
		t.Fatal("String or GoString is not redacted")
	}
}
