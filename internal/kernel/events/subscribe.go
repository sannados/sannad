package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// Nothing consumed the event bus before this file existed. Publisher had exactly
// Publish and Close, so an event went into JetStream and no part of the system
// could react to it — which also meant webhooks were not blocked on webhook
// code, they were blocked on there being a consumer at all.
//
// Two things a consumer needs that a publisher does not:
//
//   - A stream. JetStream only retains messages on subjects a stream binds, and
//     nothing here ever declared one. A publish to an unbound subject fails on
//     ack, so the existing publisher worked only against a server somebody had
//     configured by hand.
//   - Durability. A consumer that loses its position on restart either replays
//     everything or skips what arrived while it was down. Durable consumers make
//     the position the server's business rather than each consumer's.

// Delivery is one event handed to a consumer.
type Delivery struct {
	// Subject is the full subject, including the tenant segment.
	Subject string

	// TenantID is the tenant the event belongs to, taken from the subject rather
	// than from the payload. A payload field would be whatever the publisher
	// wrote; the subject is what the broker routed on.
	TenantID string

	// Event is the event name with the prefix and tenant stripped.
	Event string

	// Data is the raw encoded payload. Consumers decode it into their own type,
	// so the bus stays free of every module's schema.
	Data []byte

	// Sequence is the stream position, which is what makes ordering and replay
	// meaningful. Unlike a timestamp it is unique and monotonic.
	Sequence uint64

	// Deliveries counts how many times this message has been delivered. Greater
	// than one means a redelivery, which is normal and is exactly why consumers
	// must be idempotent.
	Deliveries uint64
}

// Decode unmarshals the payload into v.
func (d Delivery) Decode(v any) error {
	if err := json.Unmarshal(d.Data, v); err != nil {
		return fmt.Errorf("events: decode %s: %w", d.Subject, err)
	}
	return nil
}

// Handler processes one delivery.
//
// Returning nil acknowledges the message and it is not redelivered. Returning an
// error leaves it unacknowledged for redelivery after the ack wait — so a
// handler must be idempotent, because at-least-once delivery guarantees it will
// eventually see the same event twice. modulekit.Idempotent exists for this.
type Handler func(ctx context.Context, delivery Delivery) error

// Subscription is a running consumer. Close stops it.
type Subscription interface {
	Close() error
}

// Consumer subscribes to events.
//
// Separate from Publisher because most modules only publish, and a module that
// takes a Publisher should not thereby gain the ability to consume every tenant's
// events.
type Consumer interface {
	// Subscribe delivers events matching filter to handler.
	//
	// durable names the consumer's position on the server. Two processes sharing
	// a durable name share the work and the position, which is what makes a
	// replicated deployment consume each event once rather than once per
	// replica. An empty durable name is refused: an ephemeral consumer silently
	// loses everything published while it was down, which is indistinguishable
	// from working until something is missing.
	Subscribe(ctx context.Context, durable, filter string, handler Handler) (Subscription, error)
}

// EnsureStream declares the stream Sannad publishes to, creating it if absent
// and leaving an existing one alone.
//
// Called at startup rather than left to operators. A publisher against an
// undeclared stream fails on every ack, and the failure reads as a broker
// problem rather than a missing declaration.
func EnsureStream(js nats.JetStreamContext) error {
	config := &nats.StreamConfig{
		Name:     StreamName,
		Subjects: []string{StreamSubjects},
		// File storage: an event nobody consumed yet must survive a broker
		// restart, or the durability the consumers are built on is fiction.
		Storage: nats.FileStorage,
		// Limits, not WorkQueue: several consumers legitimately read the same
		// event — a projection, a webhook dispatcher, an audit log — and
		// WorkQueue would give it to exactly one of them.
		Retention: nats.LimitsPolicy,
		MaxAge:    streamMaxAge,
		Discard:   nats.DiscardOld,
	}

	existing, err := js.StreamInfo(StreamName)
	if err == nil {
		// Already declared. Subjects are widened if a previous version bound
		// less, but nothing else is overwritten: an operator's retention or
		// replication settings are theirs, and silently resetting them at every
		// boot would be worse than not managing the stream at all.
		if !bindsSubjects(existing.Config.Subjects, StreamSubjects) {
			updated := existing.Config
			updated.Subjects = append(updated.Subjects, StreamSubjects)
			if _, err := js.UpdateStream(&updated); err != nil {
				return fmt.Errorf("events: widen stream %s: %w", StreamName, err)
			}
		}
		return nil
	}

	if _, err := js.AddStream(config); err != nil {
		return fmt.Errorf("events: declare stream %s: %w", StreamName, err)
	}
	return nil
}

