package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	kayanflow "github.com/getkayan/kayan/core/flow"
	kayanidentity "github.com/getkayan/kayan/core/identity"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

// Kayan v0.1.0's flow.OIDCManager is gone in v0.3.0, and not by accident: its
// callback took no state parameter, so it could not validate CSRF state, and
// it requested no nonce, so a captured ID token could be replayed. It also
// resolved a federated login to an existing local account by bare email —
// without checking the provider's own email_verified claim — which is an
// account takeover at any provider that lets a user assert an address they do
// not own.
//
// flow.KayanOIDCStrategy is the replacement: state, PKCE, and nonce are
// carried explicitly, server-side, single-use. This file supplies the three
// things it needs from us — an OIDCClient wrapping golang.org/x/oauth2, an
// IDTokenParser wrapping coreos/go-oidc's signature and claims verification,
// and a KayanOIDCRepository over our own tables — rather than any custom
// token or signature handling of our own. Kayan still owns state, PKCE,
// nonce, and the relying-party flow; only the transport (a standard OAuth2
// client) and JWT verification (a standard OIDC library) are ours to wire in,
// exactly as the strategy's interfaces ask.

// OIDCProviderConfig is one configured OIDC provider. Deliberately our own
// type: Kayan v0.3.0 removed core/config, and there is no equivalent type to
// import. This is unmarshalled directly from the deployment's provider JSON.
type OIDCProviderConfig struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURL  string `json:"redirect_url"`
}

// oidcState is one in-flight authorization attempt's server-side record:
// state, PKCE verifier, and nonce, single-use and short-lived. See
// migration 00009 for why this is not tenant-scoped.
type oidcState struct {
	State        string `gorm:"primaryKey;column:state"`
	CodeVerifier string
	Nonce        string
	ExpiresAt    time.Time
}

func (oidcState) TableName() string { return "oidc_states" }

// oidcIdentity maps one provider's stable subject claim to a local account.
// The lookup key is (provider, sub) — what the provider actually vouches
// for — never email, which is exactly the bug flow.OIDCManager had. See
// migration 00009.
type oidcIdentity struct {
	ID        string `gorm:"primaryKey"`
	Provider  string
	Sub       string
	AccountID string
	CreatedAt time.Time
}

func (oidcIdentity) TableName() string { return "oidc_identities" }

// kayanOIDCRepo implements flow.KayanOIDCRepository for one provider. One
// instance is constructed per configured provider, with the provider name
// closed over — the interface itself carries no provider parameter, since a
// strategy (and therefore a repository) is already scoped to one.
type kayanOIDCRepo struct {
	db       *gorm.DB
	provider string
}

func (r *kayanOIDCRepo) StoreOIDCState(ctx context.Context, state, codeVerifier, nonce string, expiry time.Duration) error {
	row := oidcState{
		State:        state,
		CodeVerifier: codeVerifier,
		Nonce:        nonce,
		ExpiresAt:    time.Now().Add(expiry),
	}
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("auth: store oidc state: %w", err)
	}
	return nil
}

// ConsumeOIDCState retrieves and deletes the state record atomically, inside
// a transaction: the delete's own row lock is what makes two concurrent
// callbacks for the same state resolve to exactly one success, the property
// that makes state genuinely single-use rather than merely time-limited.
func (r *kayanOIDCRepo) ConsumeOIDCState(ctx context.Context, state string) (codeVerifier, nonce string, err error) {
	txErr := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row oidcState
		if err := tx.Where("state = ?", state).First(&row).Error; err != nil {
			return err
		}
		if time.Now().After(row.ExpiresAt) {
			return fmt.Errorf("auth: oidc state expired")
		}
		result := tx.Where("state = ?", state).Delete(&oidcState{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			// Deleted by a concurrent request between the read above and this
			// delete — the state has already been consumed once.
			return fmt.Errorf("auth: oidc state already consumed")
		}
		codeVerifier, nonce = row.CodeVerifier, row.Nonce
		return nil
	})
	if txErr != nil {
		return "", "", txErr
	}
	return codeVerifier, nonce, nil
}

