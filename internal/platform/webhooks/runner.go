package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/sannados/sannad/internal/kernel/events"
	"github.com/sannados/sannad/pkg/modulekit"
)

// The dispatcher had no way to run. This file connects it at both ends: events
// arriving from the bus become queued deliveries, and a worker drains the queue.
//
// Queue-then-send, rather than sending as events arrive. Sending inline would
// tie an event's acknowledgement to a third party's availability — a receiver
// down for an hour would hold the bus consumer for an hour, and every other
// event behind it. Writing the delivery is fast and local; the network happens
// on somebody else's schedule.

// Runner queues deliveries from the bus and drains them.
type Runner struct {
	store  modulekit.Store
	client *http.Client
	now    func() time.Time

	// jitter spreads retries. Without it every delivery queued during an outage
	// retries in the same instant, and the recovering receiver is knocked over
	// by the recovery.
	jitter func(time.Duration) time.Duration
}

// NewRunner builds a runner over a store.
func NewRunner(store modulekit.Store) *Runner {
	return &Runner{
		store:  store,
		client: &http.Client{Timeout: RequestTimeout},
		now:    time.Now,
		jitter: defaultJitter,
	}
}

// defaultJitter spreads a delay by up to 20% either way.
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := float64(d) * 0.2
	return d + time.Duration((rand.Float64()*2-1)*spread)
}

// Enqueue turns one event into deliveries, one per matching endpoint.
//
// Called from the bus consumer. It writes rows and returns; nothing here talks
// to a receiver.
func (r *Runner) Enqueue(ctx context.Context, delivery events.Delivery) error {
	var endpoints []Endpoint
	// Tenant-scoped by the isolation callbacks: the context carries the tenant
	// the event's subject named, so this cannot reach another tenant's
	// endpoints even though the consumer spans tenants.
	if err := r.store.Find(ctx, &endpoints, modulekit.Where("active", true)); err != nil {
		return fmt.Errorf("webhooks: list endpoints: %w", err)
	}

	eventID := eventIdentity(delivery)

	for i := range endpoints {
		endpoint := &endpoints[i]
		if !endpoint.Wants(delivery.Event) {
			continue
		}

		row := Delivery{
			ID:            uuid.NewString(),
			EndpointID:    endpoint.ID,
			EventID:       eventID,
			Event:         delivery.Event,
			Payload:       string(delivery.Data),
			Status:        StatusPending,
			NextAttemptAt: r.now(),
			CreatedAt:     r.now(),
			UpdatedAt:     r.now(),
		}

		if err := r.store.Create(ctx, &row); err != nil {
			// A unique index on (endpoint_id, event_id) makes a redelivery from
			// the bus a duplicate key rather than a second webhook. The bus is
			// at-least-once, so this is routine, not exceptional.
			if isDuplicate(err) {
				continue
			}
			return fmt.Errorf("webhooks: queue delivery for %s: %w", endpoint.ID, err)
		}
	}
	return nil
}

// eventIdentity derives a stable ID for an event.
//
// The stream sequence is unique and monotonic per stream, so it identifies a
// message precisely — and unlike a timestamp two events cannot share one. A
// consumer redelivered the same message sees the same identity, which is what
// makes the duplicate check work.
func eventIdentity(delivery events.Delivery) string {
	if delivery.Sequence > 0 {
		return fmt.Sprintf("%s-%d", delivery.TenantID, delivery.Sequence)
	}
	// No sequence means this did not come from JetStream. Falling back to a
	// random ID makes the delivery unique rather than silently colliding with
	// an unrelated event under a shared placeholder.
	return uuid.NewString()
}

