package auth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/getkayan/kayan/core/audit"
	"github.com/getkayan/kayan/core/domain"
	kayanidentity "github.com/getkayan/kayan/core/identity"
	"github.com/getkayan/kayan/core/tenant"
	ksaml "github.com/getkayan/kayan/kayan-saml"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Kayan's kayan-saml package is a transport-neutral SAML 2.0 Service
// Provider: it owns AuthnRequest generation, signature verification
// (including XML Signature Wrapping defenses), replay detection, and
// assertion validation. This file supplies the three things the strategy
// still needs from us: a pending-authentication session store, a per-tenant
// identity provider registry, and reconciliation against our own Account
// type — no XML or cryptography of our own.
//
// The generic domain.IdentityStorage path ksaml.ServiceProvider otherwise
// falls back to does not fit Account: its zero-value factory produces an
// Account with no ID and no tenant, and on first-ever provisioning it always
// also writes to Kayan's own "credentials" table (kayan-gorm's
// gormCredential), which carries a tenant_id column this deployment's schema
// does not have — nothing else in this codebase uses that table, since
// PasswordStrategy stores its hash directly on Account instead (BYOS mode).
// OIDC hit the identical shape of problem and built its own linking table
// (oidc_identities); saml.go follows the same pattern with saml_identities,
// and noopIdentityStorage below makes the otherwise-unavoidable
// CreateCredential/GetCredentialByIdentifier calls harmless no-ops.
var (
	// ErrSAMLNotConfigured is returned when a SAML operation is attempted but
	// no identity provider has been configured for this deployment.
	ErrSAMLNotConfigured = errors.New("auth: SAML is not configured")
)

// SAMLProviderConfig is one configured enterprise identity provider.
type SAMLProviderConfig struct {
	// TenantID is the tenant this identity provider authenticates into.
	// Unlike OIDC social login, a SAML IdP is a property of one enterprise
	// customer: the tenant is fixed by which IdP the caller signs in
	// through, decided at configuration time, never chosen at login time.
	TenantID string `json:"tenant_id"`

	// EntityID is the IdP's own entity ID, from its metadata. Only used when
	// SSOUrl/Certificate are set directly rather than via MetadataURL.
	EntityID string `json:"entity_id"`

	// SSOUrl is the IdP's SSO endpoint. Required unless MetadataURL is set.
	SSOUrl string `json:"sso_url"`

	// Certificate is the IdP's signing certificate, PEM-encoded. Required
	// unless MetadataURL is set.
	Certificate string `json:"certificate"`

	// MetadataURL fetches EntityID, SSOUrl, and every signing certificate
	// from the IdP's published metadata instead of setting them
	// individually. A public HTTPS host is required — see kayan-saml's
	// default metadata URL policy.
	MetadataURL string `json:"metadata_url"`

	// AttributeMapping maps SAML attribute names to identity fields (email,
	// first_name, last_name). Empty tries common defaults.
	AttributeMapping map[string]string `json:"attribute_mapping"`
}

// SAMLSPConfig configures this deployment's own SAML Service Provider —
// deployment-wide, not per-IdP: every configured identity provider posts
// back to the same ACS endpoint, which resolves the right IdP from the
// pending session the login started.
type SAMLSPConfig struct {
	// EntityID is this SP's own entity ID, usually a URL.
	EntityID string
	// ACSUrl is where this deployment's Assertion Consumer Service endpoint
	// is reachable.
	ACSUrl string
}

// samlSession is one pending SP-initiated authentication attempt, persisted
// so it survives across the redirect to the identity provider and back.
type samlSession struct {
	ID                     string `gorm:"primaryKey"`
	RequestID              string `gorm:"index"`
	IdPID                  string `gorm:"column:idp_id"`
	RelayState             string
	ReturnURL              string
	ForceAuthn             bool
	RequestedAuthnContexts kayanidentity.JSON
	CreateTime             time.Time
	ExpiresAt              time.Time
}

func (samlSession) TableName() string { return "saml_sessions" }

// samlSessionStore implements ksaml.SessionStore over samlSession.
type samlSessionStore struct{ db *gorm.DB }

