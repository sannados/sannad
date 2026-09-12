package bus

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/pkg/modulekit"
)

type Bus struct {
	registry *registry.Registry
}

func New(reg *registry.Registry) *Bus {
	return &Bus{registry: reg}
}

func (b *Bus) Call(ctx context.Context, ref modulekit.CapabilityRef, request any) (any, error) {
	tracer := otel.Tracer("sannad.kernel.bus")
	ctx, span := tracer.Start(ctx, "bus.Call/"+ref.Key())
	defer span.End()
	span.SetAttributes(
		attribute.String("capability.name", ref.Name),
		attribute.String("capability.version", ref.Version),
	)

	handler, ok := b.registry.ResolveCapability(ref)
	if !ok {
		err := fmt.Errorf("bus: capability %s not registered", ref.Key())
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	result, err := handler(ctx, request)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return result, err
}
