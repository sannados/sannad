package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/getkayan/kayan/core/identity"
	"github.com/glebarez/sqlite"
	"github.com/sannados/sannad/internal/app/migrations"
	"gorm.io/gorm"
)

// kayanOIDCRepo is unexported, so these run in-package rather than
// alongside service_test.go and revocation_test.go's external auth_test
// package.

func oidcDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "oidc.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	if err := migrations.RunMigrations(sqlDB, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// TestConsumeOIDCStateIsSingleUse: the whole reason state moved server-side.
// A captured or replayed callback request must not succeed a second time.
func TestConsumeOIDCStateIsSingleUse(t *testing.T) {
	repo := &kayanOIDCRepo{db: oidcDB(t), provider: "test"}
	ctx := context.Background()

	if err := repo.StoreOIDCState(ctx, "state-1", "verifier-1", "nonce-1", 10*time.Minute); err != nil {
		t.Fatalf("store: %v", err)
	}

	verifier, nonce, err := repo.ConsumeOIDCState(ctx, "state-1")
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if verifier != "verifier-1" || nonce != "nonce-1" {
		t.Fatalf("got (%q, %q), want (verifier-1, nonce-1)", verifier, nonce)
	}

	if _, _, err := repo.ConsumeOIDCState(ctx, "state-1"); err == nil {
		t.Fatal("a second consume of the same state succeeded")
	}
}

// TestConsumeOIDCStateRejectsExpired: a state record outlives its intended
// window only long enough for ConsumeOIDCState to report it, never to
// succeed on it.
func TestConsumeOIDCStateRejectsExpired(t *testing.T) {
	repo := &kayanOIDCRepo{db: oidcDB(t), provider: "test"}
	ctx := context.Background()

	if err := repo.StoreOIDCState(ctx, "state-1", "verifier-1", "nonce-1", -time.Minute); err != nil {
		t.Fatalf("store: %v", err)
	}

	if _, _, err := repo.ConsumeOIDCState(ctx, "state-1"); err == nil {
		t.Fatal("an expired state was consumed successfully")
	}
}

// TestConsumeOIDCStateRejectsUnknown: a state nobody stored — forged, or
// simply never issued — is refused the same way an expired one is.
func TestConsumeOIDCStateRejectsUnknown(t *testing.T) {
	repo := &kayanOIDCRepo{db: oidcDB(t), provider: "test"}
	if _, _, err := repo.ConsumeOIDCState(context.Background(), "never-issued"); err == nil {
		t.Fatal("an unknown state was consumed successfully")
	}
}

// TestFindOrCreateByProviderSubCreatesThenReuses: first login for a subject
// creates an account; a second login for the same subject resolves back to
// the same account rather than creating a duplicate.
func TestFindOrCreateByProviderSubCreatesThenReuses(t *testing.T) {
	repo := &kayanOIDCRepo{db: oidcDB(t), provider: "google"}
	ctx := context.Background()
	factory := func() any { return &Account{} }
	traits := identity.JSON(`{"sub":"user-123","email":"alice@example.com"}`)

	first, err := repo.FindOrCreateByProviderSub(ctx, "user-123", traits, factory)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	firstAccount, ok := first.(*Account)
	if !ok {
		t.Fatalf("got %T, want *Account", first)
	}
	if firstAccount.Email != "alice@example.com" {
		t.Fatalf("email = %q, want alice@example.com", firstAccount.Email)
	}
	if firstAccount.TenantID != "" {
		t.Fatalf("a freshly created account already has a tenant: %q", firstAccount.TenantID)
	}

	second, err := repo.FindOrCreateByProviderSub(ctx, "user-123", traits, factory)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	secondAccount, ok := second.(*Account)
	if !ok {
		t.Fatalf("got %T, want *Account", second)
	}
	if secondAccount.ID != firstAccount.ID {
		t.Fatalf("second login created a different account: %s vs %s", secondAccount.ID, firstAccount.ID)
	}
}

// TestFindOrCreateByProviderSubIsScopedToItsProvider: the same subject value
// from two different providers must resolve to two different accounts —
// "sub" is only unique within one provider's namespace, never globally.
//
// The first account is stamped with a tenant between the two calls, the same
// as service.HandleOAuthCallback always does immediately after this method
// returns in the real flow: leaving both permanently untenanted would collide
// on the (tenant_id, email) unique index for a reason that has nothing to do
// with what this test checks, and is not a state the real call path reaches.
func TestFindOrCreateByProviderSubIsScopedToItsProvider(t *testing.T) {
	db := oidcDB(t)
	ctx := context.Background()
	factory := func() any { return &Account{} }
	traits := identity.JSON(`{"sub":"user-123","email":"same@example.com"}`)

	google := &kayanOIDCRepo{db: db, provider: "google"}
	okta := &kayanOIDCRepo{db: db, provider: "okta"}

	googleAccount, err := google.FindOrCreateByProviderSub(ctx, "user-123", traits, factory)
	if err != nil {
		t.Fatalf("google: %v", err)
	}
	if err := db.Model(&Account{}).Where("id = ?", googleAccount.(*Account).ID).
		Update("tenant_id", "acme").Error; err != nil {
		t.Fatalf("stamp tenant: %v", err)
	}

	oktaAccount, err := okta.FindOrCreateByProviderSub(ctx, "user-123", traits, factory)
	if err != nil {
		t.Fatalf("okta: %v", err)
	}

	if googleAccount.(*Account).ID == oktaAccount.(*Account).ID {
		t.Fatal("two different providers' identical sub values resolved to the same account")
	}
}
