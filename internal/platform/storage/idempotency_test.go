package storage_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/pkg/modulekit"
)

// The idempotency helper lives in pkg/modulekit but cannot be tested there: it
// needs a real Store with real transactions and a real primary key, and the SDK
// names no engine. These tests exercise it through the shipped adapter, which
// is the only configuration that will ever run in production.

// sideEffect stands in for whatever a consumer does — apply a payment, move
// stock. Counting it is how a test proves the work happened exactly once.
type sideEffect struct {
	mu    sync.Mutex
	count int
}

func (s *sideEffect) apply() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
}

func (s *sideEffect) times() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func TestIdempotentRunsOnceForRepeatedDelivery(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)
	ctx := tenantCtx("tenant-a")

	var effect sideEffect
	handle := func(tx modulekit.Store) error {
		effect.apply()
		return nil
	}

	if err := modulekit.Idempotent(ctx, store, "billing", "evt-1", handle); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	// The broker redelivers. This is routine, not exceptional.
	err := modulekit.Idempotent(ctx, store, "billing", "evt-1", handle)
	if !errors.Is(err, modulekit.ErrDuplicateEvent) {
		t.Fatalf("second delivery: expected ErrDuplicateEvent, got %v", err)
	}

	if effect.times() != 1 {
		t.Fatalf("handler ran %d times, want 1", effect.times())
	}
}

// TestIdempotentIsPerConsumer: one event reaching two consumers must run in
// both. A marker keyed on the event alone would silently drop the second.
func TestIdempotentIsPerConsumer(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)
	ctx := tenantCtx("tenant-a")

	var billing, shipping sideEffect
	if err := modulekit.Idempotent(ctx, store, "billing", "evt-1", func(modulekit.Store) error {
		billing.apply()
		return nil
	}); err != nil {
		t.Fatalf("billing: %v", err)
	}
	if err := modulekit.Idempotent(ctx, store, "shipping", "evt-1", func(modulekit.Store) error {
		shipping.apply()
		return nil
	}); err != nil {
		t.Fatalf("shipping: %v", err)
	}

	if billing.times() != 1 || shipping.times() != 1 {
		t.Fatalf("billing ran %d, shipping ran %d; both should be 1",
			billing.times(), shipping.times())
	}
}

// TestIdempotentIsPerTenant: the marker table is tenant-scoped, so one tenant's
// processing must not suppress another's. Without this, the first tenant to
// receive a broadcast event would silently consume it for everyone.
func TestIdempotentIsPerTenant(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)

	var effect sideEffect
	handle := func(modulekit.Store) error { effect.apply(); return nil }

	if err := modulekit.Idempotent(tenantCtx("tenant-a"), store, "billing", "evt-1", handle); err != nil {
		t.Fatalf("tenant-a: %v", err)
	}
	if err := modulekit.Idempotent(tenantCtx("tenant-b"), store, "billing", "evt-1", handle); err != nil {
		t.Fatalf("tenant-b: %v", err)
	}

	if effect.times() != 2 {
		t.Fatalf("handler ran %d times, want 2 — one tenant suppressed another", effect.times())
	}
}

// TestHandlerFailureRollsBackTheMarker is the property that makes the whole
// thing safe: if the work fails, the claim must fail with it, or the event is
// lost forever. Marker and work commit together or not at all.
func TestHandlerFailureRollsBackTheMarker(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)
	ctx := tenantCtx("tenant-a")

	boom := errors.New("handler failed")
	err := modulekit.Idempotent(ctx, store, "billing", "evt-1", func(modulekit.Store) error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected the handler error, got %v", err)
	}

	// The event must still be processable. If the marker survived the rollback,
	// a transient failure would permanently swallow the event.
	processed, err := modulekit.WasProcessed(ctx, store, "billing", "evt-1")
	if err != nil {
		t.Fatalf("WasProcessed: %v", err)
	}
	if processed {
		t.Fatal("the marker survived a failed handler; the event can never be retried")
	}

	var effect sideEffect
	if err := modulekit.Idempotent(ctx, store, "billing", "evt-1", func(modulekit.Store) error {
		effect.apply()
		return nil
	}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if effect.times() != 1 {
		t.Fatalf("retry ran %d times, want 1", effect.times())
	}
}

