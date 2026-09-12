package modulekit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ADR 0004 forbids a module from reading another module's tables, and ADR 0006
// forbids a transaction spanning two modules. Together they mean a module that
// needs another's data keeps its own copy, built from events. That copy is a
// projection.
//
// Three things make projections hard, and each is a place authors get it wrong:
//
//   - Events arrive out of order. A stale update must not overwrite a fresh one.
//   - Delivery is at-least-once, so every event will be seen twice.
//   - Projections need rebuilding — after a bug, a schema change, or a new
//     field — which means replaying history into an empty table.
//
// ADR 0006 decision 4 puts this in the SDK because thirty modules solving it
// separately is thirty chances to solve it wrong, and a projection that silently
// holds stale data is not obviously broken until someone bills against it.

// Projected is implemented by a projection row.
//
// A projection model must key on (tenant, projection key), not the projection
// key alone. Two tenants legitimately project the same source ID, and a key
// that omits the tenant makes the second collide with the first. Tenant
// isolation scopes queries; it cannot widen a primary key, so this is the
// module author's responsibility and nothing will report it if it is wrong.
//
// The module owns its projection's schema; this interface only asks it to
// answer two questions about itself. That is deliberate — a helper that assumed
// particular column names would work until the first module that named things
// differently, and fail at runtime rather than at compile time.
type Projected interface {
	// ProjectionKey identifies the row this event applies to — typically the
	// source entity's ID.
	ProjectionKey() string

	// KeyColumn is the column ProjectionKey is stored in, so the helper can
	// query the module's own table without guessing.
	KeyColumn() string

	// SourceSequence is the ordering position of the event that produced this
	// state. It must come from the *source* — an event sequence number, a
	// version column, a monotonic revision — and never from the consumer's own
	// clock or arrival order, which are exactly what reordering breaks.
	SourceSequence() int64

	// ProjectedColumns is the complete row as a column map, used to replace an
	// existing row in one statement.
	//
	// The module supplies this rather than the helper deriving it: reflecting
	// over struct tags would put engine mapping back into the SDK, which the
	// boundary forbids, and would fail at runtime on the first model that named
	// a column differently.
	//
	// It must include the sequence column. Omitting it leaves the stored row
	// holding an old sequence while carrying new data, and every later event
	// would then be compared against the wrong position.
	ProjectedColumns() map[string]any
}

// ErrStaleProjection is returned when an event is older than the state already
// projected. Like ErrDuplicateEvent, it means the mechanism worked: the caller
// should acknowledge the delivery and move on.
var ErrStaleProjection = errors.New("modulekit: event is older than projected state")

// ErrProjectionNotSequenced is returned when a projection row reports a
// non-positive sequence or an empty key. A zero sequence would make every
// subsequent event look stale, silently freezing the projection.
var ErrProjectionNotSequenced = errors.New("modulekit: projection row is not sequenced")

// Project applies an event to a projection, discarding it if the row already
// holds newer state.
//
// row must be a pointer to a projection model implementing Projected, populated
// with the new state. It is written whole: projections are derived data, so
// last-write-wins on a complete row is correct and avoids a merge nobody can
// reason about.
//
// Sequence comparison, not timestamps. Two events produced in the same clock
// tick are indistinguishable by time, and clocks on separate machines disagree.
//
// current must be a pointer to a zero value of the same model; the helper loads
// the stored row into it to compare sequences. Asking for it keeps the helper
// free of reflection over the module's type.
//
// Returns ErrStaleProjection if a newer version is already stored.
//
// Requires the kernel store: the read and the write must share a transaction,
// or a concurrent apply commits between them.
func Project(ctx context.Context, store Store, row Projected, current Projected) error {
	if row.ProjectionKey() == "" {
		return fmt.Errorf("%w: projection row has no key", ErrProjectionNotSequenced)
	}
	if row.SourceSequence() <= 0 {
		return fmt.Errorf("%w: key %s carries sequence %d",
			ErrProjectionNotSequenced, row.ProjectionKey(), row.SourceSequence())
	}
	if row.KeyColumn() == "" {
		return fmt.Errorf("%w: projection model names no key column", ErrProjectionNotSequenced)
	}

	return store.Transact(ctx, func(tx Store) error {
		// Read the current state inside the transaction. Reading outside it
		// would leave a window where a concurrent apply commits between the
		// check and the write.
		err := tx.First(ctx, current, Where(row.KeyColumn(), row.ProjectionKey()))
		switch {
		case errors.Is(err, ErrNotFound):
			// First time this key has been seen.
			return tx.Create(ctx, row)
		case err != nil:
			return fmt.Errorf("read projection %s: %w", row.ProjectionKey(), err)
		}

		if current.SourceSequence() >= row.SourceSequence() {
			return fmt.Errorf("%w: key %s holds sequence %d, event carries %d",
				ErrStaleProjection, row.ProjectionKey(), current.SourceSequence(), row.SourceSequence())
		}

		// Replace in one statement rather than delete-then-insert. The pair
		// would deadlock against the row's own primary key inside a single
		// transaction, and an update is what this operation actually is.
		//
		// The whole row is written, not a merge: it is derived data, and the
		// event carries the complete new state.
		columns := row.ProjectedColumns()
		if len(columns) == 0 {
			return fmt.Errorf("%w: key %s has no projected columns",
				ErrProjectionNotSequenced, row.ProjectionKey())
		}
		return tx.Update(ctx, row, columns, Where(row.KeyColumn(), row.ProjectionKey()))
	})
}

