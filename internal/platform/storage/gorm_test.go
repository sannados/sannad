package storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/glebarez/sqlite"
	"github.com/sannados/sannad/internal/app/migrations"
	"github.com/sannados/sannad/internal/platform/storage"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

type widget struct {
	ID       string `gorm:"primaryKey"`
	TenantID string `gorm:"index;not null"`
	Name     string
	Size     int
}

func (w *widget) GetTenantID() string   { return w.TenantID }
func (w *widget) SetTenantID(id string) { w.TenantID = id }

// newStore mirrors bootstrap: isolation callbacks and the scoped-model registry
// are installed on the connection, then the Store is built from it.
func newStore(t *testing.T) (modulekit.Store, *gorm.DB) {
	t.Helper()
	// storage.GormConfig, not a bare &gorm.Config{}: the adapter's error
	// translation depends on it, so a test that opened the connection
	// differently would be testing a configuration that never ships.
	// A temp-file database, not ":memory:". Each pooled connection to an
	// in-memory SQLite gets its own private database, so concurrent work sees
	// tables that do not exist. This bit once already, in the step 1 tenancy
	// tests.
	//
	// _busy_timeout is not test tuning: SQLite allows one writer, and without it
	// a concurrent write fails instantly with SQLITE_BUSY rather than waiting its
	// turn. A deployment on SQLite must set it, so a test that omitted it would
	// be exercising a configuration nobody would run — and would report ordinary
	// lock contention as a coordinator defect.
	dbPath := filepath.Join(t.TempDir(), "storage-test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_pragma=busy_timeout(5000)"), storage.GormConfig())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&widget{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The SDK tables come from the real migration, not AutoMigrate. SDK models
	// carry no engine struct tags — the boundary test forbids them — so
	// AutoMigrate would build modulekit_processed_events with no primary key,
	// and the duplicate-key claim that makes Idempotent correct would never
	// conflict. Migration 00004 owns that schema.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	if err := migrations.RunMigrations(sqlDB, dbPath); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if err := tenancy.RegisterIsolation(db); err != nil {
		t.Fatalf("register isolation: %v", err)
	}
	if err := tenancy.RegisterScopedModels(db, &widget{}); err != nil {
		t.Fatalf("register scoped models: %v", err)
	}
	// Close the pool before TempDir cleanup runs, or Windows refuses to unlink
	// a file the process still holds open.
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return storage.NewGormStore(db), db
}

// migrateProcessedEvents registers the SDK idempotency table as tenant-scoped.
// The schema itself was created by migration 00004 in newStore.
func migrateProcessedEvents(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := tenancy.RegisterScopedModels(db, &modulekit.ProcessedEvent{}); err != nil {
		t.Fatalf("register processed events as scoped: %v", err)
	}
}

