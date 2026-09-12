package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/internal/platform/auth"
)

func tenantCtx(id string) context.Context {
	return tenant.WithTenantID(context.Background(), id)
}

// TestCreateServiceAccountReturnsARandomKeyOnce: the whole point of the
// return-once contract — a caller-chosen or re-readable key would defeat it.
func TestCreateServiceAccountReturnsARandomKeyOnce(t *testing.T) {
	svc := newTestService(t)

	first, firstKey, err := svc.CreateServiceAccount(tenantCtx("acme"), "ci-runner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if firstKey == "" {
		t.Fatal("no key returned")
	}
	if first.TenantID != "acme" {
		t.Fatalf("tenant = %q, want acme", first.TenantID)
	}

	second, secondKey, err := svc.CreateServiceAccount(tenantCtx("acme"), "deploy-bot")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if secondKey == firstKey {
		t.Fatal("two service accounts got the same key")
	}
	if second.ID == first.ID {
		t.Fatal("two service accounts got the same ID")
	}
}

// TestLoginWithAPIKeyIssuesATenantBoundSession: the central claim — a
// service account authenticates like any account and gets the same
// signed, tenant-bound session, so nothing downstream needs a second
// tenant-propagation path.
func TestLoginWithAPIKeyIssuesATenantBoundSession(t *testing.T) {
	svc := newTestService(t)

	account, rawKey, err := svc.CreateServiceAccount(tenantCtx("acme"), "ci-runner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	result, err := svc.LoginWithAPIKey(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if result.TenantID != "acme" {
		t.Fatalf("session tenant = %q, want acme", result.TenantID)
	}
	if result.Subject != account.ID {
		t.Fatalf("session subject = %q, want %q", result.Subject, account.ID)
	}

	subject, tenantID, err := svc.ValidateForCaller(result.AccessToken)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if subject != account.ID || tenantID != "acme" {
		t.Fatalf("validated (%q, %q), want (%q, acme)", subject, tenantID, account.ID)
	}
}

// TestLoginWithAPIKeyRejectsAWrongKey: an unrecognized key must not
// authenticate as anything.
func TestLoginWithAPIKeyRejectsAWrongKey(t *testing.T) {
	svc := newTestService(t)
	if _, _, err := svc.CreateServiceAccount(tenantCtx("acme"), "ci-runner"); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.LoginWithAPIKey(context.Background(), "whsec_not-a-real-key"); err == nil {
		t.Fatal("a wrong key authenticated successfully")
	}
}

// TestRevokedServiceAccountCannotLogin: revocation is the table-stakes
// response to a leaked key, the same as webhook secret rotation.
func TestRevokedServiceAccountCannotLogin(t *testing.T) {
	svc := newTestService(t)
	account, rawKey, err := svc.CreateServiceAccount(tenantCtx("acme"), "ci-runner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := svc.RevokeServiceAccount(tenantCtx("acme"), account.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := svc.LoginWithAPIKey(context.Background(), rawKey); err == nil {
		t.Fatal("a revoked service account's key still authenticated")
	}
}

// TestListServiceAccountsIsTenantScoped: an authenticated caller managing
// service accounts must never see another tenant's.
func TestListServiceAccountsIsTenantScoped(t *testing.T) {
	svc := newTestService(t)
	if _, _, err := svc.CreateServiceAccount(tenantCtx("acme"), "acme-bot"); err != nil {
		t.Fatalf("create acme: %v", err)
	}
	if _, _, err := svc.CreateServiceAccount(tenantCtx("globex"), "globex-bot"); err != nil {
		t.Fatalf("create globex: %v", err)
	}

	accounts, err := svc.ListServiceAccounts(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 || accounts[0].Name != "acme-bot" {
		t.Fatalf("acme saw %+v, want only its own service account", accounts)
	}
}

// TestRevokeServiceAccountAcrossTenantsIsNotFound: revoking another
// tenant's service account by ID must fail the same way a nonexistent ID
// does — a distinguishable error would leak whether the ID exists at all.
func TestRevokeServiceAccountAcrossTenantsIsNotFound(t *testing.T) {
	svc := newTestService(t)
	account, _, err := svc.CreateServiceAccount(tenantCtx("acme"), "acme-bot")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	err = svc.RevokeServiceAccount(tenantCtx("globex"), account.ID)
	if !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("expected ErrNotFound revoking another tenant's service account, got %v", err)
	}
}

// TestCreateServiceAccountRequiresATenant: a service account with no owning
// tenant would be unmanageable and unrevokable by anyone.
func TestCreateServiceAccountRequiresATenant(t *testing.T) {
	svc := newTestService(t)
	if _, _, err := svc.CreateServiceAccount(context.Background(), "ci-runner"); err == nil {
		t.Fatal("a service account was created with no tenant in context")
	}
}
