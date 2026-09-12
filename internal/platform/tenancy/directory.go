package tenancy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"gorm.io/gorm"
)

// Until now a tenant was any string that reached the edge. Nothing checked that
// it named a tenant somebody had provisioned, which had two consequences:
//
//   - Registering with an arbitrary tenant silently created a working tenant.
//     Isolation held — that data was correctly scoped — but scoped to something
//     nobody had approved, billed, or could find in a list.
//   - A tenant could not be turned off. Suspending one meant deleting its users,
//     because the identifier itself carried no state.
//
// The directory is the missing record: the set of tenants that exist, and their
// status. It is deliberately small. It is not a customer table, a billing
// record, or a settings store — those belong to whatever product is built on
// the kernel. It answers one question, asked on every unauthenticated request:
// may this tenant be used right now.

// TenantStatus is a tenant's lifecycle state.
type TenantStatus string

const (
	// StatusActive is the only status that admits new sessions.
	StatusActive TenantStatus = "active"

	// StatusSuspended blocks login and registration while preserving data.
	// This is what makes non-payment or an investigation reversible: a
	// suspension that deleted rows would not be.
	StatusSuspended TenantStatus = "suspended"

	// StatusArchived is a tenant kept only for records. It is refused like a
	// suspension but distinguished from one so an operator can tell an ended
	// relationship from a paused one.
	StatusArchived TenantStatus = "archived"
)

// Tenant is a provisioned tenant.
//
// This table is global by nature — it is the list of tenants, so scoping it to a
// tenant would be circular — and is therefore never registered as scoped.
type Tenant struct {
	ID        string
	Name      string
	Status    TenantStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (Tenant) TableName() string { return "tenants" }

var (
	// ErrUnknownTenant is returned when an identifier names no provisioned
	// tenant. It is deliberately distinct from a suspension: one is a caller
	// naming something that never existed, the other is an operator decision.
	ErrUnknownTenant = errors.New("tenancy: unknown tenant")

	// ErrTenantNotActive is returned when a tenant exists but may not be used.
	ErrTenantNotActive = errors.New("tenancy: tenant is not active")
)

// DirectoryTTL is how long a tenant's status is trusted without re-reading it.
//
// A suspension takes effect within this window rather than immediately. That is
// the same trade the session revocation list makes, for the same reason: the
// alternative is a database read on every unauthenticated request, and a
// suspension is not usually an emergency measured in seconds.
var DirectoryTTL = 60 * time.Second

// Directory answers whether a tenant exists and may be used.
//
// Safe for concurrent use: it sits on the request path.
type Directory struct {
	db *gorm.DB

	mu    sync.RWMutex
	cache map[string]cachedTenant
	now   func() time.Time
}

type cachedTenant struct {
	status    TenantStatus
	found     bool
	checkedAt time.Time
}

// NewDirectory builds a directory over db.
func NewDirectory(db *gorm.DB) *Directory {
	return &Directory{
		db:    db,
		cache: make(map[string]cachedTenant),
		now:   time.Now,
	}
}

// Check reports whether a tenant exists and is active.
//
// A database error is returned rather than swallowed. Treating an unreachable
// database as "tenant is fine" would make an outage silently readmit every
// suspended tenant, which is the failure this exists to prevent.
func (d *Directory) Check(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return fmt.Errorf("%w: no tenant named", ErrUnknownTenant)
	}

	d.mu.RLock()
	entry, ok := d.cache[tenantID]
	d.mu.RUnlock()
	if ok && d.now().Sub(entry.checkedAt) < DirectoryTTL {
		return statusError(tenantID, entry.status, entry.found)
	}

	// The lookup spans the tenant list itself, which is global by nature.
	var record Tenant
	err := d.db.WithContext(WithSystemContext(ctx)).
		Where("id = ?", tenantID).
		First(&record).Error

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		d.remember(tenantID, cachedTenant{found: false, checkedAt: d.now()})
		return fmt.Errorf("%w: %s", ErrUnknownTenant, tenantID)
	case err != nil:
		return fmt.Errorf("tenancy: look up tenant %s: %w", tenantID, err)
	}

	d.remember(tenantID, cachedTenant{status: record.Status, found: true, checkedAt: d.now()})
	return statusError(tenantID, record.Status, true)
}

func (d *Directory) remember(tenantID string, entry cachedTenant) {
	d.mu.Lock()
	d.cache[tenantID] = entry
	d.mu.Unlock()
}

func statusError(tenantID string, status TenantStatus, found bool) error {
	if !found {
		return fmt.Errorf("%w: %s", ErrUnknownTenant, tenantID)
	}
	if status != StatusActive {
		return fmt.Errorf("%w: %s is %s", ErrTenantNotActive, tenantID, status)
	}
	return nil
}

// Provision creates a tenant, or returns the existing one unchanged.
//
// Creating is idempotent because provisioning is often driven by an external
// system that retries. It deliberately does not reactivate a suspended tenant:
// re-running provisioning must not quietly undo an operator's suspension.
func (d *Directory) Provision(ctx context.Context, tenantID, name string) error {
	if tenantID == "" {
		return fmt.Errorf("%w: cannot provision a tenant with no ID", ErrUnknownTenant)
	}

	record := Tenant{
		ID:        tenantID,
		Name:      name,
		Status:    StatusActive,
		CreatedAt: d.now(),
		UpdatedAt: d.now(),
	}
	err := d.db.WithContext(WithSystemContext(ctx)).
		Where("id = ?", tenantID).
		FirstOrCreate(&record).Error
	if err != nil {
		return fmt.Errorf("tenancy: provision tenant %s: %w", tenantID, err)
	}

	d.invalidate(tenantID)
	return nil
}

// SetStatus changes a tenant's status.
//
// The cache entry is dropped rather than updated, so the next check re-reads.
// Writing the new status into the cache would be correct in this process and
// wrong in every other one, which is a difference worth not introducing.
func (d *Directory) SetStatus(ctx context.Context, tenantID string, status TenantStatus) error {
	result := d.db.WithContext(WithSystemContext(ctx)).
		Model(&Tenant{}).
		Where("id = ?", tenantID).
		Updates(map[string]any{"status": status, "updated_at": d.now()})
	if result.Error != nil {
		return fmt.Errorf("tenancy: set status of %s: %w", tenantID, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", ErrUnknownTenant, tenantID)
	}

	d.invalidate(tenantID)
	return nil
}

func (d *Directory) invalidate(tenantID string) {
	d.mu.Lock()
	delete(d.cache, tenantID)
	d.mu.Unlock()
}

// ValidatingResolver wraps a tenant.Resolver and refuses tenants that are not
// provisioned and active.
//
// It is a wrapper rather than a replacement so the resolution strategy — a
// subdomain, a header, whatever a deployment chooses — stays independent of the
// question of whether the result is usable.
type ValidatingResolver struct {
	inner     tenant.Resolver
	directory *Directory
}

// NewValidatingResolver wraps inner with a directory check.
func NewValidatingResolver(inner tenant.Resolver, directory *Directory) *ValidatingResolver {
	return &ValidatingResolver{inner: inner, directory: directory}
}

// Resolve returns the tenant only if it exists and is active.
func (r *ValidatingResolver) Resolve(ctx context.Context, info tenant.ResolveInfo) (string, error) {
	tenantID, err := r.inner.Resolve(ctx, info)
	if err != nil {
		return "", err
	}
	if tenantID == "" {
		return "", nil
	}
	if err := r.directory.Check(ctx, tenantID); err != nil {
		return "", err
	}
	return tenantID, nil
}