// TestHandlerWritesAreRolledBackToo confirms the handler's own writes share the
// transaction — the marker is not committed separately around them.
func TestHandlerWritesAreRolledBackToo(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)
	ctx := tenantCtx("tenant-a")

	boom := errors.New("failed after writing")
	err := modulekit.Idempotent(ctx, store, "billing", "evt-1", func(tx modulekit.Store) error {
		if err := tx.Create(ctx, &widget{ID: "from-handler", Name: "should not survive"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected the handler error, got %v", err)
	}

	var got widget
	if err := store.First(ctx, &got, modulekit.Where("id", "from-handler")); !errors.Is(err, modulekit.ErrNotFound) {
		t.Fatalf("the handler's write survived the rollback: %v", err)
	}
}

// TestConcurrentDeliveryRunsOnce is the race the primary key exists to win.
// A check-then-act implementation passes every sequential test above and fails
// this one.
func TestConcurrentDeliveryRunsOnce(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)
	ctx := tenantCtx("tenant-a")

	const deliveries = 8
	var effect sideEffect
	var wg sync.WaitGroup
	results := make([]error, deliveries)

	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = modulekit.Idempotent(ctx, store, "billing", "evt-race", func(modulekit.Store) error {
				effect.apply()
				return nil
			})
		}(i)
	}
	wg.Wait()

	if effect.times() != 1 {
		t.Fatalf("handler ran %d times under concurrent delivery, want exactly 1", effect.times())
	}

	// Exactly one delivery should report success; the rest are duplicates.
	// Anything else means the outcome depended on scheduling.
	var succeeded, duplicates, other int
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, modulekit.ErrDuplicateEvent):
			duplicates++
		default:
			other++
			t.Logf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d deliveries reported success, want 1", succeeded)
	}
	if succeeded+duplicates != deliveries {
		t.Errorf("%d deliveries returned an unexpected error", other)
	}
}

// TestEmptyKeysAreRefused: an empty consumer or event ID would make unrelated
// events collide on one marker, silently dropping every event after the first.
func TestEmptyKeysAreRefused(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)
	ctx := tenantCtx("tenant-a")

	for _, tc := range []struct{ consumer, event string }{
		{"", "evt-1"},
		{"billing", ""},
		{"", ""},
	} {
		err := modulekit.Idempotent(ctx, store, tc.consumer, tc.event, func(modulekit.Store) error {
			t.Error("handler ran despite an invalid key")
			return nil
		})
		if !errors.Is(err, modulekit.ErrInvalidIdempotencyKey) {
			t.Errorf("consumer=%q event=%q: expected ErrInvalidIdempotencyKey, got %v",
				tc.consumer, tc.event, err)
		}
	}
}

// TestForgetProcessedAllowsReplay covers the reconciliation path.
func TestForgetProcessedAllowsReplay(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)
	ctx := tenantCtx("tenant-a")

	var effect sideEffect
	handle := func(modulekit.Store) error { effect.apply(); return nil }

	_ = modulekit.Idempotent(ctx, store, "billing", "evt-1", handle)
	if err := modulekit.ForgetProcessed(ctx, store, "billing", "evt-1"); err != nil {
		t.Fatalf("ForgetProcessed: %v", err)
	}
	if err := modulekit.Idempotent(ctx, store, "billing", "evt-1", handle); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if effect.times() != 2 {
		t.Fatalf("handler ran %d times, want 2 after a deliberate replay", effect.times())
	}
}

// TestIdempotencyRequiresTenantContext: without a tenant the marker would be
// written unscoped, which is the cross-tenant leak ADR 0005 closed.
func TestIdempotencyRequiresTenantContext(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)

	err := modulekit.Idempotent(context.Background(), store, "billing", "evt-1", func(modulekit.Store) error {
		t.Error("handler ran with no tenant in context")
		return nil
	})
	if !errors.Is(err, tenancy.ErrNoTenantContext) {
		t.Fatalf("expected ErrNoTenantContext, got %v", err)
	}
}

// TestDuplicateKeyIsTranslated pins the adapter contract the helper depends on.
// Without gorm.Config.TranslateError the driver's raw message arrives instead,
// nothing recognises it as a duplicate, and Idempotent reports a redelivery as
// an unknown failure — so a consumer would retry the same event forever.
func TestDuplicateKeyIsTranslated(t *testing.T) {
	store, db := newStore(t)
	migrateProcessedEvents(t, db)
	ctx := tenancy.WithSystemContext(context.Background())

	marker := modulekit.ProcessedEvent{TenantID: "t", Consumer: "c", EventID: "e"}
	if err := store.Create(ctx, &marker); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := store.Create(ctx, &modulekit.ProcessedEvent{TenantID: "t", Consumer: "c", EventID: "e"})
	if !errors.Is(err, modulekit.ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v — is gorm.Config.TranslateError still set?", err)
	}
	_ = fmt.Sprint(db)
}
