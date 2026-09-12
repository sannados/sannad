package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

// contactView is a projection: a module's local copy of data another module
// owns, built from events. It is what ADR 0004 requires instead of reading
// across a module boundary.
// The primary key is (tenant_id, projection_key), not projection_key alone.
// Two tenants legitimately project the same source ID, and a key that omits the
// tenant makes the second one collide with the first. Tenant isolation scopes
// queries; it cannot widen a primary key.
type contactView struct {
	TenantID  string `gorm:"column:tenant_id;primaryKey"`
	ProjKey   string `gorm:"column:projection_key;primaryKey"`
	Name      string `gorm:"column:name"`
	SourceSeq int64  `gorm:"column:source_seq;not null"`
}

func (c *contactView) GetTenantID() string   { return c.TenantID }
func (c *contactView) SetTenantID(id string) { c.TenantID = id }

// The Projected implementation. The module names its own key column, so the
// helper never has to guess at a schema it does not own.
func (c *contactView) ProjectionKey() string { return c.ProjKey }
func (c *contactView) KeyColumn() string     { return "projection_key" }
func (c *contactView) SourceSequence() int64 { return c.SourceSeq }

// ProjectedColumns names every column the event carries, including the
// sequence. Omitting the sequence would leave the row holding new data at an
// old position, and every later event would compare against the wrong one.
func (c *contactView) ProjectedColumns() map[string]any {
	return map[string]any{
		"name":       c.Name,
		"source_seq": c.SourceSeq,
	}
}

func migrateProjections(t *testing.T, db *gorm.DB) {
	t.Helper()
	sys := tenancy.WithSystemContext(context.Background())
	// contactView is a fixture, so AutoMigrate is right for it. ProjectionState
	// is an SDK model whose schema comes from migration 00004, already applied
	// in newStore.
	if err := db.WithContext(sys).AutoMigrate(&contactView{}); err != nil {
		t.Fatalf("migrate projections: %v", err)
	}
	if err := tenancy.RegisterScopedModels(db, &contactView{}, &modulekit.ProjectionState{}); err != nil {
		t.Fatalf("register projection models: %v", err)
	}
}

func TestProjectAppliesNewState(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)
	ctx := tenantCtx("tenant-a")

	row := &contactView{ProjKey: "c-1", Name: "Amina", SourceSeq: 1}
	if err := modulekit.Project(ctx, store, row, &contactView{}); err != nil {
		t.Fatalf("Project: %v", err)
	}

	var got contactView
	if err := store.First(ctx, &got, modulekit.Where("projection_key", "c-1")); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Name != "Amina" || got.SourceSeq != 1 {
		t.Fatalf("got %+v", got)
	}
	if got.TenantID != "tenant-a" {
		t.Fatalf("projection row not tenant-stamped: %+v", got)
	}
}

func TestProjectAppliesNewerState(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)
	ctx := tenantCtx("tenant-a")

	_ = modulekit.Project(ctx, store, &contactView{ProjKey: "c-1", Name: "Amina", SourceSeq: 1}, &contactView{})
	if err := modulekit.Project(ctx, store,
		&contactView{ProjKey: "c-1", Name: "Amina Hassan", SourceSeq: 2}, &contactView{}); err != nil {
		t.Fatalf("Project newer: %v", err)
	}

	var got contactView
	if err := store.First(ctx, &got, modulekit.Where("projection_key", "c-1")); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Name != "Amina Hassan" || got.SourceSeq != 2 {
		t.Fatalf("newer event was not applied: %+v", got)
	}
}

// TestOutOfOrderEventIsDiscarded is the property that makes projections safe on
// an at-least-once, unordered backbone. A stale event overwriting fresh state is
// silent corruption: the row looks fine and is simply wrong.
func TestOutOfOrderEventIsDiscarded(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)
	ctx := tenantCtx("tenant-a")

	// Sequence 5 arrives first — normal on a redelivering broker.
	if err := modulekit.Project(ctx, store,
		&contactView{ProjKey: "c-1", Name: "current", SourceSeq: 5}, &contactView{}); err != nil {
		t.Fatalf("Project: %v", err)
	}

	// Sequence 3 arrives late and must not win.
	err := modulekit.Project(ctx, store,
		&contactView{ProjKey: "c-1", Name: "stale", SourceSeq: 3}, &contactView{})
	if !errors.Is(err, modulekit.ErrStaleProjection) {
		t.Fatalf("expected ErrStaleProjection, got %v", err)
	}

	var got contactView
	if err := store.First(ctx, &got, modulekit.Where("projection_key", "c-1")); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Name != "current" || got.SourceSeq != 5 {
		t.Fatalf("a stale event overwrote fresher state: %+v", got)
	}
}

// TestRedeliveryOfTheSameSequenceIsDiscarded: at-least-once means the same
// event, not just an older one, arrives twice.
func TestRedeliveryOfTheSameSequenceIsDiscarded(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)
	ctx := tenantCtx("tenant-a")

	row := &contactView{ProjKey: "c-1", Name: "Amina", SourceSeq: 7}
	if err := modulekit.Project(ctx, store, row, &contactView{}); err != nil {
		t.Fatalf("Project: %v", err)
	}
	err := modulekit.Project(ctx, store,
		&contactView{ProjKey: "c-1", Name: "Amina", SourceSeq: 7}, &contactView{})
	if !errors.Is(err, modulekit.ErrStaleProjection) {
		t.Fatalf("expected redelivery to be discarded, got %v", err)
	}
}

