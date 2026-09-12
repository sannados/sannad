package modulekit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The event backbone guarantees at-least-once delivery, which means every
// consumer will eventually be handed the same event twice. Redelivery is not an
// exceptional case to be handled someday: it happens on every broker restart,
// every consumer redeploy, and every acknowledgement lost mid-flight.
//
// ADR 0006 decision 4 puts idempotency in the SDK for that reason. Thirty
// modules each writing this themselves would be thirty chances to write it
// slightly wrong, and the failure — a payment applied twice, a stock movement
// double-counted — is silent and expensive.

// ProcessedEvent records that a consumer handled an event. It is a module-owned
// table: each module gets its own, because "has this been processed" is a
// question about one consumer's progress, not global state.
//
// The primary key is (tenant_id, consumer, event_id). The same event legitimately
// reaches several consumers, and each must process it exactly once.
type ProcessedEvent struct {
	TenantID    string
	Consumer    string
	EventID     string
	ProcessedAt time.Time
}

// GetTenantID and SetTenantID implement tenant.TenantAware, so the isolation
// callbacks scope this table like any other. A consumer in one tenant must not
// be able to observe — or suppress — another tenant's processing.
func (p *ProcessedEvent) GetTenantID() string   { return p.TenantID }
func (p *ProcessedEvent) SetTenantID(id string) { p.TenantID = id }

// TableName is the one piece of storage vocabulary the SDK keeps. It names a
// table, not an engine — the adapter decides what a table means — and pinning
// it here is what lets the migration and the model agree across packages.
func (ProcessedEvent) TableName() string { return "modulekit_processed_events" }

// ErrDuplicateEvent is returned by Idempotent when the event was already
// processed. It is not a failure: it is the mechanism working. Callers should
// acknowledge the delivery and move on.
var ErrDuplicateEvent = errors.New("modulekit: event already processed")

// Idempotent runs handle exactly once for a given (consumer, eventID) within a
// tenant, even if the event is delivered repeatedly.
//
// The marker is written in the same transaction as the handler's own writes.
// That is the whole design: if the marker committed separately, a crash between
// the two would either lose the work (marker first) or repeat it (work first).
// Because ADR 0006 grants a module a transaction over its own data, and the
// marker table is the module's own, one transaction covers both.
//
// The handler receives the transactional Store and must use it. Writing through
// any other handle escapes the transaction and forfeits the guarantee.
//
// Returns ErrDuplicateEvent if the event was already processed, so a caller can
// distinguish "nothing to do" from "something went wrong".
//
// Requires the kernel store. A module that declares StorageOwn cannot use this:
// the marker has to be written in the same transaction as the handler's own
// work, and there is no way to do that across two storage systems the kernel
// does not control. A self-managed module needs its own equivalent, and this
// implementation is worth reading first — the check-then-act version of it ran
// a handler six times under eight concurrent deliveries while passing every
// sequential test.
func Idempotent(ctx context.Context, store Store, consumer, eventID string, handle func(tx Store) error) error {
	if consumer == "" {
		return fmt.Errorf("%w: consumer name is required", ErrInvalidIdempotencyKey)
	}
	if eventID == "" {
		return fmt.Errorf("%w: event ID is required", ErrInvalidIdempotencyKey)
	}

	return store.Transact(ctx, func(tx Store) error {
		// The claim goes first. Two concurrent deliveries of the same event race
		// here, and the loser's insert violates the primary key — which is the
		// point. A check-then-act would let both pass the check.
		//
		// This relies on the marker being a real primary key, not an index the
		// migration forgot.
		marker := ProcessedEvent{
			Consumer:    consumer,
			EventID:     eventID,
			ProcessedAt: now(),
		}
		if err := tx.Create(ctx, &marker); err != nil {
			if isDuplicateKey(err) {
				return ErrDuplicateEvent
			}
			return fmt.Errorf("claim event %s for %s: %w", eventID, consumer, err)
		}

		return handle(tx)
	})
}

// WasProcessed reports whether a consumer has already handled an event.
//
// This is for diagnostics and reconciliation, not for guarding a handler: a
// check followed by a separate write is a race, and Idempotent exists so that
// nobody has to write one. Use it to answer "did we see this", not to decide
// whether to process.
func WasProcessed(ctx context.Context, store Store, consumer, eventID string) (bool, error) {
	n, err := store.Count(ctx, &ProcessedEvent{},
		Where("consumer", consumer),
		Where("event_id", eventID))
	if err != nil {
		return false, fmt.Errorf("check processed %s/%s: %w", consumer, eventID, err)
	}
	return n > 0, nil
}

// ForgetProcessed removes a processing marker, so the event will run again.
//
// This deliberately has no bulk form. Replaying a consumer's entire history is
// a projection rebuild, which has its own machinery and its own safeguards;
// a loop over this function is not that.
func ForgetProcessed(ctx context.Context, store Store, consumer, eventID string) error {
	return store.Delete(ctx, &ProcessedEvent{},
		Where("consumer", consumer),
		Where("event_id", eventID))
}

// ErrInvalidIdempotencyKey is returned when a consumer name or event ID is
// empty. An empty key would make unrelated events collide on one marker.
var ErrInvalidIdempotencyKey = errors.New("modulekit: invalid idempotency key")

// isDuplicateKey reports whether an error is a primary-key or unique-constraint
// violation.
//
// The SDK names no engine, so this cannot test for a driver's error type. The
// adapter is responsible for translating its engine's constraint violation into
// ErrDuplicate before it reaches here — the same contract that turns
// gorm.ErrRecordNotFound into ErrNotFound.
func isDuplicateKey(err error) bool {
	return errors.Is(err, ErrDuplicate)
}

// now is a variable so tests can freeze time without the SDK depending on a
// clock abstraction that every module would then have to thread through.
var now = time.Now
