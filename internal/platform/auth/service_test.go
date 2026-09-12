package auth_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/sannados/sannad/internal/app/migrations"
	"github.com/sannados/sannad/internal/platform/auth"
	"gorm.io/gorm"
)

// testSecret is 47 chars — satisfies the ≥32 char validation rule.
const testSecret = "auth-test-session-secret-long-enough-for-hs256"

func newTestService(t *testing.T) *auth.Service {
	t.Helper()
	// Note: bcrypt cost=12 makes individual login calls ~300ms. This is intentional.
	//
	// A file-backed database in the test's temp directory, not ":memory:".
	//
	// Registration runs inside a transaction, and a bare ":memory:" DSN gives
	// every pooled connection its own private database — the transaction would
	// open on a second connection that never saw the migrations. Shared-cache
	// in-memory fixes visibility but serializes every connection on one write
	// lock, which deadlocks that same transaction. A temp file has neither
	// problem and is removed with the test directory.
	dsn := filepath.Join(t.TempDir(), "auth-test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql.DB: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := migrations.RunMigrations(sqlDB, dsn); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	svc, err := auth.NewService(db, testSecret)
	if err != nil {
		t.Fatalf("new auth service: %v", err)
	}
	return svc
}

func TestRegister_Login_Validate(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "tenant-a", "alice@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	result, err := svc.Login(ctx, "tenant-a", "alice@example.com", "hunter2-hunter2-hunter2")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if result.AccessToken == "" {
		t.Fatal("expected non-empty access token")
	}
	if result.Subject == "" {
		t.Fatal("expected non-empty subject")
	}

	session, err := svc.Validate(result.AccessToken)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if session.IdentityID != result.Subject {
		t.Errorf("session.IdentityID = %q, want %q", session.IdentityID, result.Subject)
	}
	if result.TenantID != "tenant-a" {
		t.Errorf("result.TenantID = %q, want tenant-a", result.TenantID)
	}

	// The tenant must travel inside the signed token, not beside it.
	subject, tenantID, err := svc.ValidateForCaller(result.AccessToken)
	if err != nil {
		t.Fatalf("ValidateForCaller: %v", err)
	}
	if subject != result.Subject {
		t.Errorf("subject = %q, want %q", subject, result.Subject)
	}
	if tenantID != "tenant-a" {
		t.Errorf("tenant from token = %q, want tenant-a", tenantID)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "tenant-a", "bob@example.com", "correct-horse-battery-staple"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.Login(ctx, "tenant-a", "bob@example.com", "wrong-password"); err == nil {
		t.Fatal("expected Login to fail with wrong password")
	}
}

// TestLogin_WrongTenantIsRejected is the auth-layer half of the isolation
// guarantee: correct credentials against a tenant the account does not belong
// to must fail, and must fail the same way a wrong password does.
func TestLogin_WrongTenantIsRejected(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	const password = "correct-horse-battery-staple"
	if err := svc.Register(ctx, "tenant-a", "dave@example.com", password); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.Login(ctx, "tenant-b", "dave@example.com", password); err == nil {
		t.Fatal("expected login into a foreign tenant to fail")
	}
}

// TestRegister_SameEmailDifferentTenants confirms email uniqueness is scoped to
// the tenant. Two customers must be able to have a user at the same address.
func TestRegister_SameEmailDifferentTenants(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	const email = "shared@example.com"
	const password = "password-that-is-long-enough"

	if err := svc.Register(ctx, "tenant-a", email, password); err != nil {
		t.Fatalf("register in tenant-a: %v", err)
	}
	if err := svc.Register(ctx, "tenant-b", email, password); err != nil {
		t.Fatalf("register the same email in tenant-b: %v", err)
	}

	a, err := svc.Login(ctx, "tenant-a", email, password)
	if err != nil {
		t.Fatalf("login to tenant-a: %v", err)
	}
	b, err := svc.Login(ctx, "tenant-b", email, password)
	if err != nil {
		t.Fatalf("login to tenant-b: %v", err)
	}
	if a.Subject == b.Subject {
		t.Fatal("expected two distinct accounts for the same email in different tenants")
	}
	if a.TenantID == b.TenantID {
		t.Fatalf("expected distinct tenants, both were %q", a.TenantID)
	}
}

// TestRegister_RequiresTenant asserts an untenanted account cannot be created.
func TestRegister_RequiresTenant(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Register(context.Background(), "", "nobody@example.com", "password-long-enough"); err == nil {
		t.Fatal("expected registration without a tenant to fail")
	}
}

func TestRegister_Duplicate(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "tenant-a", "carol@example.com", "password-that-is-long-enough"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := svc.Register(ctx, "tenant-a", "carol@example.com", "password-that-is-long-enough"); err == nil {
		t.Fatal("expected second Register to fail with duplicate email")
	}
}
