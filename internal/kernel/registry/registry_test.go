package registry_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/pkg/modulekit"
)

// fakeModule is a test double that records Start and Stop calls into a shared
// calls slice as "<id>:start" or "<id>:stop".
type fakeModule struct {
	desc       modulekit.Descriptor
	installErr error
	startErr   error
	stopErr    error
	calls      *[]string
}

func (f *fakeModule) Descriptor() modulekit.Descriptor    { return f.desc }
func (f *fakeModule) Install(_ modulekit.Registrar) error { return f.installErr }
func (f *fakeModule) Start(_ context.Context) error {
	if f.calls != nil {
		*f.calls = append(*f.calls, f.desc.ID+":start")
	}
	return f.startErr
}
func (f *fakeModule) Stop(_ context.Context) error {
	if f.calls != nil {
		*f.calls = append(*f.calls, f.desc.ID+":stop")
	}
	return f.stopErr
}

func newFake(id string, calls *[]string) *fakeModule {
	return &fakeModule{
		desc:  modulekit.Descriptor{ID: id, Kind: "embedded", Storage: modulekit.StorageNone},
		calls: calls,
	}
}

func TestStartAll_RollbackOnFailure(t *testing.T) {
	reg := registry.New()
	calls := []string{}

	m1 := newFake("m1", &calls)
	m2 := &fakeModule{
		desc:     modulekit.Descriptor{ID: "m2", Kind: "embedded", Storage: modulekit.StorageNone},
		startErr: errors.New("start failed"),
		calls:    &calls,
	}

	if err := reg.RegisterModule(m1); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterModule(m2); err != nil {
		t.Fatal(err)
	}

	if err := reg.StartAll(context.Background()); err == nil {
		t.Fatal("expected StartAll to return an error")
	}

	// m1 started successfully, m2 failed → m1 must be stopped during rollback.
	want := []string{"m1:start", "m2:start", "m1:stop"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

func TestStopAll_ReverseOrder(t *testing.T) {
	reg := registry.New()
	calls := []string{}

	for _, id := range []string{"m1", "m2", "m3"} {
		if err := reg.RegisterModule(newFake(id, &calls)); err != nil {
			t.Fatal(err)
		}
	}

	if err := reg.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reg.StopAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"m1:start", "m2:start", "m3:start",
		"m3:stop", "m2:stop", "m1:stop",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

func TestRegisterModule_Duplicate(t *testing.T) {
	reg := registry.New()
	m := newFake("mod1", nil)

	if err := reg.RegisterModule(m); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterModule(m); !errors.Is(err, registry.ErrDuplicateModule) {
		t.Errorf("expected ErrDuplicateModule, got %v", err)
	}
}

func TestFailedInstallDoesNotLeaveCapabilityExposed(t *testing.T) {
	reg := registry.New()
	ref := modulekit.CapabilityRef{Name: "failed.public", Version: "v1"}
	module := &fakeModule{
		desc: modulekit.Descriptor{
			ID:       "com.example.failed",
			Kind:     "sandboxed",
			Storage:  modulekit.StorageNone,
			Provides: []modulekit.CapabilityRef{ref},
			Exposed:  []modulekit.CapabilityRef{ref},
		},
		installErr: errors.New("install failed"),
	}

	if err := reg.RegisterModule(module); err == nil {
		t.Fatal("failed module registration reported success")
	}
	if reg.IsExposed(ref) {
		t.Fatal("failed module left its capability publicly exposed")
	}
}

func TestStartAll_UnsatisfiedConsumes(t *testing.T) {
	reg := registry.New()

	consumer := &fakeModule{
		desc: modulekit.Descriptor{
			ID:      "consumer",
			Kind:    "embedded",
			Storage: modulekit.StorageNone,
			Consumes: []modulekit.CapabilityRef{
				{Name: "missing.capability", Version: "v1"},
			},
		},
	}
	if err := reg.RegisterModule(consumer); err != nil {
		t.Fatalf("RegisterModule: %v", err)
	}

	// StartAll must fail because the consumed capability is not registered.
	if err := reg.StartAll(context.Background()); err == nil {
		t.Fatal("expected StartAll to fail for unsatisfied Consumes declaration")
	}
}
