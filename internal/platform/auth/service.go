package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/getkayan/kayan/core/audit"
	"github.com/getkayan/kayan/core/flow"
	kayanidentity "github.com/getkayan/kayan/core/identity"
	"github.com/getkayan/kayan/core/rbac"
	"github.com/getkayan/kayan/core/session"
	"github.com/getkayan/kayan/core/tenant"
	gormstore "github.com/getkayan/kayan/kayan-gorm"
	ksaml "github.com/getkayan/kayan/kayan-saml"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

var ErrInvalidIdentityType = errors.New("auth: unexpected identity type")
var ErrOIDCNotConfigured = errors.New("auth: OIDC is not configured")

// ErrNoTenant is returned when an auth operation is attempted without a tenant.
var ErrNoTenant = errors.New("auth: tenant is required")

// ErrAccountExists is returned when an email is already registered within the
// target tenant. The same address in a different tenant is not a conflict.
var ErrAccountExists = errors.New("auth: account already exists in this tenant")

// dummyHash is a valid bcrypt hash of a value no password will match. It gives
// the "no such account" path the same cost as a real comparison, so response
// time does not disclose whether an address is registered in a tenant.
const dummyHash = "$2a$12$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// ErrInvalidCredentials is returned for a failed login. It covers a wrong
// password and a valid password against a tenant the account does not belong
// to, deliberately without distinguishing them.
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

type Service struct {
	repository   *gormstore.Repository
	registration *flow.RegistrationManager
	login        *flow.LoginManager
	sessions     *session.Manager
	strategy     *TenantJWTStrategy
	revocations  *RevocationStore
	hasher       *flow.BcryptHasher
	db           *gorm.DB
	// oidc holds one strategy per configured provider, keyed by provider ID.
	// nil/empty when no providers are configured.
	oidc map[string]*flow.KayanOIDCStrategy
	// saml is nil when no enterprise SAML identity provider is configured.
	saml  *ksaml.ServiceProvider
	rbac  *rbac.Manager
	audit *audit.Logger
}

type LoginResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Subject      string `json:"subject"`
	TenantID     string `json:"tenant_id"`
}

// NewService builds the auth service, signing sessions with sessionSecret.
//
// previousSecrets are prior signing secrets, still accepted to verify a
// token issued before a rotation but never used to sign a new one — see
// NewTenantJWTStrategy for why they matter and how long to keep one listed.
func NewService(db *gorm.DB, sessionSecret string, previousSecrets ...string) (*Service, error) {
	repository := gormstore.NewRepository(db)

	factory := func() any { return &Account{} }
	registration := flow.NewRegistrationManager(repository, factory)
	login := flow.NewLoginManager(repository, factory)
	// BYOS mode: credentials are stored directly on the Account struct.
	// Field names must match Go struct field names (used via reflection).
	hasher := flow.NewBcryptHasher(12)
	passwordStrategy := flow.NewPasswordStrategy(repository, hasher, "Email", factory)
	passwordStrategy.MapFields([]string{"Email"}, "PasswordHash")
	passwordStrategy.SetIDGenerator(func() any { return uuid.NewString() })
	registration.RegisterStrategy(passwordStrategy)
	login.RegisterStrategy(passwordStrategy)

	// Machine-to-machine login. A service account has no registration flow —
	// it is created directly via CreateServiceAccount, never self-service —
	// so this strategy is only ever registered on login, never registration.
	// It brings its own factory: a service account is a different identity
	// type from Account, and each Kayan strategy owns the type it resolves.
	apiKeyStrategy := flow.NewAPIKeyStrategy(&serviceAccountKeyRepo{db: db}, func() any { return &ServiceAccount{} })
	login.RegisterStrategy(apiKeyStrategy)

	// Kayan's own HS256 strategy cannot carry a tenant claim: session.JWTClaims
	// is a closed struct and identity.Session has no tenant field. The local
	// strategy implements the same session.Strategy interface and adds it.
	strategy := NewTenantJWTStrategy(sessionSecret, 24*time.Hour, 7*24*time.Hour, previousSecrets...)
	// Revocation is wired here rather than left to the caller, so logout works
	// by default. An opt-in would mean a deployment that never called it had a
	// logout endpoint that silently did nothing — which is the state this
	// replaced.
	revocations := NewRevocationStore(db)
	strategy = strategy.WithRevocation(revocations)
	sessions := session.NewManager(strategy)

	// RoleAssignment and RoleDefinition are registered as scoped models by
	// bootstrap.New alongside every other tenant-scoped table this package
	// owns, so rbacRepo's queries are isolated the ordinary way rather than
	// by a manual predicate — see rbac.go's package comment for why this is
	// not kayan-gorm's own RBACRepository.
	rbacManager := rbac.NewManager(rbac.NewStorageStrategy(&rbacRepo{db: db}, &rbacRepo{db: db}))

	// repository already implements audit.AuditStore — gormstore.Repository
	// is the same identity repository this constructor builds everything
	// else on, so no second connection or table set is introduced for audit
	// storage. IDGenerator gives every event a stable ID even though this
	// package never sets one explicitly at the call site.
	auditLogger := audit.NewLogger(repository, audit.Hooks{IDGenerator: uuid.NewString})

	return &Service{
		repository:   repository,
		registration: registration,
		login:        login,
		sessions:     sessions,
		strategy:     strategy,
		revocations:  revocations,
		hasher:       hasher,
		db:           db,
		rbac:         rbacManager,
		audit:        auditLogger,
	}, nil
}