// DrainOnce attempts every delivery currently due and returns how many it
// handled.
//
// One pass rather than a loop, so the caller owns the schedule and a test can
// run exactly one round.
func (r *Runner) DrainOnce(ctx context.Context, limit int) (int, error) {
	now := r.now()

	var due []Delivery
	err := r.store.Find(ctx, &due,
		modulekit.Where("status", StatusPending),
		modulekit.Compare("next_attempt_at", modulekit.OpLessEqual, now))
	if err != nil {
		return 0, fmt.Errorf("webhooks: list due deliveries: %w", err)
	}
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}

	handled := 0
	for i := range due {
		delivery := &due[i]

		var endpoint Endpoint
		if err := r.store.First(ctx, &endpoint, modulekit.Where("id", delivery.EndpointID)); err != nil {
			// The endpoint was deleted while deliveries were queued. There is
			// nowhere to send this and never will be, so it is dead rather than
			// retried forever against a destination that no longer exists.
			delivery.Status = StatusDead
			delivery.LastError = "endpoint no longer exists"
			delivery.UpdatedAt = now
			if err := r.save(ctx, delivery); err != nil {
				return handled, err
			}
			handled++
			continue
		}

		outcome := Attempt(ctx, r.client, &endpoint, delivery, r.now)
		Advance(delivery, outcome, r.now(), r.jitter)

		if err := r.save(ctx, delivery); err != nil {
			return handled, err
		}
		if delivery.Status == StatusDead {
			slog.WarnContext(ctx, "webhook delivery dead-lettered",
				"delivery", delivery.ID, "endpoint", endpoint.ID,
				"event", delivery.Event, "attempts", delivery.Attempts,
				"last_error", delivery.LastError)
		}
		handled++
	}
	return handled, nil
}

// Replay puts a dead delivery back in the queue.
//
// ADR 0002 names replay as table stakes, and this is why dead deliveries are
// kept rather than discarded: an operator who fixed a receiver needs to resend
// what it missed, and a deleted row cannot be resent.
//
// The attempt count resets, because the receiver being fixed is new information
// — carrying the old count forward would dead-letter it again almost at once.
func (r *Runner) Replay(ctx context.Context, deliveryID string) error {
	var delivery Delivery
	if err := r.store.First(ctx, &delivery, modulekit.Where("id", deliveryID)); err != nil {
		return fmt.Errorf("webhooks: read delivery %s: %w", deliveryID, err)
	}
	if delivery.Status == StatusDelivered {
		// Resending something the receiver already accepted would create a
		// duplicate side effect on their side for no reason.
		return fmt.Errorf("webhooks: delivery %s was already delivered", deliveryID)
	}

	now := r.now()
	return r.store.Update(ctx, &Delivery{}, map[string]any{
		"status":          StatusPending,
		"attempts":        0,
		"next_attempt_at": now,
		"last_error":      "",
		"updated_at":      now,
	}, modulekit.Where("id", deliveryID))
}

// ReplayEndpoint requeues every dead delivery for one endpoint, which is the
// shape of the actual request after an outage: "resend everything this
// integration missed."
func (r *Runner) ReplayEndpoint(ctx context.Context, endpointID string) (int, error) {
	var dead []Delivery
	err := r.store.Find(ctx, &dead,
		modulekit.Where("endpoint_id", endpointID),
		modulekit.Where("status", StatusDead))
	if err != nil {
		return 0, fmt.Errorf("webhooks: list dead deliveries: %w", err)
	}

	for i := range dead {
		if err := r.Replay(ctx, dead[i].ID); err != nil {
			return i, err
		}
	}
	return len(dead), nil
}

func (r *Runner) save(ctx context.Context, delivery *Delivery) error {
	changes := map[string]any{
		"status":           delivery.Status,
		"attempts":         delivery.Attempts,
		"next_attempt_at":  delivery.NextAttemptAt,
		"last_error":       delivery.LastError,
		"last_status_code": delivery.LastStatusCode,
		"updated_at":       delivery.UpdatedAt,
	}
	if delivery.DeliveredAt != nil {
		changes["delivered_at"] = *delivery.DeliveredAt
	}

	if err := r.store.Update(ctx, &Delivery{}, changes, modulekit.Where("id", delivery.ID)); err != nil {
		return fmt.Errorf("webhooks: save delivery %s: %w", delivery.ID, err)
	}
	return nil
}

func isDuplicate(err error) bool {
	return errors.Is(err, modulekit.ErrDuplicate)
}

// PayloadOf is a convenience for tests and tools that need the decoded body.
func PayloadOf(delivery Delivery, v any) error {
	return json.Unmarshal([]byte(delivery.Payload), v)
}
