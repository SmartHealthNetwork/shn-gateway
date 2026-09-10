package pgstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// ingressKeyLockID coordinates creators even when the holder has no rows. Its
// namespace is separate from schema locking; every query still scopes the holder,
// so a hash collision only serializes otherwise independent creators.
func ingressKeyLockID(holderID string) int64 {
	sum := sha256.Sum256([]byte("shn-gateway/ingress-key/adoption/v1\x00" + holderID))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// selectOrCreateKey returns the committed readable winner. Begin, isolation,
// holder lock, selection, insertion and commit share one store deadline. Callers
// publish the result under their cache lock only after this operation succeeds.
func (s *IngressKeyStore) selectOrCreateKey(now time.Time) (string, cachedKey, error) {
	ctx, cancel := storeCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", cachedKey{}, err
	}
	defer tx.Rollback(ctx)
	// Each statement must see commits made while the advisory lock was awaited,
	// including on a pool whose session default was configured more strictly.
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		return "", cachedKey{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, ingressKeyLockID(s.holderID)); err != nil {
		return "", cachedKey{}, err
	}
	rows, err := tx.Query(ctx, `SELECT kid, private_key_pem, created_at, not_after
FROM gw_ingress_key
WHERE holder_id=$1 AND not_after > $2
ORDER BY created_at DESC, kid DESC`, s.holderID, now)
	if err != nil {
		return "", cachedKey{}, err
	}
	var kid string
	var chosen cachedKey
	for rows.Next() {
		var candidate, pemStr string
		var created, notAfter time.Time
		if err := rows.Scan(&candidate, &pemStr, &created, &notAfter); err != nil {
			rows.Close()
			return "", cachedKey{}, err
		}
		if kid != "" || !now.Before(created.Add(ingressKeyRotation)) {
			continue
		}
		key, err := parsePKCS8EC(pemStr)
		if err != nil {
			// Isolate unreadable rows without logging key material.
			continue
		}
		kid, chosen = candidate, cachedKey{key: key, created: created, notAfter: notAfter}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", cachedKey{}, err
	}
	if kid == "" {
		key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			return "", cachedKey{}, err
		}
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", cachedKey{}, err
		}
		kid = hex.EncodeToString(b[:])
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return "", cachedKey{}, err
		}
		pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		notAfter := now.Add(ingressKeyRotation + engine.IngressBearerTTL + time.Minute)
		if _, err := tx.Exec(ctx, `DELETE FROM gw_ingress_key WHERE holder_id=$1 AND not_after <= $2`, s.holderID, now); err != nil {
			return "", cachedKey{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO gw_ingress_key (holder_id, kid, private_key_pem, created_at, not_after) VALUES ($1,$2,$3,$4,$5)`,
			s.holderID, kid, pemStr, now, notAfter); err != nil {
			return "", cachedKey{}, err
		}
		chosen = cachedKey{key: key, created: now, notAfter: notAfter}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", cachedKey{}, err
	}
	return kid, chosen, nil
}
