package pgstore

import (
	"context"
	"time"
)

// storeTimeout bounds every call on the shared-state stores (key, replay,
// exchange); the engine seams carry no ctx. Deliberately not below RDS Multi-AZ
// failover latency for one statement on a warm pool: a lower bound would turn a
// routine failover into a burst of rejections rather than slow calls.
const storeTimeout = 2 * time.Second

// The store names these seams report through their error hooks. They MUST match the names
// the engine reports its own store failures under (engine.Config.StoreErrorMetric
// documents the set), since both feed one counter's `store` dimension: the app layer's EMF
// row pins the strings.
//
// A hook exists only where a failure never reaches the engine at all: the exchange seam's
// best-effort Begin insert, and the ingress key store's BACKGROUND refresh loop (no
// request is waiting on it, so nothing is returned). Every request-path failure of the key
// store and the replay store is RETURNED to the engine, which counts it once, there.
const (
	storeNameIngressKey = "ingresskey"
	storeNameExchange   = "exchange"
)

func storeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storeTimeout)
}
