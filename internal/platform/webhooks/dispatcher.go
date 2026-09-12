package webhooks

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"time"
)

// Retry policy.
//
// The shape matters more than the numbers. A receiver is usually down for
// seconds (a deploy) or hours (an outage), rarely in between, so the intervals
// grow fast enough to cover the second case without hammering the first.
const (
	// MaxAttempts before a delivery is dead. Roughly a day of coverage with the
	// backoff below — long enough to survive an unattended overnight outage,
	// short enough that a permanently broken endpoint stops consuming capacity.
	MaxAttempts = 8

	// BaseDelay is the first retry interval.
	BaseDelay = 10 * time.Second

	// MaxDelay caps the growth. Without a cap the last intervals stretch to days
	// and a recovered receiver waits pointlessly.
	MaxDelay = 6 * time.Hour

	// RequestTimeout bounds one attempt. A receiver that accepts the connection
	// and never responds would otherwise hold a worker indefinitely, which is
	// how one bad endpoint stalls delivery for every other.
	RequestTimeout = 15 * time.Second
)

// BackoffFor returns the delay before a given attempt number.
//
// Exponential, capped. Deterministic here and jittered by the caller: without
// jitter every delivery queued during an outage retries in the same instant, and
// the recovering receiver is knocked over by the recovery.
func BackoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Duration(float64(BaseDelay) * math.Pow(2, float64(attempt-1)))
	if delay > MaxDelay || delay <= 0 {
		return MaxDelay
	}
	return delay
}

// Outcome is what one attempt produced.
type Outcome struct {
	StatusCode int
	Err        error

	// Retryable distinguishes "try again" from "this will never work". A 410
	// Gone or a 401 will fail identically forever, and retrying it eight times
	// wastes a day before reaching the same conclusion.
	Retryable bool
}

// Attempt performs one delivery.
//
// Signing happens here rather than at queue time: the timestamp must be from the
// moment of the request, or a delivery that sat in a backoff queue for an hour
// arrives with a signature the receiver rejects as stale.
func Attempt(ctx context.Context, client *http.Client, endpoint *Endpoint, delivery *Delivery, now func() time.Time) Outcome {
	payload := []byte(delivery.Payload)
	timestamp := now()

	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, bytes.NewReader(payload))
	if err != nil {
		// A URL that will not parse will not parse next time either.
		return Outcome{Err: fmt.Errorf("webhooks: build request: %w", err), Retryable: false}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", timestamp.Unix()))
	req.Header.Set(HeaderSignature, Sign(endpoint.Secret, timestamp, payload))
	req.Header.Set(HeaderEvent, delivery.Event)
	req.Header.Set(HeaderDelivery, delivery.ID)
	// The source event's ID, not the delivery's: a receiver deduplicating on
	// this must treat two attempts of the same event as one, and the delivery ID
	// changes per attempt row.
	req.Header.Set(HeaderIdempotencyKey, delivery.EventID)

	resp, err := client.Do(req)
	if err != nil {
		// Transport failures — DNS, connection refused, timeout — are exactly
		// the transient case retries exist for.
		return Outcome{Err: fmt.Errorf("webhooks: post to %s: %w", endpoint.URL, err), Retryable: true}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Outcome{StatusCode: resp.StatusCode}
	}

	return Outcome{
		StatusCode: resp.StatusCode,
		Err:        fmt.Errorf("webhooks: %s returned %d", endpoint.URL, resp.StatusCode),
		Retryable:  retryableStatus(resp.StatusCode),
	}
}

// retryableStatus decides whether a status code is worth another attempt.
//
// 4xx means the receiver understood and refused: a wrong URL, a revoked
// credential, a payload it rejects. Those fail identically forever, and retrying
// them buys nothing while delaying the dead-letter that tells an operator to
// look. The exceptions are the two 4xx codes that are explicitly temporary.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		// 408 and 429 both mean "not now", which is precisely a retry.
		return true
	}
	return code >= 500
}

// Advance records an attempt's outcome on the delivery and returns whether it
// should be retried.
//
// The state transition lives here rather than in the caller so both the live
// dispatcher and a replay follow the same rules — two implementations of "when
// is a delivery dead" would eventually disagree.
func Advance(delivery *Delivery, outcome Outcome, now time.Time, jitter func(time.Duration) time.Duration) bool {
	delivery.Attempts++
	delivery.UpdatedAt = now
	delivery.LastStatusCode = outcome.StatusCode

	if outcome.Err == nil {
		delivery.Status = StatusDelivered
		delivery.LastError = ""
		delivered := now
		delivery.DeliveredAt = &delivered
		return false
	}

	delivery.LastError = outcome.Err.Error()

	if !outcome.Retryable || delivery.Attempts >= MaxAttempts {
		// Dead, not deleted. "Which events did this integration miss" is the
		// first question after an outage, and a discarded delivery cannot answer
		// it. Replay works from these rows.
		delivery.Status = StatusDead
		return false
	}

	delivery.Status = StatusPending
	delivery.NextAttemptAt = now.Add(jitter(BackoffFor(delivery.Attempts)))
	return true
}
