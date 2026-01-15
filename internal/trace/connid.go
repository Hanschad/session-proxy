package trace

import "context"

// ctxKeyConnID is used to attach a SOCKS connection id to a context for correlation.
//
// Note: the key type is unexported to avoid collisions; only helper functions in this
// package should access it.
type ctxKeyConnID struct{}

// WithConnID returns a child context that carries the given connection id.
func WithConnID(ctx context.Context, id uint64) context.Context {
	return context.WithValue(ctx, ctxKeyConnID{}, id)
}

// ConnIDFromContext returns the connection id from context, if present.
func ConnIDFromContext(ctx context.Context) (uint64, bool) {
	v := ctx.Value(ctxKeyConnID{})
	id, ok := v.(uint64)
	return id, ok
}
