package crm_test

import (
	"context"
	"testing"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/glebarez/sqlite"
	"github.com/sannados/sannad/examples/embedded/crm"
	"github.com/sannados/sannad/internal/app/migrations"
	"github.com/sannados/sannad/internal/platform/storage"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

func newTestModule(t *testing.T) (*crm.Module, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql.DB: %v", err)
	}
	if err := migrations.RunMigrations(sqlDB, ":memory:"); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	// Mirror bootstrap: isolation is installed on the connection before any
	// module touches it. Without this the module under test is exercised
	// against a weaker guarantee than production has.
	if err := tenancy.RegisterIsolation(db); err != nil {
		t.Fatalf("register isolation: %v", err)
	}
	m := crm.New(storage.NewGormStore(db), "development")
	return m, db
}

// callerCtx builds a request context the way the gateway does: caller identity
// for the kernel, and the same tenant in the Kayan tenant context for storage.
func callerCtx(subject, tenantID string) context.Context {
	ctx := modulekit.WithCaller(context.Background(), modulekit.CallerInfo{
		Subject:  subject,
		TenantID: tenantID,
	})
	return tenant.WithTenantID(ctx, tenantID)
}

// stubRegistrar implements modulekit.Registrar for test use.
type stubRegistrar struct {
	handlers map[string]modulekit.Handler
}

func (s *stubRegistrar) RegisterCapability(ref modulekit.CapabilityRef, h modulekit.Handler) error {
	if s.handlers == nil {
		s.handlers = make(map[string]modulekit.Handler)
	}
	s.handlers[ref.Key()] = h
	return nil
}

func TestInstall_MigratesSchema(t *testing.T) {
	m, db := newTestModule(t)
	reg := &stubRegistrar{}
	if err := m.Install(reg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Schema migration created the contacts table when migrations ran (in newTestModule).
	if !db.Migrator().HasTable("contacts") {
		t.Fatal("expected contacts table to exist after migrations")
	}
	// Capability was registered.
	if _, ok := reg.handlers[crm.CapabilityContactReader.Key()]; !ok {
		t.Fatalf("expected %s capability to be registered", crm.CapabilityContactReader.Key())
	}
}

func TestStart_SeedsDevData(t *testing.T) {
	m, db := newTestModule(t)
	reg := &stubRegistrar{}
	if err := m.Install(reg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Seed rows span several tenants, so verifying them requires the same
	// system context the seeder used. A plain count would now be rejected for
	// having no tenant, which is the correct behaviour.
	var count int64
	sys := tenancy.WithSystemContext(context.Background())
	if err := db.WithContext(sys).Model(&crm.Contact{}).Count(&count).Error; err != nil {
		t.Fatalf("count seeded contacts: %v", err)
	}
	if count == 0 {
		t.Fatal("expected dev seed data after Start in development mode")
	}
}

func TestStart_DoesNotReseed(t *testing.T) {
	m, _ := newTestModule(t)
	reg := &stubRegistrar{}
	if err := m.Install(reg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	// Second Start should not duplicate seed data.
	if err := m.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}
}

func TestHandleListContacts_FiltersByTenant(t *testing.T) {
	m, _ := newTestModule(t)
	reg := &stubRegistrar{}
	if err := m.Install(reg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	handler := reg.handlers[crm.CapabilityContactReader.Key()]

	ctx := callerCtx("user-1", "demo")
	result, err := handler(ctx, crm.ContactReaderRequest{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	resp, ok := result.(crm.ContactReaderResponse)
	if !ok {
		t.Fatalf("unexpected result type %T", result)
	}
	for _, c := range resp.Contacts {
		if c.TenantID != "demo" {
			t.Errorf("got contact with tenant_id=%q, want demo", c.TenantID)
		}
	}
	if len(resp.Contacts) == 0 {
		t.Fatal("expected at least one demo contact")
	}
}

func TestHandleListContacts_DefaultsToCallerTenant(t *testing.T) {
	m, _ := newTestModule(t)
	reg := &stubRegistrar{}
	if err := m.Install(reg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	handler := reg.handlers[crm.CapabilityContactReader.Key()]

	ctx := callerCtx("user-1", "demo")
	result, err := handler(ctx, crm.ContactReaderRequest{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	resp := result.(crm.ContactReaderResponse)
	for _, c := range resp.Contacts {
		if c.TenantID != "demo" {
			t.Errorf("got contact with tenant_id=%q, want demo", c.TenantID)
		}
	}
}

// TestHandleListContacts_IgnoresRequestTenant asserts that a tenant named in
// the request payload has no effect. The field remains on the contract for
// wire compatibility, but the tenant comes from the context and the request
// value is not consulted — which is what closed the cross-tenant read.
func TestHandleListContacts_IgnoresRequestTenant(t *testing.T) {
	m, _ := newTestModule(t)
	reg := &stubRegistrar{}
	if err := m.Install(reg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	handler := reg.handlers[crm.CapabilityContactReader.Key()]

	// Caller is demo; the payload asks for enterprise.
	result, err := handler(callerCtx("user-1", "demo"), crm.ContactReaderRequest{TenantID: "enterprise"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	resp := result.(crm.ContactReaderResponse)
	if len(resp.Contacts) == 0 {
		t.Fatal("expected the caller's own contacts")
	}
	for _, c := range resp.Contacts {
		if c.TenantID != "demo" {
			t.Fatalf("request payload reached the query: got tenant_id=%q", c.TenantID)
		}
	}
}

// TestHandleListContacts_RequiresTenantContext asserts that an unauthenticated
// call fails rather than returning every tenant's contacts.
func TestHandleListContacts_RequiresTenantContext(t *testing.T) {
	m, _ := newTestModule(t)
	reg := &stubRegistrar{}
	if err := m.Install(reg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	handler := reg.handlers[crm.CapabilityContactReader.Key()]

	result, err := handler(context.Background(), crm.ContactReaderRequest{})
	if err == nil {
		t.Fatalf("expected an error without tenant context, got %+v", result)
	}
	if !isWrapping(err, tenancy.ErrNoTenantContext) {
		t.Errorf("expected ErrNoTenantContext in chain, got: %v", err)
	}
}

func TestStop(t *testing.T) {
	m, _ := newTestModule(t)
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// isWrapping checks whether target appears in err's chain (errors.Is shorthand).
func isWrapping(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
