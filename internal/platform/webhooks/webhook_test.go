package webhooks_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sannados/sannad/internal/platform/webhooks"
)

// The receiver is a system nobody here controls: it will be down, slow, return
// 200 and lose the message anyway, and occasionally process the same delivery
// twice. These tests assume that rather than hoping otherwise.

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func noJitter(d time.Duration) time.Duration { return d }

// TestSignatureVerifies is the base case: a receiver implementing the documented
// scheme accepts a genuine delivery.
func TestSignatureVerifies(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	payload := []byte(`{"event":"orders.created"}`)

	signature := webhooks.Sign("shared-secret", now, payload)
	if err := webhooks.Verify("shared-secret", signature, now, payload,
		webhooks.DefaultTolerance, now); err != nil {
		t.Fatalf("a genuine delivery was rejected: %v", err)
	}
}

// TestTamperedPayloadIsRejected is the property signing exists for. Anyone can
// POST to a webhook URL; only the holder of the secret can produce a signature
// over a body.
func TestTamperedPayloadIsRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	payload := []byte(`{"amount":100}`)
	signature := webhooks.Sign("shared-secret", now, payload)

	tampered := []byte(`{"amount":1000000}`)
	if err := webhooks.Verify("shared-secret", signature, now, tampered,
		webhooks.DefaultTolerance, now); !errors.Is(err, webhooks.ErrInvalidSignature) {
		t.Fatalf("a tampered payload verified: %v", err)
	}
}

func TestWrongSecretIsRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	payload := []byte(`{"event":"orders.created"}`)
	signature := webhooks.Sign("shared-secret", now, payload)

	if err := webhooks.Verify("someone-elses-secret", signature, now, payload,
		webhooks.DefaultTolerance, now); !errors.Is(err, webhooks.ErrInvalidSignature) {
		t.Fatalf("a signature verified under the wrong secret: %v", err)
	}
}

// TestCapturedDeliveryCannotBeReplayedForever. Without the timestamp inside the
// signed material, anyone who captured one delivery could resend it indefinitely
// and the signature would still verify.
func TestCapturedDeliveryCannotBeReplayedForever(t *testing.T) {
	signedAt := time.Unix(1_700_000_000, 0)
	payload := []byte(`{"event":"orders.created"}`)
	signature := webhooks.Sign("shared-secret", signedAt, payload)

	muchLater := signedAt.Add(time.Hour)
	err := webhooks.Verify("shared-secret", signature, signedAt, payload,
		webhooks.DefaultTolerance, muchLater)
	if !errors.Is(err, webhooks.ErrStaleSignature) {
		t.Fatalf("an hour-old delivery was accepted: %v", err)
	}
}

// TestTimestampIsPartOfTheSignature: a receiver that trusted the header without
// it being signed could have the timestamp rewritten by anyone in the middle,
// which would defeat the staleness check entirely.
func TestTimestampIsPartOfTheSignature(t *testing.T) {
	signedAt := time.Unix(1_700_000_000, 0)
	payload := []byte(`{"event":"orders.created"}`)
	signature := webhooks.Sign("shared-secret", signedAt, payload)

	// An attacker refreshes the timestamp to defeat the tolerance check.
	refreshed := signedAt.Add(30 * time.Minute)
	err := webhooks.Verify("shared-secret", signature, refreshed, payload,
		webhooks.DefaultTolerance, refreshed)
	if !errors.Is(err, webhooks.ErrInvalidSignature) {
		t.Fatalf("a rewritten timestamp still verified: %v", err)
	}
}

// TestBackoffGrowsAndIsCapped. Growth covers an hours-long outage; the cap stops
// the last intervals stretching to days, which would leave a recovered receiver
// waiting for nothing.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	previous := time.Duration(0)
	for attempt := 1; attempt <= webhooks.MaxAttempts; attempt++ {
		delay := webhooks.BackoffFor(attempt)
		if delay < previous {
			t.Fatalf("attempt %d waits %s, less than the previous %s", attempt, delay, previous)
		}
		if delay > webhooks.MaxDelay {
			t.Fatalf("attempt %d waits %s, above the %s cap", attempt, delay, webhooks.MaxDelay)
		}
		previous = delay
	}
}

func newDelivery() *webhooks.Delivery {
	return &webhooks.Delivery{
		ID:       "d-1",
		TenantID: "acme",
		EventID:  "evt-1",
		Event:    "orders.created",
		Payload:  `{"id":"o-1"}`,
		Status:   webhooks.StatusPending,
	}
}