// InitOIDC wires OIDC social-login providers into the service. Call this once
// during bootstrap after NewService; it is a no-op when providers is empty.
//
// One flow.KayanOIDCStrategy per provider, each with its own discovered
// endpoints, its own state/PKCE/nonce repository, and its own ID token
// verifier scoped to that provider's issuer and this deployment's client ID
// for it. Discovery requires a live request to the provider, hence ctx.
func (service *Service) InitOIDC(ctx context.Context, providers map[string]OIDCProviderConfig) error {
	if len(providers) == 0 {
		return nil
	}
	factory := func() any { return &Account{} }
	strategies := make(map[string]*flow.KayanOIDCStrategy, len(providers))

	for id, cfg := range providers {
		provider, err := oidc.NewProvider(ctx, cfg.Issuer)
		if err != nil {
			return fmt.Errorf("auth: discover OIDC provider %s: %w", id, err)
		}
		client := &standardOIDCClient{config: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "email"},
		}}
		parser := &standardIDTokenParser{verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})}
		repo := &kayanOIDCRepo{db: service.db, provider: id}

		strategies[id] = flow.NewKayanOIDCStrategy(cfg.Issuer, cfg.ClientID, cfg.RedirectURL, client, parser, repo, factory)
	}

	service.oidc = strategies
	return nil
}

// HasOIDC reports whether at least one OIDC provider has been configured.
func (service *Service) HasOIDC() bool {
	return len(service.oidc) > 0
}

// GetOAuthURL initiates a login with providerID and returns the authorization
// URL to redirect the caller to. State, PKCE, and a nonce are generated and
// stored server-side by the strategy — there is nothing for the caller to
// generate or store itself, unlike the cookie-based CSRF token the previous
// OIDC flow required.
func (service *Service) GetOAuthURL(ctx context.Context, providerID string) (string, error) {
	strategy, ok := service.oidc[providerID]
	if !ok {
		return "", ErrOIDCNotConfigured
	}
	result, err := strategy.Initiate(ctx, "")
	if err != nil {
		return "", fmt.Errorf("auth: initiate OIDC login: %w", err)
	}
	data, ok := result.(map[string]string)
	if !ok {
		return "", fmt.Errorf("auth: unexpected OIDC initiate result %T", result)
	}
	return data["redirect_url"], nil
}

// HandleOAuthCallback exchanges the authorization code for an identity and
// creates a session bound to tenantID.
//
// A social login that provisions a new account assigns it to the tenant the
// flow was started from. An existing account must already belong to that
// tenant; a mismatch is rejected rather than silently re-homed.
func (service *Service) HandleOAuthCallback(ctx context.Context, tenantID, providerID, state, code string) (LoginResult, error) {
	strategy, ok := service.oidc[providerID]
	if !ok {
		return LoginResult{}, ErrOIDCNotConfigured
	}
	if tenantID == "" {
		return LoginResult{}, ErrNoTenant
	}

	identityValue, err := strategy.Authenticate(ctx, state, code)
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: OIDC callback: %w", err)
	}
	account, ok := identityValue.(*Account)
	if !ok {
		return LoginResult{}, ErrInvalidIdentityType
	}

	switch account.TenantID {
	case tenantID:
		// Existing member of this tenant.
	case "":
		// Freshly provisioned by the OIDC flow; assign it to this tenant.
		if err := service.db.WithContext(ctx).Model(&Account{}).
			Where("id = ?", account.ID).
			Update("tenant_id", tenantID).Error; err != nil {
			return LoginResult{}, fmt.Errorf("auth: assign tenant: %w", err)
		}
		account.TenantID = tenantID
	default:
		return LoginResult{}, ErrInvalidCredentials
	}

	if !account.Active {
		service.logAudit(ctx, audit.EventLoginBlocked, tenantID, account.ID, account.ID, false, "account is locked")
		return LoginResult{}, ErrAccountLocked
	}

	sessionValue, err := service.strategy.CreateForTenant(uuid.NewString(), account.ID, account.TenantID, account.TokenVersion)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		AccessToken:  sessionValue.ID,
		RefreshToken: sessionValue.RefreshToken,
		Subject:      account.ID,
		TenantID:     account.TenantID,
	}, nil
}

