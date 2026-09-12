package bus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sannados/sannad/internal/kernel/bus"
	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/pkg/modulekit"
)

// stubModule registers a single echo capability for testing.
type stubModule struct {
	capRef modulekit.CapabilityRef
}

func (s *stubModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       "com.test.stub",
		Kind:     "embedded",
		Provides: []modulekit.CapabilityRef{s.capRef},
		Storage:  modulekit.StorageNone,
	}
}

func (s *stubModule) Install(reg modulekit.Registrar) error {
	return reg.RegisterCapability(s.capRef, func(_ context.Context, req any) (any, error) {
		return req, nil // echo the request back
	})
}

func (s *stubModule) Start(_ context.Context) error { return nil }
func (s *stubModule) Stop(_ context.Context) error  { return nil }

var echoRef = modulekit.CapabilityRef{Name: "test.echo", Version: "v1"}

func newTestBus(t *testing.T) *bus.Bus {
	t.Helper()
	reg := registry.New()
	m := &stubModule{capRef: echoRef}
	if err := reg.RegisterModule(m); err != nil {
		t.Fatalf("RegisterModule: %v", err)
	}
	if err := reg.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	return bus.New(reg)
}

func TestBus_Call_RoutesToHandler(t *testing.T) {
	b := newTestBus(t)
	payload := "hello"
	result, err := b.Call(context.Background(), echoRef, payload)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, ok := result.(string)
	if !ok || got != payload {
		t.Errorf("Call returned %v, want %q", result, payload)
	}
}

func TestBus_Call_UnknownCapabilityReturnsError(t *testing.T) {
	b := newTestBus(t)
	unknown := modulekit.CapabilityRef{Name: "does.not.exist", Version: "v1"}
	_, err := b.Call(context.Background(), unknown, nil)
	if err == nil {
		t.Fatal("expected error for unknown capability, got nil")
	}
}

func TestBus_Call_PropagatesHandlerError(t *testing.T) {
	reg := registry.New()
	sentinel := errors.New("handler failure")
	failing := &failingModule{capRef: modulekit.CapabilityRef{Name: "test.fail", Version: "v1"}, err: sentinel}
	if err := reg.RegisterModule(failing); err != nil {
		t.Fatalf("RegisterModule: %v", err)
	}
	if err := reg.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	b := bus.New(reg)
	_, err := b.Call(context.Background(), failing.capRef, nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error in chain, got: %v", err)
	}
}

type failingModule struct {
	capRef modulekit.CapabilityRef
	err    error
}

func (f *failingModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{ID: "com.test.failing", Kind: "embedded", Storage: modulekit.StorageNone, Provides: []modulekit.CapabilityRef{f.capRef}}
}

func (f *failingModule) Install(reg modulekit.Registrar) error {
	return reg.RegisterCapability(f.capRef, func(_ context.Context, _ any) (any, error) {
		return nil, f.err
	})
}

func (f *failingModule) Start(_ context.Context) error { return nil }
func (f *failingModule) Stop(_ context.Context) error  { return nil }
