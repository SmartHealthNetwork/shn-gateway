//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// benchRecord is a record object of about size bytes: dense small objects
// (a coding each), the shape that holds the most tokens per byte, or one
// large string member.
func benchRecord(size int, dense bool) []byte {
	var b bytes.Buffer
	b.WriteString(`{"resourceType":"Observation","id":"o1",`)
	if !dense {
		b.WriteString(`"note":[{"text":"`)
		b.WriteString(strings.Repeat("x", size))
		b.WriteString(`"}]}`)
		return b.Bytes()
	}
	b.WriteString(`"component":[`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"system":"http://loinc.org","code":"%d","display":"d"}`, i)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// wideRecord is a record object of about size bytes whose array holds
// objects of members names each: past memberLinearMax, each such object's
// names are kept in a set.
func wideRecord(size, members int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"resourceType":"Observation","id":"o1","component":[`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		for m := range members {
			if m > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"m%d":%d`, m, i)
		}
		b.WriteByte('}')
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

func tokens(r []byte) int {
	dec := json.NewDecoder(bytes.NewReader(r))
	n := 0
	for {
		if _, err := dec.Token(); err != nil {
			return n
		}
		n++
	}
}

func BenchmarkHeldRecordCheck(b *testing.B) {
	for _, tc := range []struct {
		name  string
		size  int
		dense bool
	}{
		{"dense-1Mtokens", 0, true},
		{"dense-9MiB", 9 << 20, true},
		{"dense-16MiB", 16 << 20, true},
		{"string-9MiB", 9 << 20, false},
		{"dense-64KiB", 64 << 10, true},
		{"wide17-16MiB", 16 << 20, false},
	} {
		size := tc.size
		if size == 0 {
			// ~8 tokens per 56-byte coding: 1,000,000 tokens.
			size = 1_000_000 / 8 * 56
		}
		r := benchRecord(size, tc.dense)
		if strings.HasPrefix(tc.name, "wide17") {
			r = wideRecord(size, memberLinearMax+1)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.Logf("%d bytes, %d tokens", len(r), tokens(r))
			b.Run("valid", func(b *testing.B) {
				b.SetBytes(int64(len(r)))
				b.ReportAllocs()
				for range b.N {
					_ = json.Valid(r)
				}
			})
			b.Run("oneSpellingPerMember", func(b *testing.B) {
				b.SetBytes(int64(len(r)))
				b.ReportAllocs()
				for range b.N {
					if err := oneSpellingPerMember(r); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("tokenWalk", func(b *testing.B) {
				b.SetBytes(int64(len(r)))
				b.ReportAllocs()
				for range b.N {
					if err := oneSpellingPerMemberByTokens(r); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("uncarriableRecordBytes", func(b *testing.B) {
				b.SetBytes(int64(len(r)))
				b.ReportAllocs()
				for range b.N {
					if why := uncarriableRecordBytes(r); why != "" {
						b.Fatal(why)
					}
				}
			})
		})
	}
}
