// Package kayan provides a Kayan-backed implementation of auth.Authenticator.
// Import this sub-package only when you want Kayan for auth; you may replace
// it with any other struct that satisfies auth.Authenticator.
//
// This package names *gorm.DB, and that is deliberate rather than a breach of
// ADR 0006's layering rule. It is a concrete adapter, in the same position as
// internal/platform/storage: the engine-neutral surface is auth.Authenticator,
// and this is one binding of it. A caller who does not want GORM implements
// that interface instead of importing this package. The rule ADR 0006 enforces
// is that pkg/modulekit — the surface every module compiles against — names no
// engine, and pkg/modulekit/boundary_test.go tests exactly that.
package kayan

import (
	"context"

	internalauth "github.com/sannados/sannad/internal/platform/auth"
	"github.com/sannados/sannad/pkg/auth"
	"gorm.io/gorm"
)

// compile-time check that *Adapter satisfies auth.Authenticator.
var _ auth.Authenticator = (*Adapter)(nil)

// Adapter wraps the Kayan-backed internal auth service.
type Adapter struct {
	inner *internalauth.Service
}

// New creates an Adapter. The db must already have the Kayan tables present
// (use the sannad migrations or run them yourself).
//
// previousSecrets are prior signing secrets, still accepted to verify a
// token issued before a rotation of sessionSecret but never used to sign a
// new one.
func New(db *gorm.DB, sessionSecret string, previousSecrets ...string) (*Adapter, error) {
	svc, err := internalauth.NewService(db, sessionSecret, previousSecrets...)
	if err != nil {
		return nil, err
	}
	return &Adapter{inner: svc}, nil
}

func (a *Adapter) Register(ctx context.Context, tenantID, email, password string) error {
	return a.inner.Register(ctx, tenantID, email, password)
}

func (a *Adapter) Login(ctx context.Context, tenantID, email, password string) (auth.LoginResult, error) {
	res, err := a.inner.Login(ctx, tenantID, email, password)
	if err != nil {
		return auth.LoginResult{}, err
	}
	return auth.LoginResult{
		AccessToken: res.AccessToken,
		Subject:     res.Subject,
		TenantID:    res.TenantID,
	}, nil
}

func (a *Adapter) ValidateForSubject(token string) (string, error) {
	return a.inner.ValidateForSubject(token)
}
