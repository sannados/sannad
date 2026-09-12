package webhooks_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/glebarez/sqlite"
	"github.com/sannados/sannad/internal/app/migrations"
	"github.com/sannados/sannad/internal/kernel/events"
	"github.com/sannados/sannad/internal/platform/storage"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/internal/platform/webhooks"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

// These run through the shipped adapter with the real migration, because the
// properties under test are storage properties: the unique index is what makes a
// redelivery a no-op, and tenant isolation is what stops one tenant's events
// reaching another's endpoints. AutoMigrate would build neither.

func runnerStore(t *testing.T) (modulekit.Store, *gorm.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "webhooks-test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_pragma=busy_timeout(5000)"), storage.GormConfig())
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
	if err := tenancy.RegisterIsolation(db); err != nil {
		t.Fatalf("isolation: %v", err)
	}
	if err := tenancy.RegisterScopedModels(db, &webhooks.Endpoint{}, &webhooks.Delivery{}); err != nil {
		t.Fatalf("scoped models: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return storage.NewGormStore(db), db
}

func tenantContext(id string) context.Context {
	return tenant.WithTenantID(context.Background(), id)
}

func addEndpoint(t *testing.T, store modulekit.Store, ctx context.Context, id, url, eventFilter string) *webhooks.Endpoint {
	t.Helper()
	endpoint := &webhooks.Endpoint{
		ID: id, URL: url, Events: eventFilter, Secret: "shared-secret", Active: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := store.Create(ctx, endpoint); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	return endpoint
}

func busEvent(tenantID, event string, sequence uint64) events.Delivery {
	return events.Delivery{
		Subject:  "sannad." + tenantID + "." + event,
		TenantID: tenantID,
		Event:    event,
		Data:     []byte(`{"id":"o-1"}`),
		Sequence: sequence,
	}
}

func pendingCount(t *testing.T, store modulekit.Store, ctx context.Context) int64 {
	t.Helper()
	n, err := store.Count(ctx, &webhooks.Delivery{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestEventReachesTheReceiver is the end-to-end path: an event on the bus
// becomes a signed HTTP request the receiver verifies.
func TestEventReachesTheReceiver(t *testing.T) {
	store, _ := runnerStore(t)
	ctx := tenantContext("acme")

	var received atomic.Int32
	var verified atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		timestamp, err := webhooks.ParseTimestamp(r.Header.Get(webhooks.HeaderTimestamp))
		if err == nil {
			verified.Store(webhooks.Verify("shared-secret", r.Header.Get(webhooks.HeaderSignature),
				timestamp, body, webhooks.DefaultTolerance, time.Now()) == nil)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	addEndpoint(t, store, ctx, "ep-1", server.URL, "orders.created")
	runner := webhooks.NewRunner(store)

	if err := runner.Enqueue(ctx, busEvent("acme", "orders.created", 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := runner.DrainOnce(ctx, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if received.Load() != 1 {
		t.Fatalf("receiver got %d requests, want 1", received.Load())
	}
	if !verified.Load() {
		t.Fatal("the receiver could not verify the signature")
	}
}

// TestRedeliveryFromTheBusDoesNotDoubleSend. The bus is at-least-once, so a
// consumer will see the same event twice; without the unique index that becomes
// two webhooks and a duplicate side effect on the receiver.
func TestRedeliveryFromTheBusDoesNotDoubleSend(t *testing.T) {
	store, _ := runnerStore(t)
	ctx := tenantContext("acme")

	addEndpoint(t, store, ctx, "ep-1", "http://192.0.2.1:9/hook", "")
	runner := webhooks.NewRunner(store)

	event := busEvent("acme", "orders.created", 42)
	if err := runner.Enqueue(ctx, event); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// The same message, redelivered. Same stream sequence, so the same identity.
	if err := runner.Enqueue(ctx, event); err != nil {
		t.Fatalf("redelivery should be a no-op, got: %v", err)
	}

	if n := pendingCount(t, store, ctx); n != 1 {
		t.Fatalf("a redelivered event queued %d deliveries, want 1", n)
	}
}

// TestEndpointsOfOtherTenantsAreNotReached. The consumer spans tenants, so this
// is the boundary that stops one tenant's event reaching another's endpoint.
func TestEndpointsOfOtherTenantsAreNotReached(t *testing.T) {
	store, _ := runnerStore(t)
	acme := tenantContext("acme")
	other := tenantContext("other")

	addEndpoint(t, store, acme, "ep-acme", "http://192.0.2.1:9/acme", "")
	addEndpoint(t, store, other, "ep-other", "http://192.0.2.1:9/other", "")

	runner := webhooks.NewRunner(store)
	if err := runner.Enqueue(acme, busEvent("acme", "orders.created", 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if n := pendingCount(t, store, acme); n != 1 {
		t.Fatalf("acme has %d deliveries, want 1", n)
	}
	if n := pendingCount(t, store, other); n != 0 {
		t.Fatalf("another tenant received %d deliveries from acme's event", n)
	}
}

// TestUnsubscribedEventsAreNotQueued.
func TestUnsubscribedEventsAreNotQueued(t *testing.T) {
	store, _ := runnerStore(t)
	ctx := tenantContext("acme")

	addEndpoint(t, store, ctx, "ep-1", "http://192.0.2.1:9/hook", "orders.created")
	runner := webhooks.NewRunner(store)

	if err := runner.Enqueue(ctx, busEvent("acme", "contacts.created", 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n := pendingCount(t, store, ctx); n != 0 {
		t.Fatalf("an unsubscribed event queued %d deliveries", n)
	}
}

// TestFailingReceiverIsRetriedThenDeadLettered, and the dead delivery keeps
// everything replay needs.
func TestFailingReceiverIsRetriedThenDeadLettered(t *testing.T) {
	store, _ := runnerStore(t)
	ctx := tenantContext("acme")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	addEndpoint(t, store, ctx, "ep-1", server.URL, "")
	runner := webhooks.NewRunner(store)
	if err := runner.Enqueue(ctx, busEvent("acme", "orders.created", 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Each pass must move the delivery on. Backoff is stored, so a delivery just
	// attempted is not due again immediately — the test steps the clock by
	// requeueing rather than sleeping through real backoff.
	for attempt := 0; attempt < webhooks.MaxAttempts; attempt++ {
		if _, err := runner.DrainOnce(ctx, 0); err != nil {
			t.Fatalf("drain: %v", err)
		}
		var current []webhooks.Delivery
		if err := store.Find(ctx, &current); err != nil {
			t.Fatalf("read: %v", err)
		}
		if current[0].Status == webhooks.StatusDead {
			break
		}
		// Make it due again without waiting out the real backoff.
		if err := store.Update(ctx, &webhooks.Delivery{},
			map[string]any{"next_attempt_at": time.Now().Add(-time.Minute)},
			modulekit.Where("id", current[0].ID)); err != nil {
			t.Fatalf("requeue: %v", err)
		}
	}

	var final []webhooks.Delivery
	if err := store.Find(ctx, &final); err != nil {
		t.Fatalf("read: %v", err)
	}
	if final[0].Status != webhooks.StatusDead {
		t.Fatalf("status %s after %d attempts, want dead", final[0].Status, final[0].Attempts)
	}
	if final[0].Payload == "" || final[0].EventID == "" {
		t.Fatal("a dead delivery lost what replay needs")
	}
	if final[0].LastStatusCode != http.StatusServiceUnavailable {
		t.Errorf("last status %d, want 503", final[0].LastStatusCode)
	}
}

// TestReplayResendsAfterTheReceiverIsFixed is the property dead deliveries are
// kept for: an operator who fixed a receiver resends what it missed.
func TestReplayResendsAfterTheReceiverIsFixed(t *testing.T) {
	store, _ := runnerStore(t)
	ctx := tenantContext("acme")

	var healthy atomic.Bool
	var delivered atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		delivered.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	addEndpoint(t, store, ctx, "ep-1", server.URL, "")
	runner := webhooks.NewRunner(store)
	if err := runner.Enqueue(ctx, busEvent("acme", "orders.created", 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Fail it all the way to dead.
	for attempt := 0; attempt < webhooks.MaxAttempts; attempt++ {
		if _, err := runner.DrainOnce(ctx, 0); err != nil {
			t.Fatalf("drain: %v", err)
		}
		var current []webhooks.Delivery
		_ = store.Find(ctx, &current)
		if current[0].Status == webhooks.StatusDead {
			break
		}
		_ = store.Update(ctx, &webhooks.Delivery{},
			map[string]any{"next_attempt_at": time.Now().Add(-time.Minute)},
			modulekit.Where("id", current[0].ID))
	}

	// The operator fixes the receiver and asks for everything it missed.
	healthy.Store(true)
	n, err := runner.ReplayEndpoint(ctx, "ep-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed %d deliveries, want 1", n)
	}

	if _, err := runner.DrainOnce(ctx, 0); err != nil {
		t.Fatalf("drain after replay: %v", err)
	}
	if delivered.Load() != 1 {
		t.Fatalf("receiver got %d deliveries after replay, want 1", delivered.Load())
	}

	var final []webhooks.Delivery
	_ = store.Find(ctx, &final)
	if final[0].Status != webhooks.StatusDelivered {
		t.Fatalf("status %s after a successful replay, want delivered", final[0].Status)
	}
}

// TestReplayRefusesAnAlreadyDeliveredEvent: resending something the receiver
// accepted would create a duplicate side effect on their side for no reason.
func TestReplayRefusesAnAlreadyDeliveredEvent(t *testing.T) {
	store, _ := runnerStore(t)
	ctx := tenantContext("acme")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	addEndpoint(t, store, ctx, "ep-1", server.URL, "")
	runner := webhooks.NewRunner(store)
	_ = runner.Enqueue(ctx, busEvent("acme", "orders.created", 1))
	if _, err := runner.DrainOnce(ctx, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}

	var delivered []webhooks.Delivery
	_ = store.Find(ctx, &delivered)
	if delivered[0].Status != webhooks.StatusDelivered {
		t.Fatalf("setup: status %s, want delivered", delivered[0].Status)
	}

	if err := runner.Replay(ctx, delivered[0].ID); err == nil {
		t.Fatal("replaying a delivered webhook was allowed")
	}
}

// TestDeletedEndpointDeadLetters rather than retrying forever against a
// destination that no longer exists.
func TestDeletedEndpointDeadLetters(t *testing.T) {
	store, _ := runnerStore(t)
	ctx := tenantContext("acme")

	addEndpoint(t, store, ctx, "ep-1", "http://192.0.2.1:9/hook", "")
	runner := webhooks.NewRunner(store)
	_ = runner.Enqueue(ctx, busEvent("acme", "orders.created", 1))

	if err := store.Delete(ctx, &webhooks.Endpoint{}, modulekit.Where("id", "ep-1")); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}

	if _, err := runner.DrainOnce(ctx, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}

	var final []webhooks.Delivery
	_ = store.Find(ctx, &final)
	if final[0].Status != webhooks.StatusDead {
		t.Fatalf("status %s, want dead", final[0].Status)
	}
}

// TestDeliveryNotYetDueIsSkipped: stored backoff is what stops a restart
// retrying everything at once.
func TestDeliveryNotYetDueIsSkipped(t *testing.T) {
	store, _ := runnerStore(t)
	ctx := tenantContext("acme")

	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	addEndpoint(t, store, ctx, "ep-1", server.URL, "")
	runner := webhooks.NewRunner(store)
	_ = runner.Enqueue(ctx, busEvent("acme", "orders.created", 1))

	var queued []webhooks.Delivery
	_ = store.Find(ctx, &queued)
	if err := store.Update(ctx, &webhooks.Delivery{},
		map[string]any{"next_attempt_at": time.Now().Add(time.Hour)},
		modulekit.Where("id", queued[0].ID)); err != nil {
		t.Fatalf("defer: %v", err)
	}

	handled, err := runner.DrainOnce(ctx, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if handled != 0 || received.Load() != 0 {
		t.Fatalf("a delivery scheduled an hour out was attempted (handled=%d, received=%d)",
			handled, received.Load())
	}
}

// TestReplayResetsTheAttemptCount covers what a successful replay hides.
//
// A replayed delivery that succeeds immediately never exercises the counter, so
// the reset looks unnecessary. It matters when the receiver is still flaky: with
// the old count carried forward, the delivery is already at the attempt limit
// and dead-letters again on the very first failure, giving the operator one
// attempt instead of the full retry budget they thought they were getting.
func TestReplayResetsTheAttemptCount(t *testing.T) {
	store, _ := runnerStore(t)
	ctx := tenantContext("acme")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	addEndpoint(t, store, ctx, "ep-1", server.URL, "")
	runner := webhooks.NewRunner(store)
	_ = runner.Enqueue(ctx, busEvent("acme", "orders.created", 1))

	// Drive it to dead.
	for attempt := 0; attempt < webhooks.MaxAttempts; attempt++ {
		if _, err := runner.DrainOnce(ctx, 0); err != nil {
			t.Fatalf("drain: %v", err)
		}
		var current []webhooks.Delivery
		_ = store.Find(ctx, &current)
		if current[0].Status == webhooks.StatusDead {
			break
		}
		_ = store.Update(ctx, &webhooks.Delivery{},
			map[string]any{"next_attempt_at": time.Now().Add(-time.Minute)},
			modulekit.Where("id", current[0].ID))
	}

	var dead []webhooks.Delivery
	_ = store.Find(ctx, &dead)
	if dead[0].Attempts < webhooks.MaxAttempts {
		t.Fatalf("setup: %d attempts, expected the limit", dead[0].Attempts)
	}

	if err := runner.Replay(ctx, dead[0].ID); err != nil {
		t.Fatalf("replay: %v", err)
	}

	var replayed []webhooks.Delivery
	_ = store.Find(ctx, &replayed)
	if replayed[0].Attempts != 0 {
		t.Fatalf("a replayed delivery carries %d attempts forward; it will dead-letter "+
			"again on the first failure instead of retrying", replayed[0].Attempts)
	}

	// And the budget is real: the receiver is still failing, so this attempt
	// must leave the delivery pending rather than immediately dead.
	if _, err := runner.DrainOnce(ctx, 0); err != nil {
		t.Fatalf("drain after replay: %v", err)
	}
	var after []webhooks.Delivery
	_ = store.Find(ctx, &after)
	if after[0].Status != webhooks.StatusPending {
		t.Fatalf("status %s after one post-replay failure, want pending", after[0].Status)
	}
}
