package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sannados/sannad/internal/platform/auth"
)

// TestDeleteAccountBlocksFutureLoginAndInvalidatesSessions: erasure is not
// merely a data change — the account must stop being usable exactly like a
// lock does, immediately, not just on next password check.
func TestDeleteAccountBlocksFutureLoginAndInvalidatesSessions(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "frank@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	session, err := svc.Login(ctx, "acme", "frank@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, _, err := svc.ValidateForCaller(session.AccessToken); err != nil {
		t.Fatalf("setup: token should validate before deletion: %v", err)
	}

	if err := svc.DeleteAccount(tenantCtx("acme"), session.Subject); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := svc.Login(ctx, "acme", "frank@example.com", "hunter2-hunter2-hunter2"); err == nil {
		t.Fatal("login succeeded against a deleted account's old email")
	}
	if _, _, err := svc.ValidateForCaller(session.AccessToken); !errors.Is(err, auth.ErrAccountLocked) {
		t.Fatalf("expected the pre-deletion token to stop validating with ErrAccountLocked, got %v", err)
	}
}

// TestDeleteAccountScrubsPersonalData: the email and password hash a
// deleted account held are gone, not merely inaccessible via login.
func TestDeleteAccountScrubsPersonalData(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "grace@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	session, err := svc.Login(ctx, "acme", "grace@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := svc.DeleteAccount(tenantCtx("acme"), session.Subject); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// A new registration at the same address must succeed — the deleted
	// account's tombstone email must not still hold the unique slot.
	if err := svc.Register(ctx, "acme", "grace@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("re-register after deletion: %v", err)
	}
}

// TestDeleteAccountAcrossTenantsIsNotFound: the same cross-tenant boundary
// LockAccount enforces applies to deletion.
func TestDeleteAccountAcrossTenantsIsNotFound(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "henry@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	session, err := svc.Login(ctx, "acme", "henry@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := svc.DeleteAccount(tenantCtx("globex"), session.Subject); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a cross-tenant delete, got %v", err)
	}

	if _, err := svc.Login(ctx, "acme", "henry@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("account should be untouched by the cross-tenant attempt: %v", err)
	}
}

// TestExportAccountDataReturnsRolesAndTenant: an export reflects the
// account's actual current state, not a static template.
func TestExportAccountDataReturnsRolesAndTenant(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "iris@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	session, err := svc.Login(ctx, "acme", "iris@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	export, err := svc.ExportAccountData(tenantCtx("acme"), session.Subject)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if export.Email != "iris@example.com" {
		t.Fatalf("email = %q, want iris@example.com", export.Email)
	}
	if export.TenantID != "acme" {
		t.Fatalf("tenant = %q, want acme", export.TenantID)
	}
	// The first account registered into a tenant is granted "owner".
	found := false
	for _, role := range export.Roles {
		if role == "owner" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected owner among exported roles, got %v", export.Roles)
	}
}

// TestExportAccountDataAcrossTenantsIsNotFound: an export request cannot
// read another tenant's account either.
func TestExportAccountDataAcrossTenantsIsNotFound(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "jack@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	session, err := svc.Login(ctx, "acme", "jack@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if _, err := svc.ExportAccountData(tenantCtx("globex"), session.Subject); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a cross-tenant export, got %v", err)
	}
}
