package webhooks

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/internal/kernel/events"
	"github.com/sannados/sannad/pkg/modulekit"
)

// The dispatcher and the runner's queue existed with nothing connecting them to
// the bus, and nothing draining the queue on a schedule. This file is that
// wiring: a durable subscription turns every event into queued deliveries, and
// a ticking loop attempts what is due. Both are started once, at application
// startup, and stopped together at shutdown.

// ConsumerDurableName is the durable consumer name the webhook runner
// registers under.
//
// Fixed rather than configurable: two processes sharing this name share the
// work and the position, which is what makes a replicated deployment consume
// each event once rather than once per replica. A configurable name would
// let two unrelated deployments collide, or one deployment accidentally split
// its own consumption across two names on a typo.
const ConsumerDurableName = "sannad-webhooks"

// StartConsuming subscribes to every tenant's events and turns each into
// queued deliveries.
//
// filter is deliberately the whole stream (events.StreamSubjects), not one
// event at a time: an endpoint's Events field is what actually narrows
// delivery (see Endpoint.Wants), and subscribing per-event would mean a new
// event type never reaches an endpoint that asked for "everything" until this
// wiring was updated to know about it.
func (r *Runner) StartConsuming(ctx context.Context, consumer events.Consumer) (events.Subscription, error) {
	sub, err := consumer.Subscribe(ctx, ConsumerDurableName, events.StreamSubjects, r.handleDelivery)
	if err != nil {
		return nil, fmt.Errorf("webhooks: subscribe to event stream: %w", err)
	}
	return sub, nil
}

// handleDelivery adapts an events.Handler to Enqueue.
//
// The tenant is placed in context before Enqueue runs, because the runner's
// own storage calls are isolated like any other module's: Enqueue must act
// for the tenant the event belongs to, not for whichever tenant happened to
// be in the caller's context — the bus consumer this runs under sees every
// tenant's events on one connection.
func (r *Runner) handleDelivery(ctx context.Context, delivery events.Delivery) error {
	scoped := tenant.WithTenantID(ctx, delivery.TenantID)
	if err := r.Enqueue(scoped, delivery); err != nil {
		slog.ErrorContext(ctx, "webhooks: failed to queue delivery",
			"event", delivery.Event, "tenant", delivery.TenantID, "err", err)
		return err
	}
	return nil
}

// RunDrainLoop calls DrainOnce on an interval until ctx is cancelled.
//
// A loop rather than a per-delivery trigger: sending inline as events arrive
// would tie the bus consumer's acknowledgement to a third party's
// availability, which is exactly what Enqueue/DrainOnce being separate steps
// exists to avoid (see the package doc). Polling on an interval is the
// simplest thing that cannot make that mistake.
//
// Each pass runs under modulekit.WithSystemContext: this is the one place a
// due delivery for any tenant is drained in one pass, deliberately, and the
// escape is loud and greppable rather than a tenant value smuggled in some
// other way. Every row it touches was already written under its owning
// tenant's own context in Enqueue — nothing here decides which tenant a
// delivery belongs to, only which of the already-tenant-scoped rows are due.
func (r *Runner) RunDrainLoop(ctx context.Context, interval time.Duration, batchSize int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			drainCtx := modulekit.WithSystemContext(ctx)
			if _, err := r.DrainOnce(drainCtx, batchSize); err != nil {
				slog.ErrorContext(ctx, "webhooks: drain failed", "err", err)
			}
		}
	}
}

// DefaultDrainInterval is how often RunDrainLoop attempts due deliveries.
//
// Short enough that a fresh delivery is attempted quickly, long enough that
// an idle deployment is not issuing a query several times a second for
// nothing.
const DefaultDrainInterval = 5 * time.Second

// DefaultDrainBatchSize bounds how many deliveries one drain pass attempts,
// so a large backlog after an outage is worked through over several passes
// rather than blocking the loop for an unbounded time on one pass.
const DefaultDrainBatchSize = 100
