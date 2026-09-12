package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/getkayan/kayan/core/audit"
	"github.com/getkayan/kayan/core/flow"
	"github.com/getkayan/kayan/core/tenant"
	"github.com/google/uuid"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

// Service identity, deferred in ADR 0005: HeaderResolver-based
// service-to-service tenancy was rejected there because it is only safe
// behind a gateway that overwrites the header from an authenticated source,
// and none existed. A service account is that authenticated source — it logs
// in like any account and receives the same signed, tenant-bound session a
// human does, so nothing downstream needs a second notion of "who is
// calling" or a second tenant-propagation path to get right.
//
// Kayan v0.3.0's flow.APIKeyStrategy is what makes this a few dozen lines
// instead of a hand-rolled credential scheme: hashing, constant-time
// comparison, and the strategy contract are Kayan's; this file supplies only
// the storage adapter and the tenant-stamping wiring, the same shape as the
// password and OIDC strategies already wired into this service.

// ServiceAccount is a machine identity: something that authenticates itself,
// as opposed to Account, which authenticates a human. Both act for exactly
// one tenant.
//
// ServiceAccount deliberately does NOT implement tenant.TenantAware, for the
// identical reason Account does not (see Account's own doc comment):
// authenticating by API key means looking a row up by its key hash before
// any tenant context exists, and a scoped lookup would fail that closed —
// nobody could ever authenticate. Isolation for service accounts is enforced
// explicitly in this file's own methods instead, the same as Account's.
type ServiceAccount struct {
	ID         string `gorm:"primaryKey"`
	TenantID   string `gorm:"index;not null"`
	Name       string
	APIKeyHash string `gorm:"uniqueIndex"`
	Active     bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (s *ServiceAccount) GetID() any   { return s.ID }
func (s *ServiceAccount) SetID(id any) { s.ID = id.(string) }

func (ServiceAccount) TableName() string { return "service_accounts" }

// serviceAccountKeyRepo implements flow.APIKeyRepository.
//
// Unlike accounts, a service account's tenant is known at creation — there is
// no untenanted window to compensate for the way OIDC and password
// registration have — so the lookup here can be a single scoped read rather
// than a two-step resolve-then-stamp.
type serviceAccountKeyRepo struct {
	db *gorm.DB
}

func (r *serviceAccountKeyRepo) FindIdentityByAPIKeyHash(ctx context.Context, keyHash string, factory func() any) (any, error) {
	account, ok := factory().(*ServiceAccount)
	if !ok {
		return nil, ErrInvalidIdentityType
	}
	err := r.db.WithContext(ctx).
		Where("api_key_hash = ? AND active = ?", keyHash, true).
		First(account).Error
	if err != nil {
		return nil, err
	}
	return account, nil
}

// CreateServiceAccount registers a new service account for the tenant in
// ctx and returns it alongside its raw API key. The key is returned exactly
// once, on creation — it is not otherwise retrievable, the same rule webhook
// endpoint secrets and OAuth client secrets already follow: a value that can
// be re-read is a value an attacker who later reaches the datastore can read
// too, not only whoever had it at issuance.
//
// The tenant comes from ctx, the same as every other authenticated write in
// this codebase — there is no tenant parameter for a caller to supply
// instead, because ServiceAccount is not isolation-enforced by the database
// layer (see its own doc comment) and a caller-supplied tenant here would be
// the one place that absence became exploitable.
func (service *Service) CreateServiceAccount(ctx context.Context, name string) (ServiceAccount, string, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return ServiceAccount{}, "", ErrNoTenant
	}
	if name == "" {
		return ServiceAccount{}, "", fmt.Errorf("auth: service account name is required")
	}

	rawKey, keyHash, err := flow.GenerateAPIKey(32)
	if err != nil {
		return ServiceAccount{}, "", fmt.Errorf("auth: generate api key: %w", err)
	}

	now := time.Now()
	account := ServiceAccount{
		ID:         uuid.NewString(),
		TenantID:   tenantID,
		Name:       name,
		APIKeyHash: keyHash,
		Active:     true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := service.db.WithContext(ctx).Create(&account).Error; err != nil {
		return ServiceAccount{}, "", fmt.Errorf("auth: create service account: %w", err)
	}
	caller, _ := modulekit.CallerFrom(ctx)
	service.logAudit(ctx, "service_account.created", tenantID, caller.Subject, account.ID, true,
		fmt.Sprintf("service account %q created", name))
	return account, rawKey, nil
}

// ListServiceAccounts returns every service account for the tenant in ctx.
// Keys are never included — there is nothing to redact, since only the hash
// is ever stored.
//
// The tenant filter is explicit here, in application code, rather than
// applied by an isolation callback — see ServiceAccount's own doc comment
// for why it cannot be a scoped model.
func (service *Service) ListServiceAccounts(ctx context.Context) ([]ServiceAccount, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, ErrNoTenant
	}
	var accounts []ServiceAccount
	if err := service.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Find(&accounts).Error; err != nil {
		return nil, fmt.Errorf("auth: list service accounts: %w", err)
	}
	return accounts, nil
}