func (s *samlSessionStore) Save(ctx context.Context, session *ksaml.Session) error {
	contexts, err := json.Marshal(session.RequestedAuthnContexts)
	if err != nil {
		return fmt.Errorf("auth: marshal saml requested authn contexts: %w", err)
	}
	row := samlSession{
		ID:                     session.ID,
		RequestID:              session.RequestID,
		IdPID:                  session.IdPID,
		RelayState:             session.RelayState,
		ReturnURL:              session.ReturnURL,
		ForceAuthn:             session.ForceAuthn,
		RequestedAuthnContexts: contexts,
		CreateTime:             session.CreateTime,
		ExpiresAt:              session.ExpiresAt,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("auth: save saml session: %w", err)
	}
	return nil
}

func (s *samlSessionStore) Get(ctx context.Context, id string) (*ksaml.Session, error) {
	var row samlSession
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		return nil, err
	}
	return row.toSession(), nil
}

func (s *samlSessionStore) GetByRequestID(ctx context.Context, requestID string) (*ksaml.Session, error) {
	var row samlSession
	if err := s.db.WithContext(ctx).Where("request_id = ?", requestID).First(&row).Error; err != nil {
		return nil, err
	}
	return row.toSession(), nil
}

func (s *samlSessionStore) Delete(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Where("id = ?", id).Delete(&samlSession{}).Error
}

func (row *samlSession) toSession() *ksaml.Session {
	var contexts []string
	_ = json.Unmarshal(row.RequestedAuthnContexts, &contexts)
	return &ksaml.Session{
		ID:                     row.ID,
		RequestID:              row.RequestID,
		IdPID:                  row.IdPID,
		RelayState:             row.RelayState,
		ReturnURL:              row.ReturnURL,
		ForceAuthn:             row.ForceAuthn,
		RequestedAuthnContexts: contexts,
		CreateTime:             row.CreateTime,
		ExpiresAt:              row.ExpiresAt,
	}
}

// samlIdentity links one identity provider's NameID to a local account —
// the SAML equivalent of oidc_identities, for the identical reason: the
// lookup key is what the provider actually vouches for, never email.
type samlIdentity struct {
	ID        string `gorm:"primaryKey"`
	IdPID     string `gorm:"column:idp_id;uniqueIndex:idx_saml_identities_idp_name"`
	NameID    string `gorm:"column:name_id;uniqueIndex:idx_saml_identities_idp_name"`
	AccountID string
	CreatedAt time.Time
}

func (samlIdentity) TableName() string { return "saml_identities" }

// noopIdentityStorage satisfies domain.IdentityStorage without touching any
// table. Reconciliation runs entirely through the Hooks wired in InitSAML
// below; this exists only because ksaml.NewServiceProvider requires a
// domain.IdentityStorage value and, on a caller's first-ever login through a
// given IdP, still calls CreateCredential and (before that)
// GetCredentialByIdentifier once — see this file's package comment for why
// those must be no-ops here rather than reaching Kayan's own credentials
// table.
type noopIdentityStorage struct{}

func (noopIdentityStorage) CreateIdentity(context.Context, any) error { return nil }

func (noopIdentityStorage) GetIdentity(context.Context, func() any, any) (any, error) {
	return nil, domain.ErrNotFound
}

func (noopIdentityStorage) FindIdentity(context.Context, func() any, map[string]any) (any, error) {
	return nil, domain.ErrNotFound
}

func (noopIdentityStorage) ListIdentities(context.Context, func() any, int, int) ([]any, error) {
	return nil, nil
}

func (noopIdentityStorage) UpdateIdentity(context.Context, any) error { return nil }

func (noopIdentityStorage) DeleteIdentity(context.Context, func() any, any) error { return nil }

func (noopIdentityStorage) CreateCredential(context.Context, any) error { return nil }

func (noopIdentityStorage) GetCredentialByIdentifier(context.Context, string, string) (*kayanidentity.Credential, error) {
	return nil, domain.ErrNotFound
}

func (noopIdentityStorage) UpdateCredentialSecret(context.Context, string, string, string) error {
	return nil
}

