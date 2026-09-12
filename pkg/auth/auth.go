// Package auth defines the Authenticator interface that every auth backend
// must satisfy. Sannad-os ships an optional Kayan-backed adapter in the
// sub-package pkg/auth/kayan; developers may provide any other implementation.
//
// Typical usage:
//
//	// choose a backend — swap for your own implementation at any time
//	var svc auth.Authenticator = kayan.New(db, sessionSecret)
//
//	result, err := svc.Login(ctx, tenantID, email, password)
//	subject, err := svc.ValidateForSubject(result.AccessToken)
//	ctx = modulekit.WithCaller(ctx, modulekit.CallerInfo{Subject: subject})
package auth

import "context"

// LoginResult is the successful outcome of a Login call.
type LoginResult struct {
	// AccessToken is an opaque, short-lived credential the caller presents on
	// subsequent requests. The format is determined by the backend.
	AccessToken string
	// Subject is the stable identity ID of the authenticated user.
	Subject string
	// TenantID is the tenant the session is bound to. It is derived from the
	// account, never from caller input, and every subsequent request carries it
	// inside the access token rather than alongside it.
	TenantID string
}

// Authenticator is the single interface every auth backend must implement.
// It covers registration, password login, and token validation; more advanced
// flows (OIDC, MFA, refresh) are left to the concrete implementation.
type Authenticator interface {
	// Register creates a new user account within tenantID, identified by email
	// and protected by the supplied password. Returns an error if the email is
	// already taken within that tenant; the same address in another tenant is
	// not a conflict.
	Register(ctx context.Context, tenantID, email, password string) error

	// Login verifies the credentials against an account in tenantID and returns
	// a LoginResult whose access token is bound to that tenant. Credentials
	// that are valid for a different tenant fail exactly as a wrong password
	// does, so the error does not disclose where an account exists.
	Login(ctx context.Context, tenantID, email, password string) (LoginResult, error)

	// ValidateForSubject verifies the token returned by Login and returns the
	// subject ID of the authenticated user. Use the subject as
	// modulekit.CallerInfo.Subject before dispatching to the kernel.
	ValidateForSubject(token string) (string, error)
}
