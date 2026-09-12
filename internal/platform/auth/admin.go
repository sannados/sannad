package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/getkayan/kayan/core/audit"
	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

// Admin operations on a single account. Before this, "suspend" existed only
// at the tenant level (tenancy.Directory) — an operator could disable every
// account in a tenant but not one of them, and nothing could invalidate a
// token already issued short of waiting for it to expire. Both gaps close
// here, using storage already added for this feature (Account.Active,
// Account.TokenVersion) rather than a session store.

// ErrAccountLocked is returned by Login, HandleOAuthCallback, and
// ValidateForCaller when the account exists, the credential or token is
// genuine, and the account is locked anyway.
var ErrAccountLocked = errors.New("auth: account is locked")

// LockAccount blocks future logins for accountID within the tenant in ctx
// and invalidates every session already issued to it, immediately — not
// only new ones. Locking without also revoking existing sessions would
// leave someone already logged in unaffected until their token's natural
// expiry, which defeats the point of an emergency lock.
func (service *Service) LockAccount(ctx context.Context, accountID string) error {
	return service.setAccountLock(ctx, accountID, false, "account.locked", audit.EventUserSuspended)
}

// UnlockAccount restores an account's ability to log in. It does not restore
// its old sessions — those were invalidated by the lock and stay that way;
// the account logs in again and gets new ones.
func (service *Service) UnlockAccount(ctx context.Context, accountID string) error {
	return service.setAccountLock(ctx, accountID, true, "account.unlocked", audit.EventUserActivated)
}

func (service *Service) setAccountLock(ctx context.Context, accountID string, active bool, message, eventType string) error {
	tenantID, err := service.tenantOfAccount(ctx, accountID)
	if err != nil {
		return err
	}

	changes := map[string]any{"active": active}
	if !active {
		// The version bump is what makes the lock immediate: every token
		// already issued carries the account's *old* TokenVersion and stops
		// matching the instant this write commits, regardless of when those
		// tokens would otherwise have expired.
		changes["token_version"] = gorm.Expr("token_version + 1")
	}
	result := service.db.WithContext(ctx).Model(&Account{}).
		Where("id = ? AND tenant_id = ?", accountID, tenantID).
		Updates(changes)
	if result.Error != nil {
		return fmt.Errorf("auth: set account %s active=%v: %w", accountID, active, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("auth: account %s: %w", accountID, ErrNotFound)
	}

	caller, _ := modulekit.CallerFrom(ctx)
	service.logAudit(ctx, eventType, tenantID, caller.Subject, accountID, true, message)
	return nil
}

// RevokeAllSessions invalidates every session accountID currently holds,
// without locking the account — the account may log in again immediately
// and get a fresh session. Use this for "log out everywhere" (a user's own
// request, or an operator responding to a suspected compromise without
// wanting to lock the account outright); use LockAccount when the account
// itself should stop being usable.
func (service *Service) RevokeAllSessions(ctx context.Context, accountID string) error {
	tenantID, err := service.tenantOfAccount(ctx, accountID)
	if err != nil {
		return err
	}

	result := service.db.WithContext(ctx).Model(&Account{}).
		Where("id = ? AND tenant_id = ?", accountID, tenantID).
		Update("token_version", gorm.Expr("token_version + 1"))
	if result.Error != nil {
		return fmt.Errorf("auth: revoke sessions for %s: %w", accountID, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("auth: account %s: %w", accountID, ErrNotFound)
	}

	caller, _ := modulekit.CallerFrom(ctx)
	service.logAudit(ctx, audit.EventSessionRevoked, tenantID, caller.Subject, accountID, true,
		"all sessions revoked")
	return nil
}

// tenantOfAccount returns the caller's own tenant from ctx, after confirming
// accountID actually belongs to it. Deliberately not the account's own
// stored tenant read unconditionally: an admin operation is scoped to the
// tenant the caller was authorized in, not to wherever the target ID happens
// to live — otherwise any tenant's "owner" could lock, unlock, or revoke the
// sessions of an account in a tenant it has no relationship to, by ID alone.
// A cross-tenant ID is reported as ErrNotFound, indistinguishable from one
// that never existed, the same shape every other cross-tenant lookup in this
// codebase uses.
func (service *Service) tenantOfAccount(ctx context.Context, accountID string) (string, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return "", ErrNoTenant
	}
	var account Account
	err := service.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ?", accountID, tenantID).
		First(&account).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", fmt.Errorf("auth: account %s: %w", accountID, ErrNotFound)
		}
		return "", fmt.Errorf("auth: look up account %s: %w", accountID, err)
	}
	return tenantID, nil
}