// RevokeServiceAccount deactivates a service account owned by the tenant in
// ctx. Deactivating rather than deleting preserves what it did while it was
// live, the same choice already made for webhook endpoints and dead
// deliveries.
//
// Scoped explicitly by tenant_id in the WHERE clause, not by an isolation
// callback — an ID belonging to another tenant matches zero rows here rather
// than being reachable and merely refused, the same "not found, not merely
// forbidden" shape GetEndpoint gives a cross-tenant webhook read.
func (service *Service) RevokeServiceAccount(ctx context.Context, id string) error {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return ErrNoTenant
	}
	result := service.db.WithContext(ctx).Model(&ServiceAccount{}).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		Updates(map[string]any{"active": false, "updated_at": time.Now()})
	if result.Error != nil {
		return fmt.Errorf("auth: revoke service account %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("auth: service account %s: %w", id, ErrNotFound)
	}
	caller, _ := modulekit.CallerFrom(ctx)
	service.logAudit(ctx, "service_account.revoked", tenantID, caller.Subject, id, true, "service account revoked")
	return nil
}

// ErrNotFound is returned when a service account lookup or write addresses
// an ID that does not exist within the caller's tenant.
var ErrNotFound = errors.New("auth: not found")

// LoginWithAPIKey authenticates rawKey and issues a session bound to the
// service account's own tenant.
//
// There is no tenant parameter to accept here, and deliberately so: unlike
// human login, where the caller names a tenant and the account's membership
// is checked against it, a service account's tenant is fixed at creation and
// is exactly what a valid key proves — accepting a second, caller-supplied
// tenant would be a second path to the same claim, and the two could
// disagree.
func (service *Service) LoginWithAPIKey(ctx context.Context, rawKey string) (LoginResult, error) {
	identityValue, err := service.login.Authenticate(ctx, "api_key", "", rawKey)
	if err != nil {
		service.logAudit(ctx, audit.EventLoginFailure, "", "", "", false, "service account: invalid or revoked key")
		return LoginResult{}, fmt.Errorf("%w", ErrInvalidCredentials)
	}
	account, ok := identityValue.(*ServiceAccount)
	if !ok {
		return LoginResult{}, ErrInvalidIdentityType
	}

	sessionValue, err := service.strategy.CreateForTenant(uuid.NewString(), account.ID, account.TenantID)
	if err != nil {
		return LoginResult{}, err
	}
	service.logAudit(ctx, audit.EventLoginSuccess, account.TenantID, account.ID, account.ID, true, "service account login")
	return LoginResult{
		AccessToken:  sessionValue.ID,
		RefreshToken: sessionValue.RefreshToken,
		Subject:      account.ID,
		TenantID:     account.TenantID,
	}, nil
}
