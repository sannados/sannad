package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sannados/sannad/internal/platform/auth"
)

// TestLockAccountBlocksFutureLogin: the ordinary suspension path.
func TestLockAccountBlocksFutureLogin(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "alice@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	result, err := svc.Login(ctx, "acme", "alice@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := svc.LockAccount(tenantCtx("acme"), result.Subject); err != nil {
		t.Fatalf("lock: %v", err)
	}

	if _, err := svc.Login(ctx, "acme", "alice@example.com", "hunter2-hunter2-hunter2"); !errors.Is(err, auth.ErrAccountLocked) {
		t.Fatalf("expected ErrAccountLocked, got %v", err)
	}
}

// TestLockAccountInvalidatesAnAlreadyIssuedSession: the property that makes
// a lock an emergency measure rather than only a future-login block — a
// session issued before the lock must stop working immediately, not at its
// natural expiry.
func TestLockAccountInvalidatesAnAlreadyIssuedSession(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "bob@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	result, err := svc.Login(ctx, "acme", "bob@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// The token is genuine and unexpired at this point.
	if _, _, err := svc.ValidateForCaller(result.AccessToken); err != nil {
		t.Fatalf("setup: token should validate before the lock: %v", err)
	}

	if err := svc.LockAccount(tenantCtx("acme"), result.Subject); err != nil {
		t.Fatalf("lock: %v", err)
	}

	if _, _, err := svc.ValidateForCaller(result.AccessToken); !errors.Is(err, auth.ErrAccountLocked) {
		t.Fatalf("expected the pre-lock token to stop validating with ErrAccountLocked, got %v", err)
	}
}

// TestUnlockAccountRestoresLoginButNotOldSessions: unlocking undoes the
// login block. It does not restore what the lock invalidated — a new
// session is a new session.
func TestUnlockAccountRestoresLoginButNotOldSessions(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "carol@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	oldSession, err := svc.Login(ctx, "acme", "carol@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := svc.LockAccount(tenantCtx("acme"), oldSession.Subject); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := svc.UnlockAccount(tenantCtx("acme"), oldSession.Subject); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	newSession, err := svc.Login(ctx, "acme", "carol@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login after unlock: %v", err)
	}
	if _, _, err := svc.ValidateForCaller(newSession.AccessToken); err != nil {
		t.Fatalf("a fresh session after unlock should validate: %v", err)
	}
	if _, _, err := svc.ValidateForCaller(oldSession.AccessToken); err == nil {
		t.Fatal("the session from before the lock validated again after unlock")
	}
}

// TestRevokeAllSessionsInvalidatesWithoutLocking: "log out everywhere"
// without suspending the account — it can log back in immediately.
func TestRevokeAllSessionsInvalidatesWithoutLocking(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "dave@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	oldSession, err := svc.Login(ctx, "acme", "dave@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := svc.RevokeAllSessions(tenantCtx("acme"), oldSession.Subject); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, _, err := svc.ValidateForCaller(oldSession.AccessToken); err == nil {
		t.Fatal("the pre-revoke session still validated")
	}

	// Logging in again works — the account itself was never locked.
	if _, err := svc.Login(ctx, "acme", "dave@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("login after revoke-all should succeed: %v", err)
	}
}

// TestServiceAccountTokensAreUnaffectedByAccountValidation:
// ValidateForCaller must not reject a service account's session just
// because no Account row exists for its subject ID.
func TestServiceAccountTokensAreUnaffectedByAccountValidation(t *testing.T) {
	svc := newTestService(t)
	_, rawKey, err := svc.CreateServiceAccount(tenantCtx("acme"), "ci-runner")
	if err != nil {
		t.Fatalf("create service account: %v", err)
	}
	result, err := svc.LoginWithAPIKey(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("login with api key: %v", err)
	}
	if _, _, err := svc.ValidateForCaller(result.AccessToken); err != nil {
		t.Fatalf("a service account's own session should validate: %v", err)
	}
}

// TestLockAccountAcrossTenantsIsNotFound: an operator in one tenant cannot
// lock, unlock, or revoke sessions for an account in another tenant just by
// knowing its ID — the caller's own tenant (from ctx) bounds every admin
// operation, the same as every other cross-tenant lookup in this codebase.
func TestLockAccountAcrossTenantsIsNotFound(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if err := svc.Register(ctx, "acme", "erin@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	result, err := svc.Login(ctx, "acme", "erin@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := svc.LockAccount(tenantCtx("globex"), result.Subject); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a cross-tenant lock, got %v", err)
	}
	if err := svc.RevokeAllSessions(tenantCtx("globex"), result.Subject); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a cross-tenant revoke, got %v", err)
	}

	// The account was never touched: the original login still works and the
	// original session still validates.
	if _, err := svc.Login(ctx, "acme", "erin@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("login should still succeed, account was never locked: %v", err)
	}
	if _, _, err := svc.ValidateForCaller(result.AccessToken); err != nil {
		t.Fatalf("original session should still validate, was never revoked: %v", err)
	}
}
