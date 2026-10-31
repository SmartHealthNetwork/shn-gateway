package engine

import (
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The budget takes what fits and refuses what does not, at the exact line, and
// a release gives back exactly what was taken. A request over the limit is
// counted at the limit, so it fits an idle budget.
func TestBodyBudget(t *testing.T) {
	b := &bodyBudget{limit: 10}
	first, ok := b.acquire(4)
	if !ok {
		t.Fatal("4 of 10 refused")
	}
	if _, ok := b.acquire(7); ok {
		t.Fatal("7 more accepted with 6 free")
	}
	second, ok := b.acquire(6)
	if !ok {
		t.Fatal("6 more refused with 6 free")
	}
	if _, ok := b.acquire(1); ok {
		t.Fatal("1 more accepted with none free")
	}
	first()
	second()
	if b.held != 0 {
		t.Fatalf("held %d after both released", b.held)
	}
	over, ok := b.acquire(25)
	if !ok || b.held != 10 {
		t.Fatalf("a request over the limit on an idle budget: ok %v, held %d, want the limit", ok, b.held)
	}
	over()
	if b.held != 0 {
		t.Fatalf("held %d after a request over the limit released", b.held)
	}
}

// The budget is never below the attachment ceiling, so a full submission always
// fits; unset is the default.
func TestAttachmentInflight(t *testing.T) {
	for name, row := range map[string]struct {
		cfg  Config
		want int64
	}{
		"unset":               {Config{}, defaultAttachmentInflightBytes},
		"stated":              {Config{AttachmentInflightBytes: 100 << 20}, 100 << 20},
		"under the network's": {Config{AttachmentInflightBytes: 1 << 20}, shnsdk.MaxAttachmentBytes},
		"under a set ceiling": {Config{AttachmentInflightBytes: 1 << 20, AttachmentCeiling: 2 << 20}, 2 << 20},
		"at a lower ceiling":  {Config{AttachmentInflightBytes: 1 << 20, AttachmentCeiling: 1 << 20}, 1 << 20},
	} {
		t.Run(name, func(t *testing.T) {
			if got := attachmentInflight(row.cfg); got != row.want {
				t.Fatalf("budget %d, want %d", got, row.want)
			}
		})
	}
}