// FindOrCreateByProviderSub resolves the local account for this provider's
// subject claim, creating one on first login. A created account has no
// tenant yet — HandleOAuthCallback stamps it afterwards, the same
// compensating-write pattern password Register already uses, because the
// tenant is not known until the caller supplies it, after this returns.
func (r *kayanOIDCRepo) FindOrCreateByProviderSub(ctx context.Context, sub string, traits kayanidentity.JSON, factory func() any) (any, error) {
	var link oidcIdentity
	err := r.db.WithContext(ctx).Where("provider = ? AND sub = ?", r.provider, sub).First(&link).Error
	if err == nil {
		var account Account
		if err := r.db.WithContext(ctx).Where("id = ?", link.AccountID).First(&account).Error; err != nil {
			return nil, fmt.Errorf("auth: load account for oidc identity: %w", err)
		}
		return &account, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("auth: look up oidc identity: %w", err)
	}

	account, ok := factory().(*Account)
	if !ok {
		return nil, ErrInvalidIdentityType
	}
	var parsedTraits struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(traits, &parsedTraits)

	account.ID = uuid.NewString()
	account.Email = parsedTraits.Email
	account.Traits = traits

	txErr := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(account).Error; err != nil {
			return fmt.Errorf("auth: create account for oidc login: %w", err)
		}
		link := oidcIdentity{
			ID:        uuid.NewString(),
			Provider:  r.provider,
			Sub:       sub,
			AccountID: account.ID,
			CreatedAt: time.Now(),
		}
		if err := tx.Create(&link).Error; err != nil {
			return fmt.Errorf("auth: link oidc identity: %w", err)
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return account, nil
}

// standardOIDCClient implements flow.OIDCClient over golang.org/x/oauth2 and
// the provider's discovered endpoints. A standard client library, not a
// homegrown one: Kayan's strategy still owns state, PKCE, and nonce
// generation and verification, and this is only the transport underneath it.
type standardOIDCClient struct {
	config *oauth2.Config
}

func (c *standardOIDCClient) AuthorizationURL(request kayanflow.OIDCAuthorizationRequest) (string, error) {
	opts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("code_challenge", request.CodeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", request.CodeChallengeMethod),
		oidc.Nonce(request.Nonce),
	}
	return c.config.AuthCodeURL(request.State, opts...), nil
}

func (c *standardOIDCClient) Exchange(ctx context.Context, request kayanflow.OIDCTokenExchangeRequest) (*kayanflow.OIDCTokenSet, error) {
	token, err := c.config.Exchange(ctx, request.Code,
		oauth2.SetAuthURLParam("code_verifier", request.CodeVerifier))
	if err != nil {
		return nil, fmt.Errorf("auth: exchange oidc code: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("auth: token response carried no id_token")
	}
	return &kayanflow.OIDCTokenSet{IDToken: rawIDToken}, nil
}

// standardIDTokenParser implements flow.IDTokenParser over coreos/go-oidc's
// signature verification. The nonce comparison is ours to make explicitly:
// go-oidc parses the nonce claim but deliberately does not check it, since it
// has no way to know what value the caller expected.
type standardIDTokenParser struct {
	verifier *oidc.IDTokenVerifier
}

func (p *standardIDTokenParser) ParseAndVerify(rawIDToken, _, _, expectedNonce string) (*kayanflow.IDTokenClaims, error) {
	idToken, err := p.verifier.Verify(context.Background(), rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("auth: verify id_token: %w", err)
	}
	if idToken.Nonce != expectedNonce {
		return nil, fmt.Errorf("auth: id_token nonce does not match the authorization request")
	}

	var claims struct {
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("auth: decode id_token claims: %w", err)
	}
	return &kayanflow.IDTokenClaims{Sub: idToken.Subject, Email: claims.Email}, nil
}
