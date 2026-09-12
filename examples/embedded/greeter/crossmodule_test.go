package greeter_test

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/sannados/sannad/examples/contracts/directory"
	"github.com/sannados/sannad/examples/embedded/greeter"
	"github.com/sannados/sannad/examples/embedded/people"
	"github.com/sannados/sannad/internal/kernel/bus"
	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/pkg/modulekit"
)

// This is the first test in the repository of one module calling another
// module's capability.
//
// Until now nothing exercised that path: examples/embedded/crm is its own only
// caller, and the conformance suite proves extension through *hooks* with no
// shared code, which is a different mechanism. So the central claim — that a
// caller depends on a reference rather than on an implementation — was asserted
// in ADRs and never executed.
//
// The test package imports both modules because wiring them together is exactly
// what an entrypoint does. What matters is that neither module imports the
// other, which TestModulesDoNotImportEachOther checks structurally rather than
// trusting the comment above.

func wire(t *testing.T, provider modulekit.Module) (*bus.Bus, *registry.Registry) {
	t.Helper()
	reg := registry.New()
	b := bus.New(reg)

	if provider != nil {
		if err := reg.RegisterModule(provider); err != nil {
			t.Fatalf("register provider: %v", err)
		}
	}
	if err := reg.RegisterModule(greeter.New(b)); err != nil {
		t.Fatalf("register greeter: %v", err)
	}
	return b, reg
}

func TestModuleCallsAnotherModule(t *testing.T) {
	b, _ := wire(t, people.New(
		directory.Person{ID: "1", Name: "Amina", Email: "amina@example.com"},
		directory.Person{ID: "2", Name: "Omar", Email: "omar@example.com"},
	))

	resp, err := modulekit.CallTyped[greeter.GreetRequest, greeter.GreetResponse](
		context.Background(), b, greeter.Capability, greeter.GreetRequest{})
	if err != nil {
		t.Fatalf("call greeter: %v", err)
	}

	if len(resp.Greetings) != 2 {
		t.Fatalf("got %d greetings, want 2: %v", len(resp.Greetings), resp.Greetings)
	}
	if resp.Greetings[0] != "Hello, Amina" {
		t.Fatalf("got %q, want %q", resp.Greetings[0], "Hello, Amina")
	}
}

// TestProviderIsReplaceable is the property the whole indirection exists for. A
// different module registers the same contract; the consumer is unchanged and
// unaware.
//
// If greeter imported people, this test could not be written at all.
type altProvider struct{}

func (altProvider) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       "com.example.alt-directory",
		Kind:     "embedded",
		Provides: []modulekit.CapabilityRef{directory.Reader},
		Storage:  modulekit.StorageNone,
	}
}

func (altProvider) Install(reg modulekit.Registrar) error {
	return modulekit.HandleTyped(reg, directory.Reader,
		func(context.Context, directory.ListRequest) (directory.ListResponse, error) {
			return directory.ListResponse{People: []directory.Person{
				{ID: "9", Name: "Someone Else"},
			}}, nil
		})
}

func (altProvider) Start(context.Context) error { return nil }
func (altProvider) Stop(context.Context) error  { return nil }

func TestProviderIsReplaceable(t *testing.T) {
	b, _ := wire(t, altProvider{})

	resp, err := modulekit.CallTyped[greeter.GreetRequest, greeter.GreetResponse](
		context.Background(), b, greeter.Capability, greeter.GreetRequest{})
	if err != nil {
		t.Fatalf("call greeter: %v", err)
	}

	if len(resp.Greetings) != 1 || resp.Greetings[0] != "Hello, Someone Else" {
		t.Fatalf("consumer did not follow the replaced provider: %v", resp.Greetings)
	}
}

// TestMissingProviderFailsAtWiring: a declared dependency with nobody providing
// it must fail while the system is being assembled, not on the first request in
// production.
func TestMissingProviderFailsAtWiring(t *testing.T) {
	reg := registry.New()
	b := bus.New(reg)
	if err := reg.RegisterModule(greeter.New(b)); err != nil {
		t.Fatalf("register greeter: %v", err)
	}

	// StartAll validates Consumes. Without that check the failure surfaces as a
	// capability-not-registered error on the first call instead.
	err := reg.StartAll(context.Background())
	if err == nil {
		t.Fatal("a module whose declared dependency has no provider started cleanly")
	}
	if !strings.Contains(err.Error(), directory.Reader.Name) {
		t.Errorf("error does not name the missing contract: %v", err)
	}
}

// TestRequestTypeMismatchIsReported covers the seam CallTyped cannot close. The
// generic parameters are checked at compile time, so a caller cannot pass the
// wrong type through CallTyped — but the untyped Call underneath is still
// reachable, and a mismatch there must be an error rather than a panic.
func TestRequestTypeMismatchIsReported(t *testing.T) {
	b, _ := wire(t, people.New())

	_, err := b.Call(context.Background(), directory.Reader, "not a ListRequest")
	if err == nil {
		t.Fatal("a wrong request type was accepted")
	}
	if !errors.Is(err, modulekit.ErrTypeMismatch) {
		t.Fatalf("expected ErrTypeMismatch, got %v", err)
	}
}

// TestModulesDoNotImportEachOther enforces the claim structurally.
//
// Both modules live in one repository, so nothing but this test stops a future
// edit from importing the provider directly for convenience. Every behavioural
// test above would keep passing while the decoupling quietly became false — the
// provider would no longer be replaceable and could never move to another tier.
func TestModulesDoNotImportEachOther(t *testing.T) {
	for _, pair := range []struct{ pkg, forbidden string }{
		{"../greeter", "examples/embedded/people"},
		{"../people", "examples/embedded/greeter"},
	} {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, pair.pkg, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", pair.pkg, err)
		}

		for name, parsed := range pkgs {
			// Test files may import both: wiring modules together is what an
			// entrypoint does, and this file is standing in for one.
			if strings.HasSuffix(name, "_test") {
				continue
			}
			for path, file := range parsed.Files {
				for _, spec := range file.Imports {
					imported, err := strconv.Unquote(spec.Path.Value)
					if err != nil {
						t.Fatalf("%s: %v", path, err)
					}
					if strings.Contains(imported, pair.forbidden) {
						t.Errorf("%s imports %q: a consumer and provider that import each other "+
							"are welded into one binary, and the provider can never be replaced "+
							"or moved to another tier. Depend on the contract package instead.",
							path, imported)
					}
				}
			}
		}
	}
}
