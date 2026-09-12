package modulekit

import "context"

// CallerInfo carries authenticated caller identity into capability handlers.
// The gateway auth middleware injects it before dispatching to Bus.Call;
// module handlers read it with CallerFrom.
type CallerInfo struct {
	// Subject is the identity ID of the authenticated caller (from the session).
	Subject string
	// TenantID is the resolved tenant context, empty if not tenant-scoped.
	TenantID string
}

type callerContextKey struct{}

// WithCaller returns a child context with the caller's identity attached.
// Call this before Bus.Call so that capability handlers can identify the caller.
func WithCaller(ctx context.Context, info CallerInfo) context.Context {
	return context.WithValue(ctx, callerContextKey{}, info)
}

// CallerFrom extracts caller identity from the context.
// Returns (info, true) if a caller was injected, (zero, false) otherwise.
// The false case covers direct in-process calls that bypass the gateway.
func CallerFrom(ctx context.Context) (CallerInfo, bool) {
	info, ok := ctx.Value(callerContextKey{}).(CallerInfo)
	return info, ok
}
