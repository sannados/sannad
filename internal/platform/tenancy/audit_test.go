package tenancy_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"gorm.io/gorm"
)

// auditScopedModel is registered as tenant-scoped, the correct case.
type auditScopedModel struct {
	ID       string `gorm:"primaryKey"`
	TenantID string `gorm:"index"`
	Name     string
}

func (r *auditScopedModel) GetTenantID() string   { return r.TenantID }
func (r *auditScopedModel) SetTenantID(id string) { r.TenantID = id }

// auditForgottenModel carries a tenant column but is never registered. This is the
// case the audit exists to find: it compiles, it runs, and every query against
// it silently spans every tenant.
type auditForgottenModel struct {
	ID       string `gorm:"primaryKey"`
	TenantID string `gorm:"index"`
	Name     string
}

// auditGlobalModel has no tenant column and is legitimately global. The audit must
// stay quiet about it, or operators learn to ignore the warning.
type auditGlobalModel struct {
	ID   string `gorm:"primaryKey"`
	Name string
}

func auditDB(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := tenancy.WithSystemContext(context.Background())
	if err := db.WithContext(ctx).AutoMigrate(models...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestAuditFindsAnUnregisteredTenantTable is the whole point: a table whose
// author clearly intended per-tenant data, with nothing enforcing it.
func TestAuditFindsAnUnregisteredTenantTable(t *testing.T) {
	db := auditDB(t, &auditForgottenModel{}, &auditGlobalModel{})

	findings, err := tenancy.AuditScopedModels(context.Background(), db)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	if len(findings) != 1 {
		t.Fatalf("found %d findings, want 1: %v", len(findings), findings)
	}
	if findings[0].Table != "audit_forgotten_models" {
		t.Fatalf("found %q, want audit_forgotten_models", findings[0].Table)
	}
	if findings[0].Column != "tenant_id" {
		t.Errorf("column = %q, want tenant_id", findings[0].Column)
	}
}

// TestAuditIsQuietAboutRegisteredModels: a scoped model must not be reported, or
// every correct module produces noise and the real finding is lost in it.
func TestAuditIsQuietAboutRegisteredModels(t *testing.T) {
	db := auditDB(t, &auditScopedModel{})
	if err := tenancy.RegisterScopedModels(db, &auditScopedModel{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	findings, err := tenancy.AuditScopedModels(context.Background(), db)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, f := range findings {
		if f.Table == "audit_scoped_models" {
			t.Fatalf("registered model reported as unscoped: %v", f)
		}
	}
}

// TestAuditIsQuietAboutGlobalTables: plenty of tables are legitimately global —
// migrations, feature flags, the tenant directory itself.
func TestAuditIsQuietAboutGlobalTables(t *testing.T) {
	db := auditDB(t, &auditGlobalModel{})

	findings, err := tenancy.AuditScopedModels(context.Background(), db)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("global table reported: %v", findings)
	}
}

// TestAuditRunsWithoutTenantContext: the audit runs at startup, before any
// request exists, so it must not need a tenant.
//
// It passes today for a reason worth stating plainly rather than taking credit
// for: GORM's migrator issues schema introspection outside the query callbacks,
// so isolation never inspects it. This test pins the behaviour the audit needs
// regardless of which layer currently provides it — if a GORM upgrade routes
// introspection through the callbacks, this fails rather than every boot
// logging an audit error.
func TestAuditRunsWithoutTenantContext(t *testing.T) {
	db := auditDB(t, &auditForgottenModel{})
	if err := tenancy.RegisterIsolation(db); err != nil {
		t.Fatalf("register isolation: %v", err)
	}

	// Plain background context: no tenant, no system marker.
	findings, err := tenancy.AuditScopedModels(context.Background(), db)
	if err != nil {
		t.Fatalf("audit refused to run at startup: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("found %d findings, want 1", len(findings))
	}
}

// TestAuditOrderIsStable: an audit whose output order changes between boots is
// one nobody can diff.
func TestAuditOrderIsStable(t *testing.T) {
	type alphaRecord struct {
		ID       string `gorm:"primaryKey"`
		TenantID string
	}
	type zuluRecord struct {
		ID       string `gorm:"primaryKey"`
		TenantID string
	}
	db := auditDB(t, &zuluRecord{}, &alphaRecord{}, &auditForgottenModel{})

	first, err := tenancy.AuditScopedModels(context.Background(), db)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	second, err := tenancy.AuditScopedModels(context.Background(), db)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("two runs disagreed: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("order differs at %d: %v vs %v", i, first[i], second[i])
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].Table > first[i].Table {
			t.Fatalf("findings are not sorted: %v", first)
		}
	}
}

func TestAuditRefusesNilDatabase(t *testing.T) {
	if _, err := tenancy.AuditScopedModels(context.Background(), nil); err == nil {
		t.Fatal("expected an error for a nil database")
	}
}

// TestAuditRespectsExemptions: some tables carry a tenant column and genuinely
// cannot be scoped — accounts is looked up by email at login, before any tenant
// context exists. Those must be declarable, because a warning that fires on
// every boot forever is a warning operators filter out, taking the real
// findings with it.
func TestAuditRespectsExemptions(t *testing.T) {
	db := auditDB(t, &auditForgottenModel{})

	findings, err := tenancy.AuditScopedModels(context.Background(), db,
		"audit_forgotten_models")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("exempt table still reported: %v", findings)
	}

	// An exemption must be exact: naming one table must not silence another.
	findings, err = tenancy.AuditScopedModels(context.Background(), db, "something_else")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("an unrelated exemption silenced a real finding: %v", findings)
	}
}
