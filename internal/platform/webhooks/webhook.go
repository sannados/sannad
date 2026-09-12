// Package webhooks delivers kernel events to external HTTP endpoints.
//
// ADR 0002 makes webhooks one consumer of the event backbone rather than a
// separate mechanism, and names five things as table stakes: HMAC signing,
// retries with exponential backoff, idempotency keys, a dead-letter path, and
// replay of missed events. Stripe and Shopify both set that expectation, and an
// integrator who has used either will notice immediately if any is missing.
//
// The hard part is not the HTTP request. It is that the receiver is a system
// nobody here controls: it will be down, it will be slow, it will return 200 and
// lose the message anyway, and it will occasionally process the same delivery
// twice. Every design decision below follows from assuming that rather than
// hoping otherwise.
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Endpoint is a registered webhook destination.
//
// Tenant-scoped like any other model: one tenant must not be able to see, edit,
// or receive another's endpoints.
type Endpoint struct {
	ID       string
	TenantID string

	// URL is where deliveries are POSTed.
	URL string

	// Events are the event names this endpoint receives. Empty means every
	// event in the tenant — a deliberate choice an integrator makes, not a
	// default, because it means new event types start arriving without warning.
	Events string

	// Secret signs deliveries. It is the shared secret between this deployment
	// and the receiver, and is the only thing that lets the receiver distinguish
	// a genuine delivery from anyone who learned the URL.
	Secret string

	// Active allows an endpoint to be paused without deleting it, which is what
	// makes an outage recoverable rather than a re-registration.
	Active bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

func (e *Endpoint) GetTenantID() string   { return e.TenantID }
func (e *Endpoint) SetTenantID(id string) { e.TenantID = id }

func (Endpoint) TableName() string { return "webhook_endpoints" }

// Wants reports whether this endpoint should receive an event.
func (e *Endpoint) Wants(event string) bool {
	if !e.Active {
		return false
	}
	if strings.TrimSpace(e.Events) == "" {
		return true
	}
	for _, want := range strings.Split(e.Events, ",") {
		if strings.TrimSpace(want) == event {
			return true
		}
	}
	return false
}

// DeliveryStatus is where a delivery attempt stands.
type DeliveryStatus string

const (
	// StatusPending is queued, not yet attempted or awaiting retry.
	StatusPending DeliveryStatus = "pending"

	// StatusDelivered is acknowledged by the receiver. Terminal.
	StatusDelivered DeliveryStatus = "delivered"

	// StatusDead exhausted its retries. Terminal, and the state that needs a
	// human: the event happened, the receiver never accepted it, and no
	// automatic path will change that.
	//
	// Kept rather than discarded, because "which events did this integration
	// miss" is the first question asked after an outage, and a delivery that was
	// deleted cannot answer it.
	StatusDead DeliveryStatus = "dead"
)

// Delivery is one event addressed to one endpoint.
//
// One row per (event, endpoint) rather than per event: two endpoints subscribed
// to the same event fail and retry independently, and a shared row would let one
// slow receiver hold up another.
type Delivery struct {
	ID         string
	TenantID   string
	EndpointID string

	// EventID is the source event's identity, and is what makes redelivery
	// detectable. It travels to the receiver as the idempotency key.
	EventID string
	Event   string
	Payload string

	Status   DeliveryStatus
	Attempts int

	// NextAttemptAt is when this delivery becomes eligible again. Backoff is
	// stored rather than held in memory so a restart does not retry everything
	// at once — which is how a recovering system finishes off a receiver that
	// was only briefly down.
	NextAttemptAt time.Time

	// LastError records why the last attempt failed, for an operator reading a
	// dead delivery. Status codes alone do not explain a TLS failure or a
	// timeout.
	LastError string

	// LastStatusCode is the receiver's last HTTP status, kept separately because
	// "which of my deliveries got a 410" is a question with an obvious answer
	// only if the code is queryable.
	LastStatusCode int

	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeliveredAt *time.Time
}

func (d *Delivery) GetTenantID() string   { return d.TenantID }
func (d *Delivery) SetTenantID(id string) { d.TenantID = id }

func (Delivery) TableName() string { return "webhook_deliveries" }

// Signature header names. These follow the shape Stripe established, because an
// integrator who has implemented one webhook receiver should not have to learn a
// new scheme to implement this one.
const (
	// HeaderSignature carries the HMAC.
	HeaderSignature = "X-Sannad-Signature"

	// HeaderTimestamp carries the signing timestamp, which is what makes replay
	// detectable on the receiver's side.
	HeaderTimestamp = "X-Sannad-Timestamp"

	// HeaderIdempotencyKey is the source event's ID. A receiver that records it
	// can recognise a redelivery, which it will see: delivery is at-least-once
	// by construction, since a response lost in transit is indistinguishable
	// from a receiver that never got the request.
	HeaderIdempotencyKey = "X-Sannad-Idempotency-Key"

	// HeaderEvent names the event, so a receiver can route without parsing the
	// body.
	HeaderEvent = "X-Sannad-Event"

	// HeaderDelivery identifies this attempt, for correlating logs across two
	// systems.
	HeaderDelivery = "X-Sannad-Delivery"
)

// ErrInvalidSignature is returned when a signature does not verify.
var ErrInvalidSignature = errors.New("webhooks: signature does not match")

// ErrStaleSignature is returned when a signature is valid but too old.
var ErrStaleSignature = errors.New("webhooks: signature timestamp is outside the tolerance")

// Sign computes the signature for a payload at a moment in time.
//
// The timestamp is inside the signed material, not merely alongside it. Signing
// the body alone would let anyone who captured one delivery replay it forever,
// and the receiver would have no way to tell: the signature would still verify.
// With the timestamp signed, a replayed delivery is either stale or has a
// signature that no longer matches.
func Sign(secret string, timestamp time.Time, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", timestamp.Unix())
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature against a payload.
//
// Provided so this deployment's own receivers, and the tests, verify exactly the
// way an integrator would — a signing scheme nobody verifies in-tree is one that
// drifts from its documentation.
//
// tolerance bounds how old a delivery may be. Without it a captured request
// stays valid forever.
func Verify(secret, signature string, timestamp time.Time, payload []byte, tolerance time.Duration, now time.Time) error {
	expected := Sign(secret, timestamp, payload)

	// Constant-time comparison. A byte-by-byte compare leaks how much of a
	// forged signature was correct, which is enough to derive the rest one byte
	// at a time.
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return ErrInvalidSignature
	}

	// Checked after the signature, so an attacker cannot use timing differences
	// on the cheaper check to probe anything.
	age := now.Sub(timestamp)
	if age < 0 {
		age = -age
	}
	if age > tolerance {
		return fmt.Errorf("%w: %s old", ErrStaleSignature, age)
	}
	return nil
}

// ParseTimestamp reads the timestamp header.
func ParseTimestamp(header string) (time.Time, error) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("webhooks: invalid timestamp header %q", header)
	}
	return time.Unix(seconds, 0), nil
}

// DefaultTolerance is how far a delivery's timestamp may be from the receiver's
// clock. Five minutes absorbs ordinary clock skew without leaving a captured
// request replayable for long.
const DefaultTolerance = 5 * time.Minute
