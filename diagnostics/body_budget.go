package diagnostics

import "context"

// BodyBudget composes capture's local bounds with its owning gateway's budget.
// Implementations must be prompt and concurrency-safe, and must not call sinks.
type BodyBudget interface {
	TryReserve(int) bool
	Release(int)
}
type bodyBudgetKey struct{}

func WithBodyBudget(ctx context.Context, b BodyBudget) context.Context {
	return context.WithValue(ctx, bodyBudgetKey{}, b)
}
func bodyBudget(ctx context.Context) BodyBudget {
	b, _ := ctx.Value(bodyBudgetKey{}).(BodyBudget)
	return b
}

// WithEventBudget preserves the owner while a bounded sink copies borrowed bytes.
// The reservation identity is never serialized into diagnostic traffic.
func WithEventBudget(e Event, b BodyBudget) Event { e.bodyBudget = b; return e }
