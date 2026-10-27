package auditappend

import (
	"encoding/binary"
	"io"
	"time"
)

// crockford is the ULID alphabet: Crockford's base32, without I, L, O and U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewRecordID mints a record's recordId: a ULID, 26 characters of Crockford
// base32 over 48 bits of milliseconds since the Unix epoch, big-endian, then
// 80 bits read from rnd. The Audit Plane requires the recordId of a
// module-details or gateway-start record and refuses any value its strict
// ULID parse refuses. It treats the value as an opaque token, unique by its
// own index, and orders records by its commit sequence, never by recordId: so
// uniqueness matters here and order within a millisecond does not.
//
// The gateway module takes no ULID dependency for this; a test pins the
// encoding against fixed inputs, and the root module's test parses minted
// values with the Plane's own strict parser.
func NewRecordID(now time.Time, rnd io.Reader) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))
	if _, err := io.ReadFull(rnd, b[6:]); err != nil {
		return "", err
	}
	return encodeULID(b), nil
}

// encodeULID writes the 128 bits of b as 26 base32 characters, the first
// carrying the top 3 bits: the canonical ULID text form.
func encodeULID(b [16]byte) string {
	var out [26]byte
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
