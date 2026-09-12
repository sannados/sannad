package modulekit

import "context"

// EventPublisher is the module-facing asynchronous event surface.
// Modules publish events but do not own or close the deployment's broker.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, payload any) error
}

// TenantAware is the public structural form of Kayan's tenant.TenantAware.
type TenantAware interface {
	GetTenantID() string
	SetTenantID(string)
}

// ScopedModelProvider declares the tenant-owned models a module ships.
// The host registers them before migrations and installation.
type ScopedModelProvider interface {
	ScopedModels() []TenantAware
}

type systemContextKey struct{}

// WithSystemContext marks deliberate trusted work that may span tenants.
// Never derive this from request input. It disables the storage isolation
// boundary for the returned context and is intended only for migrations,
// platform administration, and explicit cross-tenant background jobs.
func WithSystemContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemContextKey{}, true)
}

// IsSystemContext reports whether a trusted caller explicitly enabled
// cross-tenant storage access.
func IsSystemContext(ctx context.Context) bool {
	allowed, _ := ctx.Value(systemContextKey{}).(bool)
	return allowed
}
