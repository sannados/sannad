package events

import "context"

// NoopPublisher discards every event without error. Use it when no message
// broker is configured (e.g. local development, unit tests).
type NoopPublisher struct{}

func (NoopPublisher) Publish(_ context.Context, _ string, _ any) error { return nil }

func (NoopPublisher) Close() error { return nil }
