package tenancy_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/glebarez/sqlite"
	"github.com/sannados/sannad/internal/app/migrations"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"gorm.io/gorm"
)

// Before the directory, any string was a valid tenant: registering with an
// arbitrary name silently created one, and a tenant could not be turned off.

func directoryDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "directory.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	if err := migrations.RunMigrations(sqlDB, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// TestUnknownTenantIsRefused is the gap this closes. An unprovisioned tenant
// used to work: isolation scoped its data correctly, to something nobody had
// approved or could find in a list.
func TestUnknownTenantIsRefused(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))

	err := dir.Check(context.Background(), "never-provisioned")
	if !errors.Is(err, tenancy.ErrUnknownTenant) {
		t.Fatalf("expected ErrUnknownTenant, got %v", err)
	}
}

func TestProvisionedTenantIsAccepted(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))
	ctx := context.Background()

	if err := dir.Provision(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := dir.Check(ctx, "tenant-a"); err != nil {
		t.Fatalf("a provisioned tenant was refused: %v", err)
	}
}

// TestSuspendedTenantIsRefused: suspension must block new sessions while
// preserving data, which is what makes it reversible.
func TestSuspendedTenantIsRefused(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))
	ctx := context.Background()

	if err := dir.Provision(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := dir.SetStatus(ctx, "tenant-a", tenancy.StatusSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	err := dir.Check(ctx, "tenant-a")
	if !errors.Is(err, tenancy.ErrTenantNotActive) {
		t.Fatalf("expected ErrTenantNotActive, got %v", err)
	}
	// Distinct from unknown: one is a caller naming something that never
	// existed, the other is an operator decision, and an auth log must not
	// conflate them.
	if errors.Is(err, tenancy.ErrUnknownTenant) {
		t.Error("a suspension was reported as an unknown tenant")
	}
}

// TestSuspensionIsVisibleImmediately: an operator suspending a tenant during an
// incident must not wait out a cache window in the process that did it.
func TestSuspensionIsVisibleImmediately(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))
	ctx := context.Background()

	if err := dir.Provision(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Warm the cache with the accepting answer, which is the one that would
	// otherwise be served stale.
	if err := dir.Check(ctx, "tenant-a"); err != nil {
		t.Fatalf("check: %v", err)
	}

	if err := dir.SetStatus(ctx, "tenant-a", tenancy.StatusSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := dir.Check(ctx, "tenant-a"); !errors.Is(err, tenancy.ErrTenantNotActive) {
		t.Fatalf("a just-suspended tenant was still accepted from cache: %v", err)
	}
}

// TestReactivationWorks: suspension has to be reversible or it is deletion.
func TestReactivationWorks(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))
	ctx := context.Background()

	if err := dir.Provision(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := dir.SetStatus(ctx, "tenant-a", tenancy.StatusSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := dir.SetStatus(ctx, "tenant-a", tenancy.StatusActive); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if err := dir.Check(ctx, "tenant-a"); err != nil {
		t.Fatalf("a reactivated tenant is still refused: %v", err)
	}
}

// TestProvisionDoesNotUndoASuspension. Provisioning is often driven by an
// external system that retries; a retry must not quietly reactivate a tenant an
// operator suspended.
func TestProvisionDoesNotUndoASuspension(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))
	ctx := context.Background()

	if err := dir.Provision(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := dir.SetStatus(ctx, "tenant-a", tenancy.StatusSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	if err := dir.Provision(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatalf("re-provision: %v", err)
	}

	if err := dir.Check(ctx, "tenant-a"); !errors.Is(err, tenancy.ErrTenantNotActive) {
		t.Fatalf("re-provisioning reactivated a suspended tenant: %v", err)
	}
}

func TestSetStatusOnUnknownTenantIsRefused(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))
	err := dir.SetStatus(context.Background(), "nope", tenancy.StatusSuspended)
	if !errors.Is(err, tenancy.ErrUnknownTenant) {
		t.Fatalf("expected ErrUnknownTenant, got %v", err)
	}
}

// TestDirectoryFailsClosedOnDatabaseError: treating an unreachable database as
// "the tenant is fine" would silently readmit every suspended tenant at once.
func TestDirectoryFailsClosedOnDatabaseError(t *testing.T) {
	db := directoryDB(t)
	dir := tenancy.NewDirectory(db)
	ctx := context.Background()

	if err := dir.Provision(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Suspend, then break the database, so the cached answer is not what makes
	// this pass.
	if err := dir.SetStatus(ctx, "tenant-a", tenancy.StatusSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := dir.Check(ctx, "tenant-a"); err == nil {
		t.Fatal("an unreachable database was reported as an acceptable tenant")
	}
}

// staticResolver stands in for whatever strategy a deployment uses.
type staticResolver struct {
	tenantID string
	err      error
}

func (r staticResolver) Resolve(context.Context, tenant.ResolveInfo) (string, error) {
	return r.tenantID, r.err
}

// TestValidatingResolverRefusesUnknownTenants is the edge-level guarantee: the
// refusal happens before any login flow runs, not after.
func TestValidatingResolverRefusesUnknownTenants(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))
	resolver := tenancy.NewValidatingResolver(staticResolver{tenantID: "ghost"}, dir)

	req, _ := http.NewRequest(http.MethodGet, "https://ghost.example.com/", nil)
	if _, err := resolver.Resolve(context.Background(), tenant.ResolveInfoFromRequest(req)); !errors.Is(err, tenancy.ErrUnknownTenant) {
		t.Fatalf("expected ErrUnknownTenant, got %v", err)
	}
}

func TestValidatingResolverPassesActiveTenants(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))
	ctx := context.Background()
	if err := dir.Provision(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatalf("provision: %v", err)
	}

	resolver := tenancy.NewValidatingResolver(staticResolver{tenantID: "tenant-a"}, dir)
	req, _ := http.NewRequest(http.MethodGet, "https://tenant-a.example.com/", nil)

	got, err := resolver.Resolve(ctx, tenant.ResolveInfoFromRequest(req))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "tenant-a" {
		t.Fatalf("resolved %q, want tenant-a", got)
	}
}

// TestValidatingResolverPassesEmptyThrough: a deployment with no subdomain
// tenant falls back to the request body, and an empty result is "not resolved
// here", not "unknown tenant".
func TestValidatingResolverPassesEmptyThrough(t *testing.T) {
	dir := tenancy.NewDirectory(directoryDB(t))
	resolver := tenancy.NewValidatingResolver(staticResolver{tenantID: ""}, dir)

	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	got, err := resolver.Resolve(context.Background(), tenant.ResolveInfoFromRequest(req))
	if err != nil {
		t.Fatalf("an unresolved tenant became an error: %v", err)
	}
	if got != "" {
		t.Fatalf("resolved %q, want empty", got)
	}
}
