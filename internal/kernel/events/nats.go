package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	natsConnectTimeout = 5 * time.Second
	natsPublishTimeout = 3 * time.Second
)

// NATSPublisher publishes events to NATS JetStream subjects.
// Construct via NewNATSPublisher; close with Close when the process shuts down.
type NATSPublisher struct {
	conn *nats.Conn
	js   nats.JetStreamContext
}

// NewNATSPublisher connects to the NATS server at url and returns a publisher
// backed by JetStream. Returns an error if the connection or JetStream context
// cannot be established.
func NewNATSPublisher(url string) (*NATSPublisher, error) {
	conn, err := nats.Connect(url,
		nats.Timeout(natsConnectTimeout),
		nats.MaxReconnects(5),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("events: nats connect %s: %w", url, err)
	}

	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("events: nats jetstream: %w", err)
	}

	// The stream is declared here rather than left to an operator. JetStream
	// only retains messages on subjects a stream binds, so without this every
	// publish fails on ack — and the failure reads as a broker problem rather
	// than a missing declaration.
	if err := EnsureStream(js); err != nil {
		conn.Close()
		return nil, err
	}

	return &NATSPublisher{conn: conn, js: js}, nil
}

// Publish serialises payload as JSON and publishes it to the given NATS subject.
// The context deadline is honoured for the publish call; if the context is
// already cancelled the publish is skipped and the context error is returned.
func (p *NATSPublisher) Publish(ctx context.Context, subject string, payload any) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("events: marshal payload for %s: %w", subject, err)
	}

	// Derive a timeout for the JetStream publish from the context or a default.
	timeout := natsPublishTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}

	_, err = p.js.Publish(subject, data, nats.AckWait(timeout))
	if err != nil {
		return fmt.Errorf("events: publish to %s: %w", subject, err)
	}
	return nil
}

// Close drains and closes the underlying NATS connection.
func (p *NATSPublisher) Close() error {
	if err := p.conn.Drain(); err != nil {
		// Drain failed; force-close to avoid resource leak.
		p.conn.Close()
		return fmt.Errorf("events: nats drain: %w", err)
	}
	return nil
}
