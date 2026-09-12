package announcements_test

import (
	"context"
	"errors"
	"testing"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/examples/embedded/announcements"
	"github.com/sannados/sannad/internal/kernel/bus"
	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/pkg/modulekit"
)

// This module keeps its data outside the kernel's store, so nothing applies
// tenant isolation underneath it. These tests are that isolation — the module
// declared StorageOwn, and this is what making good on the declaration looks
// like.
//
// Both tests fail against the version this replaced, which read the tenant from
// the request and returned every tenant's data when the request named none.

func wire(t *testing.T) *bus.Bus {
	t.Helper()
	reg := registry.New()
	b := bus.New(reg)
	if err := reg.RegisterModule(announcements.New()); err != nil {
		t.Fatalf("register: %v", err)
	}
	return b
}

func list(t *testing.T, b *bus.Bus, ctx context.Context) (announcements.BoardReaderResponse, error) {
	t.Helper()
	result, err := b.Call(ctx, announcements.CapabilityBoardReader,
		announcements.BoardReaderRequest{})
	if err != nil {
		return announcements.BoardReaderResponse{}, err
	}
	resp, ok := result.(announcements.BoardReaderResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", result)
	}
	return resp, nil
}

// TestReadsOnlyTheContextTenant is the isolation property. The seed data spans
// two tenants, so a module that filtered wrongly returns more than one tenant's
// announcements.
func TestReadsOnlyTheContextTenant(t *testing.T) {
	b := wire(t)

	resp, err := list(t, b, tenant.WithTenantID(context.Background(), "demo"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Announcements) == 0 {
		t.Fatal("the demo tenant sees nothing; the fixture or the filter is wrong")
	}
	for _, announcement := range resp.Announcements {
		if announcement.TenantID != "demo" {
			t.Fatalf("a demo caller read tenant %q's announcement %q",
				announcement.TenantID, announcement.ID)
		}
	}
}

// TestCannotReadAnotherTenant: the seeded "school" data must be unreachable from
// a "demo" context. The version this replaced let a caller name any tenant in
// the request, which made exactly this reachable.
func TestCannotReadAnotherTenant(t *testing.T) {
	b := wire(t)

	demo, err := list(t, b, tenant.WithTenantID(context.Background(), "demo"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, announcement := range demo.Announcements {
		if announcement.TenantID == "school" {
			t.Fatal("a demo caller read a school announcement")
		}
	}

	// And the other direction, so the test is not passing because "school" data
	// is simply absent.
	school, err := list(t, b, tenant.WithTenantID(context.Background(), "school"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(school.Announcements) == 0 {
		t.Fatal("the school tenant sees nothing; the test above proves nothing")
	}
	for _, announcement := range school.Announcements {
		if announcement.TenantID != "school" {
			t.Fatalf("a school caller read tenant %q's data", announcement.TenantID)
		}
	}
}

// TestMissingTenantIsRefused. Failing closed is the point: the version this
// replaced treated an absent tenant as "no filter" and returned every tenant's
// announcements, which is the worst possible reading of missing context.
func TestMissingTenantIsRefused(t *testing.T) {
	b := wire(t)

	_, err := list(t, b, context.Background())
	if err == nil {
		t.Fatal("a call with no tenant in context returned data")
	}
	if !errors.Is(err, modulekit.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

// TestDeclaresSelfManagedStorage: the declaration is what tells an operator this
// module's isolation is a claim rather than a mechanism, and what makes it show
// up in the startup report. StorageNone would be wrong — this module holds
// per-tenant data, just not in the kernel's store.
func TestDeclaresSelfManagedStorage(t *testing.T) {
	descriptor := announcements.New().Descriptor()
	if descriptor.Storage != modulekit.StorageOwn {
		t.Fatalf("Storage = %q, want %q: this module holds tenant data outside the kernel store",
			descriptor.Storage, modulekit.StorageOwn)
	}
}

// TestRequestCarriesNoTenantSelector guards the shape of the fix. Restoring a
// tenant field to the request would reintroduce the vulnerability, and every
// behavioural test above would still pass as long as the handler ignored it —
// until someone wired it back up.
func TestRequestCarriesNoTenantSelector(t *testing.T) {
	b := wire(t)

	// A request naming another tenant must change nothing.
	result, err := b.Call(tenant.WithTenantID(context.Background(), "demo"),
		announcements.CapabilityBoardReader,
		announcements.BoardReaderRequest{TenantID: "school"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	resp := result.(announcements.BoardReaderResponse)
	for _, announcement := range resp.Announcements {
		if announcement.TenantID != "demo" {
			t.Fatalf("a request naming tenant %q was honoured; the tenant must come "+
				"from context alone", announcement.TenantID)
		}
	}
}
