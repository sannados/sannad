// Package greeter consumes the directory.people.reader@v1 capability.
//
// It is the consumer half of the pair, and the thing worth noticing is what it
// does not import: there is no reference to examples/embedded/people anywhere in
// this file. It imports the contract and the SDK.
//
// That is what makes the provider replaceable. A developer who wants their own
// directory registers their own module under the same reference, does not
// install people, and greeter neither changes nor recompiles against anything
// new. If greeter imported people instead, both would be welded into one binary
// forever and the substitution would be impossible.
//
// Building on another module is optional, not the shape the framework imposes.
// Most modules stand alone; this is what the mechanism looks like for the ones
// that do not.
package greeter

import (
	"context"
	"fmt"

	"github.com/sannados/sannad/examples/contracts/directory"
	"github.com/sannados/sannad/pkg/modulekit"
)

// Greeting is this module's own contract, unrelated to the directory's.
var Capability = modulekit.CapabilityRef{Name: "greeter.greetings.reader", Version: "v1"}

type GreetRequest struct {
	Limit int `json:"limit"`
}

type GreetResponse struct {
	Greetings []string `json:"greetings"`
}

// Module builds greetings from whoever provides the directory contract.
type Module struct {
	// caller is the dispatch surface, held as the SDK interface rather than as
	// the kernel bus type. A module names no kernel package.
	caller modulekit.Caller
}

func New(caller modulekit.Caller) *Module {
	return &Module{caller: caller}
}

func (m *Module) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:   "com.example.greeter",
		Kind: "embedded",
		// Greeter persists nothing: it reads through another module's contract
		// and returns a derived value.
		Storage: modulekit.StorageNone,
		// Consumes is a declaration that this module needs someone to provide
		// the directory contract. The registry validates it at wiring time, so
		// a missing provider is a startup error rather than a nil dereference on
		// the first request.
		Provides: []modulekit.CapabilityRef{Capability},
		Consumes: []modulekit.CapabilityRef{directory.Reader},
	}
}

func (m *Module) Install(reg modulekit.Registrar) error {
	return modulekit.HandleTyped(reg, Capability, m.greet)
}

func (m *Module) Start(context.Context) error { return nil }
func (m *Module) Stop(context.Context) error  { return nil }

func (m *Module) greet(ctx context.Context, req GreetRequest) (GreetResponse, error) {
	// CallTyped, not Call: the request and response types are checked at compile
	// time. Without it this line ends in a runtime type assertion on an `any`,
	// which is the failure two Go modules have no reason to accept.
	people, err := modulekit.CallTyped[directory.ListRequest, directory.ListResponse](
		ctx, m.caller, directory.Reader, directory.ListRequest{Limit: req.Limit})
	if err != nil {
		// Wrapped, not swallowed: a provider failure is this module's failure to
		// answer, and the caller needs to know which contract broke.
		return GreetResponse{}, fmt.Errorf("greeter: read directory: %w", err)
	}

	greetings := make([]string, 0, len(people.People))
	for _, person := range people.People {
		greetings = append(greetings, "Hello, "+person.Name)
	}
	return GreetResponse{Greetings: greetings}, nil
}
