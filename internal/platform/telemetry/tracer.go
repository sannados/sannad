// Package telemetry initializes OpenTelemetry tracing and exposes a shutdown
// hook. Call Setup at application startup and Shutdown during graceful stop.
package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Provider wraps an OTel TracerProvider and knows how to shut itself down.
type Provider struct {
	inner sdktrace.TracerProvider
	noop  bool
}

// Setup initialises the global OTel TracerProvider.
//   - endpoint empty → no-op provider (dev / test; zero overhead).
//   - endpoint set   → OTLP/gRPC exporter to the given address
//     (e.g. "localhost:4317" for a local Jaeger or OTel Collector).
//
// The returned Provider must be closed during graceful shutdown.
func Setup(ctx context.Context, serviceName, endpoint string) (*Provider, error) {
	if endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return &Provider{noop: true}, nil
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceNameKey.String(serviceName)),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	slog.Info("telemetry: OTLP tracing enabled", "endpoint", endpoint, "service", serviceName)
	return &Provider{inner: *tp}, nil
}

// Shutdown flushes pending spans and closes the exporter.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.noop {
		return nil
	}
	return p.inner.Shutdown(ctx)
}

// Tracer returns a named tracer from the global provider.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}