// streamMaxAge bounds how long an unconsumed event is retained.
//
// Finite on purpose: unbounded retention turns a broker outage into unbounded
// disk growth. Long enough that a consumer down for a weekend catches up.
const streamMaxAge = 7 * 24 * time.Hour

func bindsSubjects(existing []string, want string) bool {
	for _, s := range existing {
		if s == want {
			return true
		}
	}
	return false
}

// Subscribe implements Consumer on the NATS publisher, which already holds the
// connection and JetStream context.
func (p *NATSPublisher) Subscribe(ctx context.Context, durable, filter string, handler Handler) (Subscription, error) {
	if durable == "" {
		return nil, fmt.Errorf("events: a durable name is required; an ephemeral consumer loses everything published while it is down")
	}
	if filter == "" {
		return nil, fmt.Errorf("events: a subject filter is required")
	}

	sub, err := p.js.QueueSubscribe(filter, durable, func(msg *nats.Msg) {
		delivery, err := deliveryFrom(msg)
		if err != nil {
			// A message whose subject does not parse cannot be routed to a
			// tenant, and guessing would be a cross-tenant delivery. Terminate
			// rather than redeliver: redelivering will not make it parse.
			_ = msg.Term()
			return
		}

		if err := handler(ctx, delivery); err != nil {
			// Unacknowledged, so it is redelivered after the ack wait. The
			// handler is required to be idempotent, so a redelivery is safe.
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	},
		nats.Durable(durable),
		nats.ManualAck(),
		nats.AckWait(consumerAckWait),
		// Deliver everything the stream still holds, not only what arrives after
		// this call. A consumer starting fresh must see the backlog, or its
		// first run silently skips history.
		nats.DeliverAll(),
	)
	if err != nil {
		return nil, fmt.Errorf("events: subscribe %s as %s: %w", filter, durable, err)
	}
	return natsSubscription{sub: sub}, nil
}

// consumerAckWait is how long the server waits for an ack before redelivering.
const consumerAckWait = 30 * time.Second

type natsSubscription struct{ sub *nats.Subscription }

// Close unsubscribes without deleting the durable consumer, so the position
// survives and a restart resumes rather than replaying from the beginning.
func (s natsSubscription) Close() error {
	if err := s.sub.Drain(); err != nil {
		return fmt.Errorf("events: drain subscription: %w", err)
	}
	return nil
}

func deliveryFrom(msg *nats.Msg) (Delivery, error) {
	tenantID, ok := TenantOf(msg.Subject)
	if !ok {
		return Delivery{}, fmt.Errorf("%w: %q", ErrInvalidSubject, msg.Subject)
	}

	// Prefix and tenant stripped, leaving the event name the publisher used.
	event := msg.Subject[len(SubjectPrefix)+len(tenantID)+2:]

	delivery := Delivery{
		Subject:  msg.Subject,
		TenantID: tenantID,
		Event:    event,
		Data:     msg.Data,
	}

	// Metadata is absent on a non-JetStream message, which should not happen
	// here but must not panic if it does.
	if meta, err := msg.Metadata(); err == nil {
		delivery.Sequence = meta.Sequence.Stream
		delivery.Deliveries = meta.NumDelivered
	}
	return delivery, nil
}