// Register creates an account inside tenantID.
//
// Kayan's registration flow does not know about tenants, so the tenant is
// stamped on the persisted row immediately afterwards.
//
// The two writes cannot share a transaction: Kayan's managers own their own
// connection, so a transaction held open here would block the registration
// write on it and deadlock. Instead the stamp is compensated — if it fails, the
// half-created account is deleted, because a row with no tenant can never be
// logged into and would hold its email address permanently.
//
// The uniqueness pre-check is advisory: it turns the common duplicate into a
// clean error instead of a constraint violation surfacing from inside the
// framework. The unique index on (tenant_id, email) is what actually enforces
// it under concurrency.
func (service *Service) Register(ctx context.Context, tenantID, email, password string) error {
	if tenantID == "" {
		return ErrNoTenant
	}

	var existing int64
	if err := service.db.WithContext(ctx).Model(&Account{}).
		Where("tenant_id = ? AND email = ?", tenantID, email).
		Count(&existing).Error; err != nil {
		return fmt.Errorf("auth: check existing account: %w", err)
	}
	if existing > 0 {
		return ErrAccountExists
	}

	// A second, distinct count: is this the first account in the tenant at
	// all, regardless of address. The duplicate-email check above is scoped
	// to this one email and is always zero for a first-time address, which
	// is not the same question — conflating them assigned "owner" to every
	// distinct email that registered, not only the tenant's first account.
	var accountsInTenant int64
	if err := service.db.WithContext(ctx).Model(&Account{}).
		Where("tenant_id = ?", tenantID).
		Count(&accountsInTenant).Error; err != nil {
		return fmt.Errorf("auth: count tenant accounts: %w", err)
	}

	// Key must match the Go struct field name used in MapFields ("Email").
	traits := kayanidentity.JSON(fmt.Sprintf(`{"Email":%q}`, email))
	created, err := service.registration.Submit(ctx, "password", traits, password)
	if err != nil {
		return err
	}

	account, ok := created.(*Account)
	if !ok {
		return ErrInvalidIdentityType
	}

	if err := service.db.WithContext(ctx).Model(&Account{}).
		Where("id = ?", account.ID).
		Update("tenant_id", tenantID).Error; err != nil {
		// Compensate: remove the untenanted account rather than leave it
		// stranded and blocking its address.
		if delErr := service.db.WithContext(ctx).
			Delete(&Account{}, "id = ?", account.ID).Error; delErr != nil {
			return fmt.Errorf("auth: assign tenant: %w (and rollback failed: %v)", err, delErr)
		}
		return fmt.Errorf("auth: assign tenant: %w", err)
	}

	// The very first account in a tenant needs a way to grant every other
	// role — otherwise nobody could ever assign one, including "admin" to
	// themselves. Every account after that starts as "member": ordinary
	// access, no kernel-management permissions, elevated deliberately by
	// someone who already holds them.
	scoped := tenant.WithTenantID(ctx, tenantID)
	role := "member"
	if accountsInTenant == 0 {
		role = "owner"
		// SeedDefaultRoles is idempotent — it skips any role name already
		// defined — so calling it here rather than only from tenant
		// provisioning also covers a tenant provisioned before this feature
		// existed, or provisioned by an operator who never called it.
		if err := service.SeedDefaultRoles(scoped, tenantID); err != nil {
			slog.WarnContext(ctx, "auth: failed to seed default roles at registration",
				"tenant", tenantID, "err", err)
		}
	}
	if err := service.AssignRole(scoped, account.ID, role); err != nil {
		slog.WarnContext(ctx, "auth: failed to assign default role at registration",
			"account", account.ID, "role", role, "err", err)
	}

	service.logAudit(scoped, audit.EventUserCreated, tenantID, account.ID, account.ID, true,
		fmt.Sprintf("account registered, assigned role %q", role))
	return nil
}

