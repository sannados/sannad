package auth_test

import (
	"testing"
	"time"

	"github.com/sannados/sannad/internal/platform/auth"
)

// TestSecretRotationAcceptsATokenSignedUnderThePreviousSecret: the whole
// point of carrying a previous secret — a token minted before a rotation
// must keep validating against the strategy configured with the new one.
func TestSecretRotationAcceptsATokenSignedUnderThePreviousSecret(t *testing.T) {
	oldStrategy := auth.NewTenantJWTStrategy("old-secret-value-long-enough-32", time.Hour, 24*time.Hour)
	session, err := oldStrategy.CreateForTenant("sess-1", "user-1", "tenant-a")
	if err != nil {
		t.Fatalf("create under old secret: %v", err)
	}

	rotated := auth.NewTenantJWTStrategy("new-secret-value-long-enough-32", time.Hour, 24*time.Hour,
		"old-secret-value-long-enough-32")

	if _, err := rotated.Validate(t.Context(), session.ID); err != nil {
		t.Fatalf("token signed under the retired secret should still validate: %v", err)
	}
}

// TestSecretRotationSignsNewTokensWithTheActiveSecretOnly: a token issued
// after rotation must not depend on the retired secret at all — dropping it
// later should not retroactively invalidate anything minted post-rotation.
func TestSecretRotationSignsNewTokensWithTheActiveSecretOnly(t *testing.T) {
	rotated := auth.NewTenantJWTStrategy("new-secret-value-long-enough-32", time.Hour, 24*time.Hour,
		"old-secret-value-long-enough-32")
	session, err := rotated.CreateForTenant("sess-2", "user-1", "tenant-a")
	if err != nil {
		t.Fatalf("create under rotated strategy: %v", err)
	}

	// A strategy that knows only the new secret (the retired one already
	// dropped from config) must still validate a token minted after rotation.
	newOnly := auth.NewTenantJWTStrategy("new-secret-value-long-enough-32", time.Hour, 24*time.Hour)
	if _, err := newOnly.Validate(t.Context(), session.ID); err != nil {
		t.Fatalf("a post-rotation token should validate without the retired secret: %v", err)
	}
}

// TestSecretRotationRejectsATokenSignedUnderAnUnlistedSecret: only secrets
// this deployment actually configured are trusted — a signature from any
// other key, retired or not, must fail.
func TestSecretRotationRejectsATokenSignedUnderAnUnlistedSecret(t *testing.T) {
	forger := auth.NewTenantJWTStrategy("forged-secret-value-long-enough32", time.Hour, 24*time.Hour)
	session, err := forger.CreateForTenant("sess-3", "user-1", "tenant-a")
	if err != nil {
		t.Fatalf("create under forger secret: %v", err)
	}

	rotated := auth.NewTenantJWTStrategy("new-secret-value-long-enough-32", time.Hour, 24*time.Hour,
		"old-secret-value-long-enough-32")
	if _, err := rotated.Validate(t.Context(), session.ID); err == nil {
		t.Fatal("a token signed under an unconfigured secret validated")
	}
}