// TestUnsequencedRowIsRefused: a zero sequence would compare as older than
// everything, so every later event would be discarded and the projection would
// silently freeze at its first value.
func TestUnsequencedRowIsRefused(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)
	ctx := tenantCtx("tenant-a")

	for _, row := range []*contactView{
		{ProjKey: "c-1", SourceSeq: 0},
		{ProjKey: "c-1", SourceSeq: -1},
		{ProjKey: "", SourceSeq: 1},
	} {
		if err := modulekit.Project(ctx, store, row, &contactView{}); !errors.Is(err, modulekit.ErrProjectionNotSequenced) {
			t.Errorf("row %+v: expected ErrProjectionNotSequenced, got %v", row, err)
		}
	}
}

// TestProjectionsAreTenantScoped: a projection holds another module's data, and
// getting isolation wrong here would leak across tenants just as a primary table
// would.
func TestProjectionsAreTenantScoped(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)

	if err := modulekit.Project(tenantCtx("tenant-a"), store,
		&contactView{ProjKey: "c-1", Name: "a-side", SourceSeq: 1}, &contactView{}); err != nil {
		t.Fatalf("tenant-a: %v", err)
	}
	// Same key, other tenant. These are different rows and must not collide.
	if err := modulekit.Project(tenantCtx("tenant-b"), store,
		&contactView{ProjKey: "c-1", Name: "b-side", SourceSeq: 1}, &contactView{}); err != nil {
		t.Fatalf("tenant-b: %v", err)
	}

	var got contactView
	if err := store.First(tenantCtx("tenant-a"), &got, modulekit.Where("projection_key", "c-1")); err != nil {
		t.Fatalf("read tenant-a: %v", err)
	}
	if got.Name != "a-side" {
		t.Fatalf("tenant-b overwrote tenant-a's projection: %+v", got)
	}
}

// TestRebuildReplacesEverything covers the rebuild path: a projection is derived
// data and must be reconstructible from scratch after a bug or a schema change.
func TestRebuildReplacesEverything(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)
	ctx := tenantCtx("tenant-a")

	_ = modulekit.Project(ctx, store, &contactView{ProjKey: "c-1", Name: "wrong", SourceSeq: 1}, &contactView{})

	err := modulekit.Rebuild(ctx, store, "contacts",
		func(tx modulekit.Store) error {
			return tx.Delete(ctx, &contactView{}, modulekit.Compare("source_seq", modulekit.OpGreaterEqual, 0))
		},
		func(tx modulekit.Store) error {
			return tx.Create(ctx, &contactView{ProjKey: "c-1", Name: "rebuilt", SourceSeq: 9})
		})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	var got contactView
	if err := store.First(ctx, &got, modulekit.Where("projection_key", "c-1")); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Name != "rebuilt" {
		t.Fatalf("rebuild did not replace the row: %+v", got)
	}

	rebuilding, err := modulekit.IsRebuilding(ctx, store, "contacts")
	if err != nil {
		t.Fatalf("IsRebuilding: %v", err)
	}
	if rebuilding {
		t.Fatal("the rebuilding mark was not cleared after a successful rebuild")
	}
}

// TestFailedRebuildStaysMarked: a failed rebuild leaves the projection empty or
// partial. Clearing the mark would advertise incomplete data as complete, which
// is worse than an obvious outage.
func TestFailedRebuildStaysMarked(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)
	ctx := tenantCtx("tenant-a")

	boom := errors.New("replay failed")
	err := modulekit.Rebuild(ctx, store, "contacts",
		func(tx modulekit.Store) error { return nil },
		func(tx modulekit.Store) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("expected the replay error, got %v", err)
	}

	rebuilding, err := modulekit.IsRebuilding(ctx, store, "contacts")
	if err != nil {
		t.Fatalf("IsRebuilding: %v", err)
	}
	if !rebuilding {
		t.Fatal("a failed rebuild reported itself as complete")
	}
}

// TestRebuildRollsBackOnFailure: a half-rebuilt projection that committed would
// look complete while holding partial data.
func TestRebuildRollsBackOnFailure(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)
	ctx := tenantCtx("tenant-a")

	_ = modulekit.Project(ctx, store, &contactView{ProjKey: "c-1", Name: "original", SourceSeq: 1}, &contactView{})

	boom := errors.New("replay failed midway")
	_ = modulekit.Rebuild(ctx, store, "contacts",
		func(tx modulekit.Store) error {
			return tx.Delete(ctx, &contactView{}, modulekit.Compare("source_seq", modulekit.OpGreaterEqual, 0))
		},
		func(tx modulekit.Store) error {
			if err := tx.Create(ctx, &contactView{ProjKey: "c-2", Name: "partial", SourceSeq: 2}); err != nil {
				return err
			}
			return boom
		})

	// The truncate and the partial write must both be gone.
	var original contactView
	if err := store.First(ctx, &original, modulekit.Where("projection_key", "c-1")); err != nil {
		t.Fatalf("the original row was lost to a failed rebuild: %v", err)
	}
	var partial contactView
	if err := store.First(ctx, &partial, modulekit.Where("projection_key", "c-2")); !errors.Is(err, modulekit.ErrNotFound) {
		t.Fatalf("a partial rebuild write survived: %v", err)
	}
}

// TestIsRebuildingOnUnknownProjection: a projection that has never been rebuilt
// is complete, not missing.
func TestIsRebuildingOnUnknownProjection(t *testing.T) {
	store, db := newStore(t)
	migrateProjections(t, db)

	rebuilding, err := modulekit.IsRebuilding(tenantCtx("tenant-a"), store, "never-seen")
	if err != nil {
		t.Fatalf("IsRebuilding: %v", err)
	}
	if rebuilding {
		t.Fatal("an unknown projection reported itself as rebuilding")
	}
}