func seedWidgets(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := tenancy.WithSystemContext(context.Background())
	rows := []widget{
		{ID: "a1", TenantID: "tenant-a", Name: "alpha", Size: 1},
		{ID: "a2", TenantID: "tenant-a", Name: "beta", Size: 5},
		{ID: "b1", TenantID: "tenant-b", Name: "gamma", Size: 9},
	}
	if err := db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func tenantCtx(id string) context.Context {
	return tenant.WithTenantID(context.Background(), id)
}

func TestStoreCRUD(t *testing.T) {
	store, db := newStore(t)
	seedWidgets(t, db)
	ctx := tenantCtx("tenant-a")

	t.Run("Find with no conditions returns the tenant's rows", func(t *testing.T) {
		var got []widget
		if err := store.Find(ctx, &got); err != nil {
			t.Fatalf("Find: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 rows, got %d", len(got))
		}
	})

	t.Run("Find with conditions", func(t *testing.T) {
		var got []widget
		if err := store.Find(ctx, &got, modulekit.Where("name", "alpha")); err != nil {
			t.Fatalf("Find: %v", err)
		}
		if len(got) != 1 || got[0].ID != "a1" {
			t.Fatalf("expected only a1, got %+v", got)
		}
	})

	t.Run("comparison and IN conditions", func(t *testing.T) {
		var got []widget
		if err := store.Find(ctx, &got,
			modulekit.Compare("size", modulekit.OpGreaterThan, 2)); err != nil {
			t.Fatalf("Find: %v", err)
		}
		if len(got) != 1 || got[0].ID != "a2" {
			t.Fatalf("expected only a2, got %+v", got)
		}

		var byID []widget
		if err := store.Find(ctx, &byID, modulekit.In("id", []string{"a1", "a2", "b1"})); err != nil {
			t.Fatalf("Find IN: %v", err)
		}
		// b1 belongs to another tenant and must not appear even though it is named.
		if len(byID) != 2 {
			t.Fatalf("expected 2 rows, got %d: %+v", len(byID), byID)
		}
	})

	t.Run("First returns ErrNotFound rather than a GORM error", func(t *testing.T) {
		var got widget
		err := store.First(ctx, &got, modulekit.Where("name", "nonexistent"))
		if !errors.Is(err, modulekit.ErrNotFound) {
			t.Fatalf("expected modulekit.ErrNotFound, got %v", err)
		}
	})

	t.Run("Count", func(t *testing.T) {
		n, err := store.Count(ctx, &widget{})
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 2 {
			t.Fatalf("Count = %d, want 2", n)
		}
	})

	t.Run("Create stamps the tenant", func(t *testing.T) {
		w := widget{ID: "a3", Name: "delta", Size: 3}
		if err := store.Create(ctx, &w); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if w.TenantID != "tenant-a" {
			t.Fatalf("expected tenant stamped, got %q", w.TenantID)
		}
	})

	t.Run("Update", func(t *testing.T) {
		if err := store.Update(ctx, &widget{},
			map[string]any{"name": "renamed"},
			modulekit.Where("id", "a1")); err != nil {
			t.Fatalf("Update: %v", err)
		}
		var got widget
		if err := store.First(ctx, &got, modulekit.Where("id", "a1")); err != nil {
			t.Fatalf("First: %v", err)
		}
		if got.Name != "renamed" {
			t.Fatalf("Name = %q, want renamed", got.Name)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		if err := store.Delete(ctx, &widget{}, modulekit.Where("id", "a3")); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		var got widget
		if err := store.First(ctx, &got, modulekit.Where("id", "a3")); !errors.Is(err, modulekit.ErrNotFound) {
			t.Fatalf("expected the row to be gone, got %v", err)
		}
	})
}

// TestUnconditionalWritesAreRefused covers the guard against a forgotten
// predicate emptying or rewriting a whole table.
func TestUnconditionalWritesAreRefused(t *testing.T) {
	store, db := newStore(t)
	seedWidgets(t, db)
	ctx := tenantCtx("tenant-a")

	if err := store.Delete(ctx, &widget{}); !errors.Is(err, modulekit.ErrUnconditional) {
		t.Fatalf("expected ErrUnconditional from Delete, got %v", err)
	}
	if err := store.Update(ctx, &widget{}, map[string]any{"name": "x"}); !errors.Is(err, modulekit.ErrUnconditional) {
		t.Fatalf("expected ErrUnconditional from Update, got %v", err)
	}

	// Nothing was removed or rewritten.
	n, err := store.Count(ctx, &widget{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows still present, got %d", n)
	}
}

// TestStoreDoesNotWeakenTenantIsolation is the point of the adapter's existence:
// introducing an abstraction over the connection must not open a path around
// the isolation callbacks registered on it.
func TestStoreDoesNotWeakenTenantIsolation(t *testing.T) {
	store, db := newStore(t)
	seedWidgets(t, db)

	t.Run("reads cannot reach another tenant", func(t *testing.T) {
		var got []widget
		if err := store.Find(tenantCtx("tenant-a"), &got, modulekit.Where("tenant_id", "tenant-b")); err != nil {
			t.Fatalf("Find: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("cross-tenant read returned %d rows: %+v", len(got), got)
		}
	})

	t.Run("writes cannot reach another tenant", func(t *testing.T) {
		if err := store.Update(tenantCtx("tenant-a"), &widget{},
			map[string]any{"name": "hijacked"},
			modulekit.Where("id", "b1")); err != nil {
			t.Fatalf("Update: %v", err)
		}

		var victim widget
		sys := tenancy.WithSystemContext(context.Background())
		if err := db.WithContext(sys).First(&victim, "id = ?", "b1").Error; err != nil {
			t.Fatalf("read victim: %v", err)
		}
		if victim.Name != "gamma" {
			t.Fatalf("tenant-a modified tenant-b's row: %+v", victim)
		}
	})

	t.Run("no tenant in context is refused", func(t *testing.T) {
		var got []widget
		err := store.Find(context.Background(), &got)
		if !errors.Is(err, tenancy.ErrNoTenantContext) {
			t.Fatalf("expected ErrNoTenantContext, got %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("unscoped Find returned %d rows", len(got))
		}
	})
}

// TestFieldNamesAreValidated confirms the one place a caller-supplied string
// reaches SQL — the column name, which cannot be a bind parameter — rejects
// anything that is not a bare identifier.
func TestFieldNamesAreValidated(t *testing.T) {
	store, db := newStore(t)
	seedWidgets(t, db)
	ctx := tenantCtx("tenant-a")

	for _, field := range []string{
		"name; DROP TABLE widgets",
		"1 OR 1=1",
		"widgets.name",
		"COUNT(*)",
		"",
	} {
		var got []widget
		err := store.Find(ctx, &got, modulekit.Where(field, "x"))
		if !errors.Is(err, modulekit.ErrUnsupportedOperator) {
			t.Fatalf("field %q: expected rejection, got %v", field, err)
		}
	}

	// The table survived.
	var n int64
	sys := tenancy.WithSystemContext(context.Background())
	if err := db.WithContext(sys).Model(&widget{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 rows intact, got %d", n)
	}
}

// TestUnknownOperatorIsRefused confirms an operator outside the enumerated set
// cannot reach the query builder.
func TestUnknownOperatorIsRefused(t *testing.T) {
	store, _ := newStore(t)
	var got []widget
	err := store.Find(tenantCtx("tenant-a"), &got,
		modulekit.Compare("name", modulekit.Operator("; DROP TABLE widgets --"), "x"))
	if !errors.Is(err, modulekit.ErrUnsupportedOperator) {
		t.Fatalf("expected ErrUnsupportedOperator, got %v", err)
	}
}

// TestTransactRollsBack confirms a module gets real ACID semantics over its own
// data, which is what ADR 0006 promises in exchange for no cross-module
// transaction.
func TestTransactRollsBack(t *testing.T) {
	store, _ := newStore(t)
	ctx := tenantCtx("tenant-a")

	sentinel := errors.New("deliberate failure")
	err := store.Transact(ctx, func(tx modulekit.Store) error {
		if err := tx.Create(ctx, &widget{ID: "t1", Name: "in-transaction"}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the sentinel error back, got %v", err)
	}

	var got widget
	if err := store.First(ctx, &got, modulekit.Where("id", "t1")); !errors.Is(err, modulekit.ErrNotFound) {
		t.Fatalf("expected the insert to be rolled back, got %v", err)
	}
}

func TestTransactCommits(t *testing.T) {
	store, _ := newStore(t)
	ctx := tenantCtx("tenant-a")

	if err := store.Transact(ctx, func(tx modulekit.Store) error {
		return tx.Create(ctx, &widget{ID: "t2", Name: "committed"})
	}); err != nil {
		t.Fatalf("Transact: %v", err)
	}

	var got widget
	if err := store.First(ctx, &got, modulekit.Where("id", "t2")); err != nil {
		t.Fatalf("expected the row to be committed: %v", err)
	}
	if got.TenantID != "tenant-a" {
		t.Fatalf("expected tenant stamped inside the transaction, got %q", got.TenantID)
	}
}