// TestSuccessIsTerminal.
func TestSuccessIsTerminal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	delivery := newDelivery()

	retry := webhooks.Advance(delivery, webhooks.Outcome{StatusCode: 200}, now, noJitter)
	if retry {
		t.Fatal("a delivered webhook was scheduled for retry")
	}
	if delivery.Status != webhooks.StatusDelivered {
		t.Fatalf("status %s, want delivered", delivery.Status)
	}
	if delivery.DeliveredAt == nil {
		t.Error("no delivery time recorded")
	}
}

// TestPermanentFailureIsNotRetried: a 410 or a 401 fails identically forever.
// Retrying wastes a day before reaching the same conclusion, and delays the
// dead-letter that tells an operator to look.
func TestPermanentFailureIsNotRetried(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	delivery := newDelivery()

	retry := webhooks.Advance(delivery, webhooks.Outcome{
		StatusCode: http.StatusGone,
		Err:        errors.New("410"),
		Retryable:  false,
	}, now, noJitter)

	if retry {
		t.Fatal("a permanent failure was scheduled for retry")
	}
	if delivery.Status != webhooks.StatusDead {
		t.Fatalf("status %s, want dead", delivery.Status)
	}
}

// TestTransientFailureRetriesUntilExhausted, then dead-letters rather than
// retrying forever.
func TestTransientFailureRetriesUntilExhausted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	delivery := newDelivery()
	outcome := webhooks.Outcome{
		StatusCode: http.StatusServiceUnavailable,
		Err:        errors.New("503"),
		Retryable:  true,
	}

	for attempt := 1; attempt < webhooks.MaxAttempts; attempt++ {
		if !webhooks.Advance(delivery, outcome, now, noJitter) {
			t.Fatalf("attempt %d was not retried; a transient failure gave up early", attempt)
		}
		if delivery.Status != webhooks.StatusPending {
			t.Fatalf("attempt %d status %s, want pending", attempt, delivery.Status)
		}
		if !delivery.NextAttemptAt.After(now) {
			t.Fatalf("attempt %d scheduled no backoff", attempt)
		}
	}

	if webhooks.Advance(delivery, outcome, now, noJitter) {
		t.Fatal("retried past the attempt limit")
	}
	if delivery.Status != webhooks.StatusDead {
		t.Fatalf("final status %s, want dead", delivery.Status)
	}
	if delivery.LastError == "" {
		t.Error("a dead delivery records no reason; an operator cannot tell why")
	}
}

// TestDeadDeliveriesAreKept. "Which events did this integration miss" is the
// first question after an outage, and a discarded delivery cannot answer it.
// Replay reads these rows.
func TestDeadDeliveryRetainsItsPayload(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	delivery := newDelivery()
	payload := delivery.Payload

	webhooks.Advance(delivery, webhooks.Outcome{
		StatusCode: http.StatusGone, Err: errors.New("410"), Retryable: false,
	}, now, noJitter)

	if delivery.Payload != payload {
		t.Fatal("a dead delivery lost its payload and cannot be replayed")
	}
	if delivery.EventID == "" {
		t.Fatal("a dead delivery lost its event ID")
	}
}

// TestEndpointFiltering: an endpoint receives what it subscribed to, and an
// inactive one receives nothing.
func TestEndpointFiltering(t *testing.T) {
	specific := &webhooks.Endpoint{Active: true, Events: "orders.created, orders.paid"}
	if !specific.Wants("orders.created") {
		t.Error("a subscribed event was filtered out")
	}
	if specific.Wants("contacts.created") {
		t.Error("an unsubscribed event was accepted")
	}

	catchAll := &webhooks.Endpoint{Active: true}
	if !catchAll.Wants("anything.at.all") {
		t.Error("an empty filter should mean every event")
	}

	paused := &webhooks.Endpoint{Active: false, Events: "orders.created"}
	if paused.Wants("orders.created") {
		t.Error("a paused endpoint still wants events")
	}
}

