//go:build !shn_coreonly

package pgstore

import (
	"context"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// MON-R45 / MON-R15: what this replica has not chosen or loaded is unset,
// never a zero time; the signing key's not_after is the current key's even
// when a newer one is cached, and the newest is the newest of any.
func TestMONR45KeyTimesReadsTheCacheAndLeavesUnknownUnset(t *testing.T) {
	s := NewIngressKeyStore(nil, "holder", nil)
	if got := s.KeyTimes(); got.HasSigning || got.HasNewest || !got.SigningNotAfter.IsZero() || !got.NewestCreated.IsZero() {
		t.Fatalf("empty store KeyTimes = %+v, want nothing set", got)
	}
	t0 := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	s.keys = map[string]cachedKey{
		"old": {created: t0, notAfter: t0.Add(25 * time.Hour)},
		"new": {created: t0.Add(24 * time.Hour), notAfter: t0.Add(49 * time.Hour)},
	}
	got := s.KeyTimes()
	if got.HasSigning || !got.HasNewest || !got.NewestCreated.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("no signing key chosen: KeyTimes = %+v, want only the newest set", got)
	}
	s.signing = "old"
	if got := s.KeyTimes(); !got.HasSigning || !got.SigningNotAfter.Equal(t0.Add(25*time.Hour)) || !got.NewestCreated.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("signing with old: KeyTimes = %+v", got)
	}
	s.signing = "absent"
	if got := s.KeyTimes(); got.HasSigning {
		t.Fatalf("a signing kid the cache does not hold reads as set: %+v", got)
	}
}

// MON-R45: with the database gone when rotation falls due, the refresh
// cannot rotate. The current key's remaining time counts down from over an
// hour, and the newest key's age passes the rotation interval, while
// SigningKey still answers. So the stall is visible before the 503.
func TestMONR45StalledRotationShowsBeforeTheSlackRunsOut(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "holder", now)
	kid, _, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	var created, notAfter time.Time
	if err := pool.QueryRow(context.Background(), `SELECT created_at, not_after FROM gw_ingress_key WHERE holder_id='holder' AND kid=$1`, kid).Scan(&created, &notAfter); err != nil {
		t.Fatal(err)
	}
	got := s.KeyTimes()
	if !got.HasSigning || !got.SigningNotAfter.Equal(notAfter) || !got.HasNewest || !got.NewestCreated.Equal(created) {
		t.Fatalf("KeyTimes = %+v, want the row's created %v and not_after %v", got, created, notAfter)
	}

	advance(ingressKeyRotation) // rotation due
	pool.Close()
	if err := s.refreshOnce(now()); err == nil {
		t.Fatal("refresh against a closed pool succeeded")
	}
	got = s.KeyTimes()
	if remaining := got.SigningNotAfter.Sub(now()); remaining != ingressKeyLife-ingressKeyRotation || remaining < time.Hour+engine.IngressBearerTTL {
		t.Fatalf("remaining = %v, want the hour of slack and more, %v", remaining, ingressKeyLife-ingressKeyRotation)
	}
	if age := now().Sub(got.NewestCreated); age < ingressKeyRotation {
		t.Fatalf("newest key age = %v, want at least the rotation interval", age)
	}
	if k, _, err := s.SigningKey(now()); err != nil || k != kid {
		t.Fatalf("inside the slack SigningKey = %q, %v; the gauges must show the stall while it still answers", k, err)
	}
}
