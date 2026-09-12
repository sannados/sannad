// Package people provides the directory.people.reader@v1 capability.
//
// It is the provider half of the pair that demonstrates a module calling
// another module. It imports the contract package and the SDK, and nothing
// else — in particular it does not know that examples/embedded/greeter exists,
// which is the point.
package people

import (
	"context"

	"github.com/sannados/sannad/examples/contracts/directory"
	"github.com/sannados/sannad/pkg/modulekit"
)

// Module serves the directory contract. A real one would read a store; this one
// holds a fixed list, because what is being demonstrated is the wiring, not the
// storage.
type Module struct {
	people []directory.Person
}

func New(people ...directory.Person) *Module {
	return &Module{people: people}
}

func (m *Module) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:   "com.example.people",
		Kind: "embedded",
		// The fixed list lives in memory, so there is nothing for the kernel to
		// isolate and nothing for the audit to inspect.
		Storage: modulekit.StorageNone,
		// Provides names the contract, not a type in this package. A consumer
		// resolves the same reference without ever naming this module.
		Provides: []modulekit.CapabilityRef{directory.Reader},
	}
}

func (m *Module) Install(reg modulekit.Registrar) error {
	// HandleTyped: the handler takes and returns the contract's concrete types.
	// The registry stores an untyped handler underneath, because it holds
	// capabilities whose types it cannot know — but that stays inside the
	// registry rather than becoming a cast in every caller.
	return modulekit.HandleTyped(reg, directory.Reader, m.list)
}

func (m *Module) Start(context.Context) error { return nil }
func (m *Module) Stop(context.Context) error  { return nil }

func (m *Module) list(_ context.Context, req directory.ListRequest) (directory.ListResponse, error) {
	people := m.people
	if req.Limit > 0 && req.Limit < len(people) {
		people = people[:req.Limit]
	}
	return directory.ListResponse{People: people}, nil
}