// TestAttemptSignsWhatItSends is the end-to-end signing check: a receiver
// verifying exactly as documented accepts what the dispatcher produces. A
// signing scheme nobody verifies in-tree drifts from its documentation.
func TestAttemptSignsWhatItSends(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const secret = "shared-secret"

	var verifyErr error
	var gotIdempotencyKey, gotEvent string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)

		gotIdempotencyKey = r.Header.Get(webhooks.HeaderIdempotencyKey)
		gotEvent = r.Header.Get(webhooks.HeaderEvent)

		timestamp, err := webhooks.ParseTimestamp(r.Header.Get(webhooks.HeaderTimestamp))
		if err != nil {
			verifyErr = err
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		verifyErr = webhooks.Verify(secret, r.Header.Get(webhooks.HeaderSignature),
			timestamp, body, webhooks.DefaultTolerance, now)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	endpoint := &webhooks.Endpoint{URL: server.URL, Secret: secret, Active: true}
	delivery := newDelivery()

	outcome := webhooks.Attempt(context.Background(), server.Client(), endpoint, delivery, fixedNow(now))
	if outcome.Err != nil {
		t.Fatalf("attempt: %v", outcome.Err)
	}
	if verifyErr != nil {
		t.Fatalf("the receiver could not verify what we sent: %v", verifyErr)
	}
	// The source event's ID, not the delivery row's: a receiver deduplicating on
	// this must treat two attempts of one event as one event.
	if gotIdempotencyKey != delivery.EventID {
		t.Errorf("idempotency key %q, want the event ID %q", gotIdempotencyKey, delivery.EventID)
	}
	if gotEvent != delivery.Event {
		t.Errorf("event header %q, want %q", gotEvent, delivery.Event)
	}
}

// TestAttemptClassifiesFailures. The distinction is what separates a day of
// pointless retries from a prompt dead-letter.
func TestAttemptClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		wantRetryable bool
	}{
		{"server error", http.StatusInternalServerError, true},
		{"unavailable", http.StatusServiceUnavailable, true},
		{"rate limited", http.StatusTooManyRequests, true},
		{"request timeout", http.StatusRequestTimeout, true},
		{"gone", http.StatusGone, false},
		{"unauthorized", http.StatusUnauthorized, false},
		{"not found", http.StatusNotFound, false},
		{"bad request", http.StatusBadRequest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			endpoint := &webhooks.Endpoint{URL: server.URL, Secret: "s", Active: true}
			outcome := webhooks.Attempt(context.Background(), server.Client(),
				endpoint, newDelivery(), time.Now)

			if outcome.Err == nil {
				t.Fatalf("status %d was treated as success", tc.status)
			}
			if outcome.Retryable != tc.wantRetryable {
				t.Errorf("status %d retryable=%v, want %v", tc.status, outcome.Retryable, tc.wantRetryable)
			}
		})
	}
}

// TestUnreachableReceiverIsRetryable: DNS failures, refused connections, and
// timeouts are exactly the transient case retries exist for.
func TestUnreachableReceiverIsRetryable(t *testing.T) {
	endpoint := &webhooks.Endpoint{
		// Reserved TEST-NET-1 address: routable nowhere, so this fails without
		// depending on a name server.
		URL:    "http://192.0.2.1:9/hook",
		Secret: "s",
		Active: true,
	}

	client := &http.Client{Timeout: 500 * time.Millisecond}
	outcome := webhooks.Attempt(context.Background(), client, endpoint, newDelivery(), time.Now)

	if outcome.Err == nil {
		t.Fatal("an unreachable receiver reported success")
	}
	if !outcome.Retryable {
		t.Fatal("an unreachable receiver was treated as permanently failed")
	}
}

// TestSignatureComparisonIsConstantTime is an AST check rather than a
// behavioural one, because no functional test can catch what it guards.
//
// Replacing hmac.Equal with == passes every other test in this file: the
// signatures still match when they should and differ when they should. What
// changes is that a byte-by-byte comparison returns faster the earlier it finds
// a mismatch, which leaks how much of a forged signature was correct — enough to
// derive the rest one byte at a time, without ever knowing the secret.
//
// So the property is enforced where it is visible: the source must call
// hmac.Equal and must not compare signature strings directly.
func TestSignatureComparisonIsConstantTime(t *testing.T) {
	const source = "webhook.go"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, source, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}

	var usesHmacEqual bool
	var directComparison token.Pos

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "hmac" && sel.Sel.Name == "Equal" {
					usesHmacEqual = true
				}
			}
		case *ast.BinaryExpr:
			// A direct comparison involving anything named like a signature is
			// the shape this test exists to refuse.
			if node.Op != token.EQL && node.Op != token.NEQ {
				return true
			}
			for _, side := range []ast.Expr{node.X, node.Y} {
				if ident, ok := side.(*ast.Ident); ok {
					name := strings.ToLower(ident.Name)
					if strings.Contains(name, "signature") || name == "expected" {
						directComparison = node.Pos()
					}
				}
			}
		}
		return true
	})

	if !usesHmacEqual {
		t.Error("Verify does not use hmac.Equal; a variable-time comparison leaks " +
			"how much of a forged signature was correct, one byte at a time")
	}
	if directComparison.IsValid() {
		t.Errorf("%s: signatures are compared directly rather than with hmac.Equal",
			fset.Position(directComparison))
	}
}
