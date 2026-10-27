package auditappend

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

// decodeULID is the inverse of encodeULID, for the round trip.
func decodeULID(t *testing.T, s string) [16]byte {
	t.Helper()
	var hi, lo uint64
	for _, c := range s {
		v := strings.IndexRune(crockford, c)
		if v < 0 {
			t.Fatalf("%q holds %q, outside the alphabet", s, c)
		}
		hi = hi<<5 | lo>>59
		lo = lo<<5 | uint64(v)
	}
	var b [16]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(hi >> (56 - 8*i))
		b[8+i] = byte(lo >> (56 - 8*i))
	}
	return b
}

// TestNewRecordIDEncoding pins the text form: the ends of the range, and the
// clock in the first 48 bits.
func TestNewRecordIDEncoding(t *testing.T) {
	for _, c := range []struct {
		ms   int64
		fill byte
		want string
	}{
		{0, 0x00, "00000000000000000000000000"},
		{1<<48 - 1, 0xff, "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"},
		{1, 0x00, "00000000010000000000000000"},
	} {
		got, err := NewRecordID(time.UnixMilli(c.ms), bytes.NewReader(bytes.Repeat([]byte{c.fill}, 10)))
		if err != nil || got != c.want {
			t.Errorf("ms %d fill %#x: %q, %v; want %q", c.ms, c.fill, got, err, c.want)
		}
	}
}

// TestNewRecordIDRoundTrips: the 16 bytes decode back from the text, the
// clock's milliseconds first, big-endian.
func TestNewRecordIDRoundTrips(t *testing.T) {
	now := time.UnixMilli(1781827200123)
	entropy := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	id, err := NewRecordID(now, bytes.NewReader(entropy))
	if err != nil {
		t.Fatal(err)
	}
	b := decodeULID(t, id)
	var ms uint64
	for _, x := range b[:6] {
		ms = ms<<8 | uint64(x)
	}
	if int64(ms) != now.UnixMilli() || !bytes.Equal(b[6:], entropy) {
		t.Fatalf("%s decodes to ms %d entropy %x", id, ms, b[6:])
	}
	if !recordIDShape.MatchString(id) {
		t.Fatalf("%s fails the guard's recordId shape", id)
	}
}

// TestNewRecordIDUniqueWithinAMillisecond: the Plane indexes recordId as
// unique and never orders by it, so what matters within one millisecond is
// that ids differ, which 80 bits of entropy give.
func TestNewRecordIDUniqueWithinAMillisecond(t *testing.T) {
	now := time.UnixMilli(1781827200000)
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		id, err := NewRecordID(now, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("recordId %s minted twice in one millisecond", id)
		}
		seen[id] = true
	}
}

// TestNewRecordIDShortEntropyIsAnError: an entropy source that runs dry mints
// nothing rather than a weaker id.
func TestNewRecordIDShortEntropyIsAnError(t *testing.T) {
	if _, err := NewRecordID(time.Now(), bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Fatal("NewRecordID minted from 3 bytes of entropy")
	}
}