// InitSAML wires enterprise SAML SSO into the service. Call this once during
// bootstrap after NewService; it is a no-op when providers is empty.
func (service *Service) InitSAML(ctx context.Context, spConfig SAMLSPConfig, providers map[string]SAMLProviderConfig) error {
	if len(providers) == 0 {
		return nil
	}
	if spConfig.EntityID == "" || spConfig.ACSUrl == "" {
		return fmt.Errorf("auth: SAML requires EntityID and ACSUrl")
	}

	provider := ksaml.NewServiceProvider(
		ksaml.Config{
			EntityID:   spConfig.EntityID,
			ACSUrl:     spConfig.ACSUrl,
			SessionTTL: 5 * time.Minute,
			// AllowIdPInitiated stays false (the zero value): an unsolicited
			// assertion is accepted only against a session this SP itself
			// started, which is what makes InResponseTo meaningful in the
			// first place. Enterprise SSO can always launch from this
			// deployment's own login link instead.
		},
		&samlSessionStore{db: service.db},
		noopIdentityStorage{},
		func() any { return &Account{} },
		ksaml.WithAutoProvision(),
	)

	idpTenants := make(map[string]string, len(providers))
	for id, cfg := range providers {
		if cfg.TenantID == "" {
			return fmt.Errorf("auth: SAML provider %q: tenant_id is required", id)
		}

		var idp *ksaml.IdPConfig
		if cfg.MetadataURL != "" {
			if err := provider.RegisterIdPFromMetadata(ctx, id, cfg.MetadataURL); err != nil {
				return fmt.Errorf("auth: register SAML provider %q: %w", id, err)
			}
			var ok bool
			idp, ok = provider.GetIdP(id)
			if !ok {
				return fmt.Errorf("auth: SAML provider %q vanished after registration", id)
			}
		} else {
			if cfg.SSOUrl == "" || cfg.Certificate == "" {
				return fmt.Errorf("auth: SAML provider %q: sso_url and certificate are required without metadata_url", id)
			}
			cert, err := parsePEMCertificate(cfg.Certificate)
			if err != nil {
				return fmt.Errorf("auth: SAML provider %q: %w", id, err)
			}
			idp = &ksaml.IdPConfig{ID: id, EntityID: cfg.EntityID, SSOUrl: cfg.SSOUrl, Certificate: cert}
			provider.RegisterIdP(idp)
		}
		idp.TenantID = cfg.TenantID
		idp.AttributeMapping = cfg.AttributeMapping
		idpTenants[id] = cfg.TenantID
	}

	provider.SetHooks(ksaml.Hooks{
		IDGenerator: uuid.NewString,
		UserLoader: func(ctx context.Context, nameID, idpID string) (any, error) {
			var link samlIdentity
			if err := service.db.WithContext(ctx).
				Where("idp_id = ? AND name_id = ?", idpID, nameID).
				First(&link).Error; err != nil {
				return nil, err
			}
			var account Account
			if err := service.db.WithContext(ctx).Where("id = ?", link.AccountID).First(&account).Error; err != nil {
				return nil, fmt.Errorf("auth: load account for saml identity: %w", err)
			}
			return &account, nil
		},
		// UserFactory provisions a new account for a NameID seen for the
		// first time. Unlike OIDC social login, the tenant is already known
		// here — it is the IdP's own configured tenant — so, unlike
		// Register/HandleOAuthCallback, there is no untenanted row and no
		// compensating write: the account is created with its tenant
		// already stamped, in the same transaction as the identity link.
		UserFactory: func(ctx context.Context, user *ksaml.SAMLUser) (any, error) {
			tenantID := idpTenants[user.IdPID]
			account := &Account{ID: uuid.NewString(), TenantID: tenantID, Email: user.Email}
			if traits, err := json.Marshal(map[string]string{
				"email":      user.Email,
				"first_name": user.FirstName,
				"last_name":  user.LastName,
			}); err == nil {
				account.Traits = traits
			}

			txErr := service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := tx.Create(account).Error; err != nil {
					return fmt.Errorf("auth: create account for saml login: %w", err)
				}
				link := samlIdentity{
					ID:        uuid.NewString(),
					IdPID:     user.IdPID,
					NameID:    user.NameID,
					AccountID: account.ID,
					CreatedAt: time.Now(),
				}
				if err := tx.Create(&link).Error; err != nil {
					return fmt.Errorf("auth: link saml identity: %w", err)
				}
				return nil
			})
			if txErr != nil {
				return nil, txErr
			}

			// The very first account provisioned into a tenant this way
			// needs the same bootstrap "owner" role Register grants its
			// tenant's first account — otherwise an enterprise customer
			// whose entire workforce arrives via SAML would have nobody
			// able to grant any permission at all.
			scoped := tenant.WithTenantID(ctx, tenantID)
			role := "member"
			var accountsInTenant int64
			if err := service.db.WithContext(ctx).Model(&Account{}).
				Where("tenant_id = ? AND id != ?", tenantID, account.ID).
				Count(&accountsInTenant).Error; err == nil && accountsInTenant == 0 {
				role = "owner"
				if err := service.SeedDefaultRoles(scoped, tenantID); err != nil {
					slog.WarnContext(ctx, "auth: failed to seed default roles at saml provisioning",
						"tenant", tenantID, "err", err)
				}
			}
			if err := service.AssignRole(scoped, account.ID, role); err != nil {
				slog.WarnContext(ctx, "auth: failed to assign default role at saml provisioning",
					"account", account.ID, "role", role, "err", err)
			}
			service.logAudit(scoped, audit.EventUserCreated, tenantID, account.ID, account.ID, true,
				fmt.Sprintf("account provisioned via SAML idp %q, assigned role %q", user.IdPID, role))

			return account, nil
		},
	})

	service.saml = provider
	return nil
}

