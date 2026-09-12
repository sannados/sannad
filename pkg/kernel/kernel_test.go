package kernel_test

import (
	"context"
	"testing"

	"github.com/sannados/sannad/pkg/kernel"
	"github.com/sannados/sannad/pkg/modulekit"
)

var ref = modulekit.CapabilityRef{Name: "compat.echo", Version: "v1"}

type compatModule struct{}

func (compatModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID: "com.example.compat", Kind: "embedded", Storage: modulekit.StorageNone,
		Provides: []modulekit.CapabilityRef{ref},
	}
}
func (compatModule) Install(reg modulekit.Registrar) error {
	return modulekit.HandleTyped(reg, ref,
		func(_ context.Context, input string) (string, error) { return input, nil })
}
func (compatModule) Start(context.Context) error { return nil }
func (compatModule) Stop(context.Context) error  { return nil }

func TestCompatibilityKernelDelegatesToRealRuntime(t *testing.T) {
	host := kernel.New()
	if err := host.Add(compatModule{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := host.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop(context.Background())

	got, err := modulekit.CallTyped[string, string](t.Context(), host, ref, "ok")
	if err != nil || got != "ok" {
		t.Fatalf("Call = %q, %v", got, err)
	}
}
