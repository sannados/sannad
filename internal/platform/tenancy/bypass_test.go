package tenancy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sannados/sannad/internal/platform/tenancy"
)

// TestRawSQLIsRefused covers the paths where a tenant predicate cannot be
// injected. Raw statements are opaque to GORM, so the only safe answer is to
// refuse them rather than run them unscoped.
//
// Each case here leaked every tenant's rows before the guard existed.
func TestRawSQLIsRefused(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)
	ctx := tenantCtx("tenant-a")

	t.Run("Raw().Scan()", func(t *testing.T) {
		var got []scopedRecord
		err := db.WithContext(ctx).Raw("SELECT * FROM scoped_records").Scan(&got).Error
		if !errors.Is(err, tenancy.ErrRawSQLOnScopedTable) {
			t.Fatalf("expected ErrRawSQLOnScopedTable, got %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("raw select returned %d rows: %+v", len(got), got)
		}
	})

	t.Run("Exec()", func(t *testing.T) {
		err := db.WithContext(ctx).Exec("UPDATE scoped_records SET value = 'overwritten'").Error
		if !errors.Is(err, tenancy.ErrRawSQLOnScopedTable) {
			t.Fatalf("expected ErrRawSQLOnScopedTable, got %v", err)
		}

		// Nothing was modified, including the caller's own rows.
		var victim scopedRecord
		sys := tenancy.WithSystemContext(context.Background())
		if err := db.WithContext(sys).First(&victim, "id = ?", "b1").Error; err != nil {
			t.Fatalf("read victim: %v", err)
		}
		if victim.Value != "bravo-one" {
			t.Fatalf("raw update reached another tenant: %+v", victim)
		}
	})

	t.Run("raw SQL is allowed under a system context", func(t *testing.T) {
		var got []scopedRecord
		sys := tenancy.WithSystemContext(context.Background())
		if err := db.WithContext(sys).Raw("SELECT * FROM scoped_records").Scan(&got).Error; err != nil {
			t.Fatalf("system-context raw select: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected all 3 rows under system context, got %d", len(got))
		}
	})
}

// TestTableByNameIsScoped covers queries that name their table as a string and
// so carry no model for the TenantAware check to inspect. Without the scoped
// table registry these ran unscoped.
func TestTableByNameIsScoped(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)

	var got []map[string]any
	if err := db.WithContext(tenantCtx("tenant-a")).
		Table("scoped_records").Find(&got).Error; err != nil {
		t.Fatalf("Table() query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows for tenant-a, got %d: %+v", len(got), got)
	}
	for _, row := range got {
		if row["tenant_id"] != "tenant-a" {
			t.Fatalf("Table() query leaked a row from %v", row["tenant_id"])
		}
	}
}

// TestCallerPredicateCannotEscapeScope confirms the isolation predicate is
// ANDed with the caller's own, and cannot be widened by an OR.
func TestCallerPredicateCannotEscapeScope(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)

	var got []scopedRecord
	if err := db.WithContext(tenantCtx("tenant-a")).
		Where("1=1 OR tenant_id = ?", "tenant-b").
		Find(&got).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if r.TenantID != "tenant-a" {
			t.Fatalf("OR predicate escaped the tenant scope: %+v", r)
		}
	}
}

// TestAggregatesAreScoped confirms that Count and Pluck, which read through
// different GORM paths than Find, are scoped too. An unscoped Count discloses
// how many rows other tenants hold.
func TestAggregatesAreScoped(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)
	ctx := tenantCtx("tenant-a")

	var n int64
	if err := db.WithContext(ctx).Model(&scopedRecord{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("Count returned %d, want 2", n)
	}

	var values []string
	if err := db.WithContext(ctx).Model(&scopedRecord{}).Pluck("value", &values).Error; err != nil {
		t.Fatalf("pluck: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("Pluck returned %d values, want 2: %v", len(values), values)
	}
}

// TestJoinToScopedTableIsRefused covers a query whose primary table is unscoped
// but which reaches scoped rows through a JOIN.
//
// The isolation predicate names the primary table, so before this guard
// `Table("global_records").Joins("CROSS JOIN scoped_records")` returned every
// tenant's scoped rows. Inferring the right predicate from an arbitrary join
// expression is not worth getting subtly wrong, so the query is refused.
func TestJoinToScopedTableIsRefused(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)

	// A row on the unscoped side, so an unguarded cross join would produce rows.
	sys := tenancy.WithSystemContext(context.Background())
	if err := db.WithContext(sys).Create(&globalRecord{ID: "g1", Value: "global"}).Error; err != nil {
		t.Fatalf("seed global record: %v", err)
	}

	var got []map[string]any
	err := db.WithContext(tenantCtx("tenant-a")).
		Table("global_records").
		Joins("CROSS JOIN scoped_records").
		Find(&got).Error

	if !errors.Is(err, tenancy.ErrJoinOnScopedTable) {
		t.Fatalf("expected ErrJoinOnScopedTable, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("join to scoped table returned %d rows: %+v", len(got), got)
	}
}

// TestJoinFromScopedTableStillWorks is the counterpart: a scoped table joining
// out to an unscoped one is legitimate and stays scoped. Without this, the
// guard above could pass by refusing every join.
func TestJoinFromScopedTableStillWorks(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)

	sys := tenancy.WithSystemContext(context.Background())
	if err := db.WithContext(sys).Create(&globalRecord{ID: "g1", Value: "global"}).Error; err != nil {
		t.Fatalf("seed global record: %v", err)
	}

	var got []map[string]any
	if err := db.WithContext(tenantCtx("tenant-a")).
		Table("scoped_records").
		Joins("CROSS JOIN global_records").
		Find(&got).Error; err != nil {
		t.Fatalf("scoped table joining outward: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows for tenant-a, got %d", len(got))
	}
	for _, row := range got {
		if row["tenant_id"] != "tenant-a" {
			t.Fatalf("join leaked a row from %v", row["tenant_id"])
		}
	}
}

// TestScopedDestTypesAreRecognised covers destination types that carry no
// TenantAware implementation of their own — a plain struct, a map — pointed at
// a scoped table by name. The scoped-table registry is what catches these.
func TestScopedDestTypesAreRecognised(t *testing.T) {
	db := newDB(t, true)
	seed(t, db)

	t.Run("plain struct destination", func(t *testing.T) {
		type plainRow struct {
			ID       string `gorm:"primaryKey"`
			TenantID string
			Value    string
		}
		var got []plainRow
		if err := db.WithContext(tenantCtx("tenant-a")).
			Table("scoped_records").Find(&got).Error; err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 rows, got %d", len(got))
		}
		for _, r := range got {
			if r.TenantID != "tenant-a" {
				t.Fatalf("leaked row from %q", r.TenantID)
			}
		}
	})

	t.Run("map destination", func(t *testing.T) {
		var got []map[string]any
		if err := db.WithContext(tenantCtx("tenant-a")).
			Table("scoped_records").Find(&got).Error; err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 rows, got %d", len(got))
		}
	})
}
