package events

import "github.com/sannados/sannad/pkg/modulekit"

// Publisher publishes events to a named subject. Implementations may be backed
// by NATS JetStream, an in-process channel, or a no-op (for dev/tests when no
// message broker is configured).
type Publisher interface {
	modulekit.EventPublisher

	// Close releases any underlying connection. Safe to call multiple times.
	Close() error
}
