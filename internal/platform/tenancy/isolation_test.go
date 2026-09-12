package tenancy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/glebarez/sqlite"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"gorm.io/gorm"
)

// scopedRecord is a tenant-scoped model.
type scopedRecord struct {
	ID       string `gorm:"primaryKey"`
	TenantID string `gorm:"index;not null"`
	Value    string
}

func (r *scopedRecord) GetTenantID() string   { return r.TenantID }
func (r *scopedRecord) SetTenantID(id string) { r.TenantID = id }

// globalRecord is deliberately not tenant-scoped, standing in for system tables
// such as accounts and migrations.
type globalRecord struct {
	ID    string `gorm:"primaryKey"`
	Value string
}

// newDB returns an in-memory database with isolation installed, matching how
// bootstrap wires the real connection.
func newDB(t *testing.T, withIsolation bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&scopedRecord{}, &globalRecord{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if withIsolation {
		if err := tenancy.RegisterIsolation(db); err != nil {
			t.Fatalf("register isolation: %v", err)
		}
		if err := tenancy.RegisterScopedModels(db, &scopedRecord{}); err != nil {
			t.Fatalf("register scoped models: %v", err)
		}
	}
	return db
}

// seed inserts rows for two tenants using the system context, bypassing
// isolation the way a migration or fixture loader legitimately would.
func seed(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := tenancy.WithSystemContext(context.Background())
	rows := []scopedRecord{
		{ID: "a1", TenantID: "tenant-a", Value: "alpha-one"},
		{ID: "a2", TenantID: "tenant-a", Value: "alpha-two"},
		{ID: "b1", TenantID: "tenant-b", Value: "bravo-one"},
	}
	if err := db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func tenantCtx(id string) context.Context {
	return tenant.WithTenantID(context.Background(), id)
}

// TestCrossTenantReadIsImpossible is the adversarial case from ADR 0005: a
// caller authenticated as tenant-a attempting to reach tenant-b's rows.
//
// It asserts on the returned rows, not only on the error. An implementation
// that returned every row while also returning an error would pass a
// error-only assertion and still be a data leak.
func TestCrossTenantReadIsImpossible(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)

	t.Run("scoped read returns only the caller's rows", func(t *testing.T) {
		var got []scopedRecord
		if err := db.WithContext(tenantCtx("tenant-a")).Find(&got).Error; err != nil {
			t.Fatalf("read as tenant-a: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 rows for tenant-a, got %d: %+v", len(got), got)
		}
		for _, r := range got {
			if r.TenantID != "tenant-a" {
				t.Fatalf("leaked row from %q: %+v", r.TenantID, r)
			}
		}
	})

	t.Run("explicit predicate for another tenant returns nothing", func(t *testing.T) {
		// The caller's own WHERE is ANDed with the isolation predicate rather
		// than replacing it, so naming another tenant yields the empty set.
		var got []scopedRecord
		err := db.WithContext(tenantCtx("tenant-a")).
			Where("tenant_id = ?", "tenant-b").
			Find(&got).Error
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("cross-tenant predicate returned %d rows: %+v", len(got), got)
		}
	})

	t.Run("fetch by another tenant's primary key returns nothing", func(t *testing.T) {
		var got scopedRecord
		err := db.WithContext(tenantCtx("tenant-a")).First(&got, "id = ?", "b1").Error
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("expected ErrRecordNotFound, got err=%v row=%+v", err, got)
		}
	})
}

// TestUnscopedContextFails covers the case that made the original defect
// dangerous: with no tenant resolved, the query ran with no predicate at all
// and returned every tenant's rows.
func TestUnscopedContextFails(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)

	var got []scopedRecord
	err := db.WithContext(context.Background()).Find(&got).Error
	if !errors.Is(err, tenancy.ErrNoTenantContext) {
		t.Fatalf("expected ErrNoTenantContext, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unscoped query returned %d rows: %+v", len(got), got)
	}
}

