package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/getkayan/kayan/core/audit"
	kayanidentity "github.com/getkayan/kayan/core/identity"
	"github.com/google/uuid"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

// DeleteAccount erases an account's personal data within the tenant in ctx.
//
// It anonymizes rather than removes the row, for the same reason a lock
// flips a flag instead of deleting anything: ValidateForCaller distinguishes
// "no such account" (a service account subject, exempt from these checks)
// from "an account, but blocked" purely by whether the row exists — see its
// own comment. Hard-deleting the row would make a still-unexpired token
// issued before the deletion take the "no such account" branch and validate
// with no Active or TokenVersion check at all, the opposite of what erasing
// someone's access is supposed to do. Anonymizing keeps the row (and the
// existing lock/revocation machinery) in place while clearing everything
// personal from it.
//
// Every other row that names this account by ID — role assignments, the
// identity-provider links a federated login created, audit events — is left
// alone. Audit events name the account only by opaque ID once its email is
// gone, which is what makes them safe to retain for a compliance trail
// after the data protection request that created them; role assignments and
// identity links carry no personal data themselves and are cleared so nothing
// still resolves against a "deleted" account.
func (service *Service) DeleteAccount(ctx context.Context, accountID string) error {
	tenantID, err := service.tenantOfAccount(ctx, accountID)
	if err != nil {
		return err
	}

	txErr := service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		tombstoneEmail := fmt.Sprintf("deleted-%s@deleted.invalid", uuid.NewString())
		result := tx.Model(&Account{}).
			Where("id = ? AND tenant_id = ?", accountID, tenantID).
			Updates(map[string]any{
				"email":         tombstoneEmail,
				"password_hash": dummyHash,
				"traits":        kayanidentity.JSON("{}"),
				"active":        false,
				"token_version": gorm.Expr("token_version + 1"),
			})
		if result.Error != nil {
			return fmt.Errorf("anonymize account: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Where("tenant_id = ? AND identity_id = ?", tenantID, accountID).
			Delete(&RoleAssignment{}).Error; err != nil {
			return fmt.Errorf("clear role assignments: %w", err)
		}
		if err := tx.Where("account_id = ?", accountID).Delete(&oidcIdentity{}).Error; err != nil {
			return fmt.Errorf("clear oidc identity links: %w", err)
		}
		if err := tx.Where("account_id = ?", accountID).Delete(&samlIdentity{}).Error; err != nil {
			return fmt.Errorf("clear saml identity links: %w", err)
		}
		return nil
	})
	if txErr != nil {
		if errors.Is(txErr, ErrNotFound) {
			return fmt.Errorf("auth: account %s: %w", accountID, ErrNotFound)
		}
		return fmt.Errorf("auth: delete account %s: %w", accountID, txErr)
	}

	caller, _ := modulekit.CallerFrom(ctx)
	service.logAudit(ctx, audit.EventUserDeleted, tenantID, caller.Subject, accountID, true,
		"account data erased")
	return nil
}

// AccountExport is the personal data this deployment holds on one account,
// returned in response to a data subject access request. It deliberately
// mirrors only what DeleteAccount actually clears: an export that promised
// more than the deletion path removes would misrepresent what "delete my
// data" accomplishes.
type AccountExport struct {
	AccountID  string    `json:"account_id"`
	TenantID   string    `json:"tenant_id"`
	Email      string    `json:"email"`
	Active     bool      `json:"active"`
	Roles      []string  `json:"roles"`
	ExportedAt time.Time `json:"exported_at"`
}

// ExportAccountData returns everything DeleteAccount would erase, for the
// data subject access request that typically precedes an erasure request.
func (service *Service) ExportAccountData(ctx context.Context, accountID string) (AccountExport, error) {
	tenantID, err := service.tenantOfAccount(ctx, accountID)
	if err != nil {
		return AccountExport{}, err
	}

	var account Account
	if err := service.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ?", accountID, tenantID).
		First(&account).Error; err != nil {
		return AccountExport{}, fmt.Errorf("auth: export account %s: %w", accountID, err)
	}

	roles, err := service.ListIdentityRoles(ctx, accountID)
	if err != nil {
		return AccountExport{}, fmt.Errorf("auth: export account %s roles: %w", accountID, err)
	}

	caller, _ := modulekit.CallerFrom(ctx)
	service.logAudit(ctx, audit.EventDataExported, tenantID, caller.Subject, accountID, true,
		"account data exported")

	return AccountExport{
		AccountID:  account.ID,
		TenantID:   account.TenantID,
		Email:      account.Email,
		Active:     account.Active,
		Roles:      roles,
		ExportedAt: time.Now(),
	}, nil
}
