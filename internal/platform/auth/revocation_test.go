package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/sannados/sannad/internal/app/migrations"
	"github.com/sannados/sannad/internal/platform/auth"
	"gorm.io/gorm"
)

// The revocation list is what makes logout mean something. Before it, Delete
// returned nil and the token kept working until it expired.

func revocationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "revocation.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	// The real migration, not AutoMigrate: the primary key is what makes a
	// repeated revoke an update rather than a duplicate row.
	if err := migrations.RunMigrations(sqlDB, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func newStrategy(t *testing.T, db *gorm.DB) (*auth.TenantJWTStrategy, *auth.RevocationStore) {
	t.Helper()
	store := auth.NewRevocationStore(db)
	strategy := auth.NewTenantJWTStrategy("test-secret-value", time.Hour, 24*time.Hour).
		WithRevocation(store)
	return strategy, store
}

// TestRevokedTokenStopsValidating is the property the whole feature exists for.
// The token is still correctly signed and unexpired; it must be refused anyway.
func TestRevokedTokenStopsValidating(t *testing.T) {
	db := revocationDB(t)
	strategy, _ := newStrategy(t, db)

	session, err := strategy.CreateForTenant("sess-1", "user-1", "tenant-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, err := strategy.ValidateWithTenant(session.ID); err != nil {
		t.Fatalf("token should validate before revocation: %v", err)
	}

	if err := strategy.Delete(context.Background(), session.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, _, err = strategy.ValidateWithTenant(session.ID)
	if !errors.Is(err, auth.ErrSessionRevoked) {
		t.Fatalf("revoked token still validates: err=%v", err)
	}
}

// TestRevocationKillsTheRefreshChain: Refresh reuses the session ID, so a
// logout that left refresh working would let the client mint a fresh pair and
// undo its own logout.
func TestRevocationKillsTheRefreshChain(t *testing.T) {
	db := revocationDB(t)
	strategy, _ := newStrategy(t, db)

	session, err := strategy.CreateForTenant("sess-2", "user-1", "tenant-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	refreshToken := session.RefreshToken
	if refreshToken == "" {
		t.Skip("strategy issues no refresh token")
	}

	if err := strategy.Delete(context.Background(), session.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := strategy.Refresh(context.Background(), refreshToken); err == nil {
		t.Fatal("a revoked session still refreshed; logout can be undone by the client")
	}
}

// TestUnrelatedSessionsAreUnaffected: revocation must be surgical, or a single
// logout becomes a fleet-wide outage.
func TestUnrelatedSessionsAreUnaffected(t *testing.T) {
	db := revocationDB(t)
	strategy, _ := newStrategy(t, db)

	first, err := strategy.CreateForTenant("sess-a", "user-1", "tenant-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := strategy.CreateForTenant("sess-b", "user-2", "tenant-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := strategy.Delete(context.Background(), first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, _, err := strategy.ValidateWithTenant(second.ID); err != nil {
		t.Fatalf("an unrelated session was revoked: %v", err)
	}
}

// TestDeleteWithoutAStoreReportsRatherThanLies. A logout that silently does
// nothing is worse than one that says it cannot: the operator believes they
// have revocation and they do not.
func TestDeleteWithoutAStoreReportsRatherThanLies(t *testing.T) {
	strategy := auth.NewTenantJWTStrategy("test-secret-value", time.Hour, 24*time.Hour)
	session, err := strategy.CreateForTenant("sess-3", "user-1", "tenant-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	err = strategy.Delete(context.Background(), session.ID)
	if !errors.Is(err, auth.ErrRevocationUnavailable) {
		t.Fatalf("expected ErrRevocationUnavailable, got %v", err)
	}
}

// TestRepeatedRevokeSucceeds: logout is retried by clients, and a second logout
// is not a failure.
func TestRepeatedRevokeSucceeds(t *testing.T) {
	db := revocationDB(t)
	strategy, _ := newStrategy(t, db)

	session, err := strategy.CreateForTenant("sess-4", "user-1", "tenant-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := strategy.Delete(context.Background(), session.ID); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := strategy.Delete(context.Background(), session.ID); err != nil {
		t.Fatalf("second delete should succeed: %v", err)
	}

	var count int64
	if err := db.Model(&auth.RevokedSession{}).
		Where("session_id = ?", "sess-4").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("repeated revoke wrote %d rows, want 1", count)
	}
}

// TestPurgeRemovesOnlyExpiredEntries. Without a purge the table grows forever;
// purging too eagerly un-revokes a live session, which is the dangerous
// direction.
func TestPurgeRemovesOnlyExpiredEntries(t *testing.T) {
	db := revocationDB(t)
	store := auth.NewRevocationStore(db)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	if err := store.Revoke(ctx, "old-session", "tenant-a", past, auth.ReasonLogout); err != nil {
		t.Fatalf("revoke old: %v", err)
	}
	if err := store.Revoke(ctx, "live-session", "tenant-a", future, auth.ReasonLogout); err != nil {
		t.Fatalf("revoke live: %v", err)
	}

	removed, err := store.PurgeExpired(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 1 {
		t.Fatalf("purged %d entries, want 1", removed)
	}

	// The still-live revocation must survive, or the purge silently restores
	// sessions somebody revoked.
	revoked, err := store.IsRevoked(ctx, "live-session")
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatal("purge un-revoked a session whose token has not expired")
	}
}

// TestPurgeClearsTheCache: an entry removed from the table but left in the
// cache would keep reporting a revocation that no longer exists.
func TestPurgeClearsTheCache(t *testing.T) {
	db := revocationDB(t)
	store := auth.NewRevocationStore(db)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	if err := store.Revoke(ctx, "old-session", "tenant-a", past, auth.ReasonLogout); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Warm the cache with the revoked answer.
	if revoked, err := store.IsRevoked(ctx, "old-session"); err != nil || !revoked {
		t.Fatalf("expected revoked, got %v (%v)", revoked, err)
	}

	if _, err := store.PurgeExpired(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}

	revoked, err := store.IsRevoked(ctx, "old-session")
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if revoked {
		t.Fatal("cache still reports a revocation the purge removed")
	}
}

// TestRevocationIsVisibleImmediatelyInTheRevokingProcess. A user who clicks
// logout and is still logged in has not experienced a logout, so the TTL must
// not apply to the process that performed the revocation.
func TestRevocationIsVisibleImmediately(t *testing.T) {
	db := revocationDB(t)
	store := auth.NewRevocationStore(db)
	ctx := context.Background()

	// Warm the cache with a negative answer first, which is the case that would
	// otherwise be served stale.
	if revoked, err := store.IsRevoked(ctx, "sess-5"); err != nil || revoked {
		t.Fatalf("expected not revoked, got %v (%v)", revoked, err)
	}

	if err := store.Revoke(ctx, "sess-5", "tenant-a", time.Now().Add(time.Hour), auth.ReasonLogout); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	revoked, err := store.IsRevoked(ctx, "sess-5")
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatal("a just-revoked session was still reported live from cache")
	}
}

// TestEmptySessionIDIsRefused: a token with no session ID could never be
// revoked individually, so it must not be treated as simply "not revoked".
func TestEmptySessionIDIsRefused(t *testing.T) {
	db := revocationDB(t)
	store := auth.NewRevocationStore(db)
	ctx := context.Background()

	if _, err := store.IsRevoked(ctx, ""); err == nil {
		t.Error("an empty session ID was accepted as not revoked")
	}
	if err := store.Revoke(ctx, "", "tenant-a", time.Now().Add(time.Hour), auth.ReasonLogout); err == nil {
		t.Error("revoking an empty session ID succeeded")
	}
}

// TestDatabaseErrorFailsClosed is the most dangerous failure mode this design
// has. If an unreachable database were reported as "not revoked", a database
// outage would silently restore every revoked session at once — turning the
// one component meant to contain a compromise into the thing that ends the
// containment.
//
// The check must therefore report the error and let the caller refuse the
// request, rather than answering optimistically.
func TestDatabaseErrorFailsClosed(t *testing.T) {
	db := revocationDB(t)
	store := auth.NewRevocationStore(db)
	ctx := context.Background()

	// Close the pool: every subsequent query fails, standing in for an
	// unreachable database.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	revoked, err := store.IsRevoked(ctx, "some-session")
	if err == nil {
		t.Fatal("an unreachable database was reported as a definitive 'not revoked'")
	}
	if revoked {
		t.Error("a failed check should not claim the session is revoked either")
	}
}

// TestValidationFailsWhenRevocationCannotBeChecked carries that guarantee up to
// the strategy: a token must not be accepted on the strength of a check that
// did not happen.
func TestValidationFailsWhenRevocationCannotBeChecked(t *testing.T) {
	db := revocationDB(t)
	strategy, _ := newStrategy(t, db)

	session, err := strategy.CreateForTenant("sess-db-down", "user-1", "tenant-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, _, err := strategy.ValidateWithTenant(session.ID); err == nil {
		t.Fatal("a token was accepted although revocation could not be checked")
	}
}