// Login authenticates a credential pair within tenantID and issues a session
// bound to that tenant.
//
// The tenant is verified against the stored account rather than trusted from
// the request: a caller who authenticates correctly but names a tenant they do
// not belong to is rejected with the same error as a bad password, so the
// response does not reveal which tenants an account exists in.
func (service *Service) Login(ctx context.Context, tenantID, email, password string) (LoginResult, error) {
	if tenantID == "" {
		return LoginResult{}, ErrNoTenant
	}

	// The account is looked up by (tenant_id, email) here rather than through
	// Kayan's LoginManager, which queries on the identifier alone and returns
	// whichever row matches first. With the same address registered in two
	// tenants that lookup is ambiguous, and it would authenticate against an
	// account the caller is not trying to log into.
	//
	// Kayan still owns the credential comparison: the hasher below is the same
	// one the registration strategy used to produce the stored hash.
	var account Account
	err := service.db.WithContext(ctx).
		Where("tenant_id = ? AND email = ?", tenantID, email).
		First(&account).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Spend the cost of a hash comparison anyway, so that a missing
			// account and a wrong password take indistinguishable time.
			service.hasher.Compare(password, dummyHash)
			service.logAudit(ctx, audit.EventLoginFailure, tenantID, "", "", false, "no such account")
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, fmt.Errorf("auth: look up account: %w", err)
	}

	if !service.hasher.Compare(password, account.PasswordHash) {
		service.logAudit(ctx, audit.EventLoginFailure, tenantID, account.ID, account.ID, false, "wrong password")
		return LoginResult{}, ErrInvalidCredentials
	}
	if !account.Active {
		service.logAudit(ctx, audit.EventLoginBlocked, tenantID, account.ID, account.ID, false, "account is locked")
		return LoginResult{}, ErrAccountLocked
	}

	sessionValue, err := service.strategy.CreateForTenant(uuid.NewString(), account.ID, account.TenantID, account.TokenVersion)
	if err != nil {
		return LoginResult{}, err
	}
	service.logAudit(ctx, audit.EventLoginSuccess, tenantID, account.ID, account.ID, true, "password login")

	return LoginResult{
		AccessToken:  sessionValue.ID,
		RefreshToken: sessionValue.RefreshToken,
		Subject:      account.ID,
		TenantID:     account.TenantID,
	}, nil
}

func (service *Service) Validate(accessToken string) (*kayanidentity.Session, error) {
	return service.sessions.Validate(context.Background(), accessToken)
}

// ValidateForSubject validates the access token and returns the authenticated
// identity's subject ID. Use this when only the caller's ID is needed and you
// want to avoid importing Kayan identity types in the calling package.
func (service *Service) ValidateForSubject(accessToken string) (string, error) {
	sess, err := service.sessions.Validate(context.Background(), accessToken)
	if err != nil {
		return "", err
	}
	return sess.IdentityID, nil
}

// ValidateForCaller validates the access token and returns the subject and the
// tenant the session is bound to. This is the entry point request middleware
// uses: the tenant comes from the signed token, never from the request.
func (service *Service) ValidateForCaller(accessToken string) (subject string, tenantID string, err error) {
	claims, err := service.strategy.ValidateClaims(accessToken)
	if err != nil {
		return "", "", err
	}

	// A signed, unrevoked token is not the whole answer for a human account:
	// it may have been locked, or had every session force-revoked, after this
	// token was issued. Looked up by ID alone, not by (tenant, ID) — a
	// service account's subject has no matching row here at all, which is
	// how this distinguishes the two without a type tag on the token.
	var account Account
	err = service.db.Where("id = ?", claims.Subject).First(&account).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// Not a human account — a service account, whose own revocation path
		// is ServiceAccount.Active plus the ordinary session revocation list.
	case err != nil:
		return "", "", fmt.Errorf("auth: look up account for validation: %w", err)
	default:
		if !account.Active {
			return "", "", ErrAccountLocked
		}
		if account.TokenVersion != claims.TokenVersion {
			return "", "", ErrSessionRevoked
		}
	}

	return claims.Subject, claims.TenantID, nil
}

func (service *Service) Repository() *gormstore.Repository {
	return service.repository
}

// Revocations returns the session revocation store.
//
// Exposed so an entrypoint can schedule PurgeExpired — without it the table
// grows for the lifetime of the deployment — and so an operator responding to a
// compromised token can revoke it directly rather than waiting for the holder
// to log out.
func (service *Service) Revocations() *RevocationStore {
	return service.revocations
}