// TestIsolationAppliesToWrites verifies that update and delete are scoped too.
// A read-only guarantee would still allow one tenant to destroy another's data.
func TestIsolationAppliesToWrites(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)

	t.Run("update cannot reach another tenant", func(t *testing.T) {
		err := db.WithContext(tenantCtx("tenant-a")).
			Model(&scopedRecord{}).
			Where("id = ?", "b1").
			Update("value", "overwritten").Error
		if err != nil {
			t.Fatalf("update: %v", err)
		}

		var victim scopedRecord
		sys := tenancy.WithSystemContext(context.Background())
		if err := db.WithContext(sys).First(&victim, "id = ?", "b1").Error; err != nil {
			t.Fatalf("read victim: %v", err)
		}
		if victim.Value != "bravo-one" {
			t.Fatalf("tenant-a modified tenant-b's row: %+v", victim)
		}
	})

	t.Run("delete cannot reach another tenant", func(t *testing.T) {
		err := db.WithContext(tenantCtx("tenant-a")).
			Where("id = ?", "b1").
			Delete(&scopedRecord{}).Error
		if err != nil {
			t.Fatalf("delete: %v", err)
		}

		var count int64
		sys := tenancy.WithSystemContext(context.Background())
		if err := db.WithContext(sys).Model(&scopedRecord{}).
			Where("id = ?", "b1").Count(&count).Error; err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 1 {
			t.Fatal("tenant-a deleted tenant-b's row")
		}
	})

	t.Run("insert is stamped with the context tenant", func(t *testing.T) {
		rec := scopedRecord{ID: "a3", Value: "alpha-three"}
		if err := db.WithContext(tenantCtx("tenant-a")).Create(&rec).Error; err != nil {
			t.Fatalf("create: %v", err)
		}
		if rec.TenantID != "tenant-a" {
			t.Fatalf("expected tenant stamped on insert, got %q", rec.TenantID)
		}
	})

	t.Run("insert into another tenant is rejected", func(t *testing.T) {
		rec := scopedRecord{ID: "x1", TenantID: "tenant-b", Value: "smuggled"}
		err := db.WithContext(tenantCtx("tenant-a")).Create(&rec).Error
		if !errors.Is(err, tenancy.ErrTenantMismatch) {
			t.Fatalf("expected ErrTenantMismatch, got %v", err)
		}
	})

	t.Run("insert without tenant context is rejected", func(t *testing.T) {
		rec := scopedRecord{ID: "x2", Value: "orphan"}
		err := db.WithContext(context.Background()).Create(&rec).Error
		if !errors.Is(err, tenancy.ErrNoTenantContext) {
			t.Fatalf("expected ErrNoTenantContext, got %v", err)
		}
	})
}

// TestUnscopedModelsAreUntouched confirms that models which do not implement
// TenantAware still work without a tenant. Accounts and migration bookkeeping
// depend on this.
func TestUnscopedModelsAreUntouched(t *testing.T) {
	db := newDB(t, true)

	if err := db.WithContext(context.Background()).
		Create(&globalRecord{ID: "g1", Value: "global"}).Error; err != nil {
		t.Fatalf("create global record: %v", err)
	}

	var got []globalRecord
	if err := db.WithContext(context.Background()).Find(&got).Error; err != nil {
		t.Fatalf("read global records: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 global record, got %d", len(got))
	}
}

// TestSystemContextCrossesTenants confirms the escape hatch works, since
// migrations and platform administration depend on it.
func TestSystemContextCrossesTenants(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)

	var got []scopedRecord
	ctx := tenancy.WithSystemContext(context.Background())
	if err := db.WithContext(ctx).Find(&got).Error; err != nil {
		t.Fatalf("system read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected all 3 rows under system context, got %d", len(got))
	}
}

// TestIsolationIsWhatEnforcesScoping is the negative control ADR 0005 requires.
//
// It runs the same cross-tenant read against a database with NO isolation
// registered and asserts the leak occurs. If this test ever fails, the other
// tests in this file are passing for some reason other than the isolation
// callbacks, and their guarantee is not real.
func TestIsolationIsWhatEnforcesScoping(t *testing.T) {
	db := newDB(t, false)
	seed(t, db)

	var got []scopedRecord
	if err := db.WithContext(tenantCtx("tenant-a")).Find(&got).Error; err != nil {
		t.Fatalf("unisolated read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected the unisolated leak to return all 3 rows, got %d; "+
			"isolation may be enforced somewhere other than RegisterIsolation", len(got))
	}
}
