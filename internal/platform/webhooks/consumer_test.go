package webhooks_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sannados/sannad/internal/kernel/events"
	"github.com/sannados/sannad/internal/platform/webhooks"
)

// fakeConsumer stands in for the NATS-backed events.Consumer: it records the
// subscription request and lets a test drive the handler directly, without a
// broker.
type fakeConsumer struct {
	durable, filter string
	handler         events.Handler
}

func (c *fakeConsumer) Subscribe(_ context.Context, durable, filter string, handler events.Handler) (events.Subscription, error) {
	c.durable = durable
	c.filter = filter
	c.handler = handler
	return noopSubscription{}, nil
}

type noopSubscription struct{}

func (noopSubscription) Close() error { return nil }

// TestStartConsumingSubscribesToTheWholeStream: an endpoint's Events field is
// what narrows delivery (Endpoint.Wants), not the subject filter — so this
// must subscribe to every tenant's events, not one event at a time.
func TestStartConsumingSubscribesToTheWholeStream(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)
	consumer := &fakeConsumer{}

	if _, err := runner.StartConsuming(context.Background(), consumer); err != nil {
		t.Fatalf("start consuming: %v", err)
	}
	if consumer.durable != webhooks.ConsumerDurableName {
		t.Fatalf("durable = %q, want %q", consumer.durable, webhooks.ConsumerDurableName)
	}
	if consumer.filter != events.StreamSubjects {
		t.Fatalf("filter = %q, want the whole stream %q", consumer.filter, events.StreamSubjects)
	}
}

// TestConsumedEventIsQueuedUnderItsOwnTenant: the handler must write the
// delivery under the tenant the event actually belongs to, taken from the
// delivery itself, not from whatever tenant (if any) happened to be in the
// caller's context — the bus consumer this runs under sees every tenant's
// events on one connection.
func TestConsumedEventIsQueuedUnderItsOwnTenant(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)
	addEndpoint(t, store, tenantContext("acme"), "ep-acme", "https://acme.example/hook", "")

	consumer := &fakeConsumer{}
	if _, err := runner.StartConsuming(context.Background(), consumer); err != nil {
		t.Fatalf("start consuming: %v", err)
	}

	// The consumer's handler runs under a bare, tenant-less context — exactly
	// the shape a bus consumer spanning every tenant actually has.
	if err := consumer.handler(context.Background(), busEvent("acme", "orders.created", 1)); err != nil {
		t.Fatalf("handler: %v", err)
	}

	deliveries, err := runner.ListDeliveries(tenantContext("acme"), "ep-acme", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("got %d deliveries under acme, want 1", len(deliveries))
	}
}

// TestRunDrainLoopAttemptsDueDeliveriesAcrossTenants: the loop runs under a
// system context (see RunDrainLoop's doc comment) so one pass can drain every
// tenant's due deliveries, not only whichever tenant happened to be in the
// caller's context.
func TestRunDrainLoopAttemptsDueDeliveriesAcrossTenants(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)

	var acmeHits, globexHits atomic.Int32
	acmeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		acmeHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer acmeServer.Close()
	globexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		globexHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer globexServer.Close()

	addEndpoint(t, store, tenantContext("acme"), "ep-acme", acmeServer.URL, "")
	addEndpoint(t, store, tenantContext("globex"), "ep-globex", globexServer.URL, "")

	if err := runner.Enqueue(tenantContext("acme"), busEvent("acme", "orders.created", 1)); err != nil {
		t.Fatalf("enqueue acme: %v", err)
	}
	if err := runner.Enqueue(tenantContext("globex"), busEvent("globex", "orders.created", 1)); err != nil {
		t.Fatalf("enqueue globex: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.RunDrainLoop(ctx, 20*time.Millisecond, 10)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if acmeHits.Load() == 1 && globexHits.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if acmeHits.Load() != 1 {
		t.Fatalf("acme endpoint received %d requests, want 1", acmeHits.Load())
	}
	if globexHits.Load() != 1 {
		t.Fatalf("globex endpoint received %d requests, want 1", globexHits.Load())
	}
}