// HasSAML reports whether at least one SAML identity provider is configured.
func (service *Service) HasSAML() bool {
	return service.saml != nil
}

// InitiateSAMLLogin starts the SAML authentication flow for idpID and
// returns the URL to redirect the caller to. There is no tenant parameter:
// the tenant is a property of which identity provider the caller is
// directed through, fixed at configuration time, never chosen by the
// request — unlike OIDC social login, where the tenant is not known until
// after the caller authenticates.
func (service *Service) InitiateSAMLLogin(ctx context.Context, idpID, returnURL string) (string, error) {
	if service.saml == nil {
		return "", ErrSAMLNotConfigured
	}
	if _, ok := service.saml.GetIdP(idpID); !ok {
		return "", fmt.Errorf("auth: SAML provider %q is not configured", idpID)
	}
	return service.saml.InitiateLogin(ctx, idpID, returnURL)
}

// HandleSAMLResponse validates the identity provider's assertion and issues
// a session for the reconciled account, exactly as Login and
// HandleOAuthCallback do for their own credential types.
func (service *Service) HandleSAMLResponse(ctx context.Context, samlResponse, relayState string) (LoginResult, error) {
	if service.saml == nil {
		return LoginResult{}, ErrSAMLNotConfigured
	}

	identityValue, err := service.saml.ProcessResponse(ctx, samlResponse, relayState)
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: saml response: %w", err)
	}
	account, ok := identityValue.(*Account)
	if !ok {
		return LoginResult{}, ErrInvalidIdentityType
	}

	if !account.Active {
		service.logAudit(ctx, audit.EventLoginBlocked, account.TenantID, account.ID, account.ID, false, "account is locked")
		return LoginResult{}, ErrAccountLocked
	}

	sessionValue, err := service.strategy.CreateForTenant(uuid.NewString(), account.ID, account.TenantID, account.TokenVersion)
	if err != nil {
		return LoginResult{}, err
	}
	service.logAudit(ctx, audit.EventLoginSuccess, account.TenantID, account.ID, account.ID, true, "saml login")

	return LoginResult{
		AccessToken:  sessionValue.ID,
		RefreshToken: sessionValue.RefreshToken,
		Subject:      account.ID,
		TenantID:     account.TenantID,
	}, nil
}

// SAMLMetadata returns this deployment's SP metadata document, served
// unauthenticated so an identity provider administrator can fetch it while
// configuring the integration on their end.
func (service *Service) SAMLMetadata() ([]byte, error) {
	if service.saml == nil {
		return nil, ErrSAMLNotConfigured
	}
	return service.saml.GetMetadata()
}

func parsePEMCertificate(pemStr string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("certificate is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}
