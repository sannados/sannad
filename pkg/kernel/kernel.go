// Package kernel is the compatibility façade for the lightweight in-process
// host. New applications should import github.com/sannados/sannad and use
// sannad.App, which includes storage, tenancy, Kayan, bridges, and WASM.
package kernel

import (
	"context"
	"fmt"

	"github.com/sannados/sannad/internal/kernel/bus"
	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/pkg/modulekit"
)

// Kernel preserves the original lightweight API while delegating registration,
// hooks, validation, lifecycle ordering, and dispatch to the real kernel.
//
// Deprecated: use the root sannad package for applications.
type Kernel struct {
	registry *registry.Registry
	bus      *bus.Bus
}

// New returns an empty lightweight kernel.
// Deprecated: use sannad.New for applications.
func New() *Kernel {
	reg := registry.New()
	return &Kernel{registry: reg, bus: bus.New(reg)}
}

// RegisterCapability implements modulekit.Registrar for compatibility with
// callers that register handlers directly.
func (k *Kernel) RegisterCapability(ref modulekit.CapabilityRef, handler modulekit.Handler) error {
	return k.registry.RegisterCapability(ref, handler)
}

// Add installs a module and enrolls it in the real kernel lifecycle.
func (k *Kernel) Add(module modulekit.Module) error {
	if err := k.registry.RegisterModule(module); err != nil {
		return fmt.Errorf("kernel: install %s: %w", module.Descriptor().ID, err)
	}
	return nil
}

// Call dispatches through the real capability bus.
func (k *Kernel) Call(ctx context.Context, ref modulekit.CapabilityRef, request any) (any, error) {
	return k.bus.Call(ctx, ref, request)
}

// Start starts modules in registration order and rolls back prior starts if one fails.
func (k *Kernel) Start(ctx context.Context) error { return k.registry.StartAll(ctx) }

// Stop preserves the original void signature and stops modules in reverse order.
func (k *Kernel) Stop(ctx context.Context) { _ = k.registry.StopAll(ctx) }

var (
	_ modulekit.Registrar = (*Kernel)(nil)
	_ modulekit.Caller    = (*Kernel)(nil)
)