// ProjectionState records whether a projection is mid-rebuild, so a reader can
// tell complete data from partial data.
//
// Column names are given explicitly rather than left to an adapter's naming
// convention, since the migration and the model have to agree and only one of
// them is in this package.
type ProjectionState struct {
	TenantID   string
	Projection string
	UpdatedAt  time.Time

	// RebuildingSince is set while a rebuild is in progress. A projection being
	// rebuilt holds incomplete data, and a reader that cannot tell will serve
	// it as though it were complete.
	RebuildingSince *time.Time
}

func (p *ProjectionState) GetTenantID() string   { return p.TenantID }
func (p *ProjectionState) SetTenantID(id string) { p.TenantID = id }

func (ProjectionState) TableName() string { return "modulekit_projection_state" }

// IsRebuilding reports whether a projection is mid-rebuild and therefore
// incomplete.
//
// A caller that serves projection data without checking this will serve partial
// results during a rebuild and have no way to know. A read model backing a
// user-visible number should check it and say "refreshing" rather than show a
// wrong figure confidently.
func IsRebuilding(ctx context.Context, store Store, projection string) (bool, error) {
	var state ProjectionState
	err := store.First(ctx, &state, Where("projection", projection))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read projection state %s: %w", projection, err)
	}
	return state.RebuildingSince != nil, nil
}

// Rebuild replays a projection from scratch.
//
// It marks the projection as rebuilding, runs truncate and replay in one
// transaction, and clears the mark on success. The mark is what makes a
// partially built projection detectable rather than silently wrong.
//
// One transaction for the whole rebuild: a half-rebuilt projection that
// committed would be worse than no rebuild at all, because it looks complete.
// The cost is that a large rebuild holds a long transaction, which is a real
// limit — a projection too big for that needs a batched strategy this helper
// deliberately does not pretend to provide.
//
// On failure the mark is left set. A failed rebuild leaves the projection empty
// or partial, and clearing the flag would advertise it as complete.
func Rebuild(ctx context.Context, store Store, projection string, truncate func(tx Store) error, replay func(tx Store) error) error {
	if projection == "" {
		return fmt.Errorf("%w: rebuild needs a projection name", ErrProjectionNotSequenced)
	}

	startedAt := now()
	if err := markRebuilding(ctx, store, projection, &startedAt); err != nil {
		return err
	}

	if err := store.Transact(ctx, func(tx Store) error {
		if err := truncate(tx); err != nil {
			return fmt.Errorf("truncate projection %s: %w", projection, err)
		}
		return replay(tx)
	}); err != nil {
		return fmt.Errorf("rebuild projection %s: %w", projection, err)
	}

	return markRebuilding(ctx, store, projection, nil)
}

// markRebuilding sets or clears the rebuilding marker, creating the state row if
// this is the projection's first rebuild.
func markRebuilding(ctx context.Context, store Store, projection string, since *time.Time) error {
	var state ProjectionState
	err := store.First(ctx, &state, Where("projection", projection))
	switch {
	case errors.Is(err, ErrNotFound):
		return store.Create(ctx, &ProjectionState{
			Projection:      projection,
			UpdatedAt:       now(),
			RebuildingSince: since,
		})
	case err != nil:
		return fmt.Errorf("read projection state %s: %w", projection, err)
	}

	return store.Update(ctx, &ProjectionState{},
		map[string]any{"rebuilding_since": since, "updated_at": now()},
		Where("projection", projection))
}
