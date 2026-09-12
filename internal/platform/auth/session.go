package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/getkayan/kayan/core/identity"
	"github.com/golang-jwt/jwt/v5"
)

// ErrNoTenantClaim is returned when a token validates but carries no tenant_id
// claim. Tokens issued before tenant membership existed fall into this case and
// are rejected rather than treated as unscoped.
var ErrNoTenantClaim = errors.New("auth: token carries no tenant_id claim")

// TenantClaims extends Kayan's session claims with the tenant the session is
// bound to.
//
// Kayan's session.JWTClaims is a closed struct with no extension point, and
// identity.Session has no tenant field, so a session strategy that carries
// tenant membership has to be implemented here. It satisfies the same
// session.Strategy interface, so session.Manager drives it unchanged.
//
// The tenant is a signed claim rather than a request header or query parameter
// precisely so that it cannot be chosen by the caller. See ADR 0005.
type TenantClaims struct {
	SessionID string `json:"sid"`
	TenantID  string `json:"tenant_id"`
	// TokenVersion is compared against the account's current TokenVersion at
	// validation time (see Service.ValidateForCaller). A mismatch means the
	// account was locked or had its sessions force-revoked after this token
	// was issued — the token is cryptographically genuine and rejected
	// anyway. Absent from tokens issued before this field existed, which
	// parses as zero and matches an account's own zero-value TokenVersion,
	// so an old token stays valid until the first admin action that would
	// have invalidated it.
	TokenVersion int `json:"ver"`
	jwt.RegisteredClaims
}

// TenantJWTStrategy issues and validates HS256 sessions that carry a tenant
// claim. It implements session.Strategy.
type TenantJWTStrategy struct {
	secret        []byte
	expiry        time.Duration
	refreshExpiry time.Duration

	// previousSecrets are old signing secrets kept for verification only,
	// newest first. Every new token signs with secret alone; a token signed
	// under a since-rotated secret still validates against whichever entry
	// here matches, until it expires or the operator drops that entry. This
	// is what makes rotating SANNAD_SESSION_SECRET safe without a forced
	// mass-logout: without it, changing the signing secret would make every
	// unexpired token everywhere fail signature verification the instant the
	// new secret deployed.
	previousSecrets [][]byte

	// revocations is nil when the deployment has no revocation store wired.
	// Validation then behaves as it did before revocation existed: a signed,
	// unexpired token is accepted. This is checked explicitly at each use rather
	// than assumed, because a nil store silently accepting revoked sessions is
	// exactly the bug this feature exists to remove.
	revocations *RevocationStore
}

// WithRevocation attaches a revocation store, making Delete effective and
// Validate reject revoked sessions.
func (s *TenantJWTStrategy) WithRevocation(store *RevocationStore) *TenantJWTStrategy {
	s.revocations = store
	return s
}

// NewTenantJWTStrategy returns a strategy signing with secret. Access tokens
// live for expiry; refresh tokens for refreshExpiry.
//
// previousSecrets are prior signing secrets, accepted for verification only
// — never used to sign a new token. Pass the secret being retired here when
// rotating SANNAD_SESSION_SECRET, and keep it listed until the longest-lived
// token that could have been signed with it (a refresh token, up to
// refreshExpiry old) has had time to expire naturally; dropping it early
// invalidates every session still relying on it, with no way to warn the
// holder first.
func NewTenantJWTStrategy(secret string, expiry, refreshExpiry time.Duration, previousSecrets ...string) *TenantJWTStrategy {
	previous := make([][]byte, len(previousSecrets))
	for i, s := range previousSecrets {
		previous[i] = []byte(s)
	}
	return &TenantJWTStrategy{
		secret:          []byte(secret),
		expiry:          expiry,
		refreshExpiry:   refreshExpiry,
		previousSecrets: previous,
	}
}

// CreateForTenant issues an access and refresh token pair bound to tenantID.
//
// This is the constructor the login and OAuth flows use. The plain Create
// method exists only to satisfy session.Strategy and deliberately refuses to
// issue an untenanted token.
//
// tokenVersion is optional — omitted, it signs zero, which matches every
// account's TokenVersion until an admin action increments it. The existing
// callers in this package's own tests never touch locking or forced logout
// and are unaffected by leaving it out.
func (s *TenantJWTStrategy) CreateForTenant(sessionID, identityID any, tenantID string, tokenVersion ...int) (*identity.Session, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("auth: cannot issue a session without a tenant: %w", ErrNoTenantClaim)
	}
	var ver int
	if len(tokenVersion) > 0 {
		ver = tokenVersion[0]
	}

	now := time.Now()
	accessExpiry := now.Add(s.expiry)
	refreshExpiry := now.Add(s.refreshExpiry)

	access, err := s.sign(sessionID, identityID, tenantID, ver, now, accessExpiry)
	if err != nil {
		return nil, err
	}
	refresh, err := s.sign(sessionID, identityID, tenantID, ver, now, refreshExpiry)
	if err != nil {
		return nil, err
	}

	return &identity.Session{
		ID:               access,
		IdentityID:       fmt.Sprintf("%v", identityID),
		RefreshToken:     refresh,
		ExpiresAt:        accessExpiry,
		RefreshExpiresAt: refreshExpiry,
		IssuedAt:         now,
		Active:           true,
	}, nil
}

// Create satisfies session.Strategy. It always fails: every Sannad session is
// tenant-bound, and a caller reaching this method has bypassed the login flow.
// Use CreateForTenant.
func (s *TenantJWTStrategy) Create(_ context.Context, _, _ any) (*identity.Session, error) {
	return nil, fmt.Errorf("auth: use CreateForTenant; %w", ErrNoTenantClaim)
}

// Validate satisfies session.Strategy: parses and verifies a token, returning
// the session and its tenant. A token without a tenant claim is rejected.
func (s *TenantJWTStrategy) Validate(_ context.Context, sessionID any) (*identity.Session, error) {
	_, sess, err := s.ValidateWithTenant(sessionID)
	return sess, err
}

// ValidateWithTenant verifies a token and returns the tenant it is bound to
// alongside the session.
func (s *TenantJWTStrategy) ValidateWithTenant(sessionID any) (string, *identity.Session, error) {
	claims, err := s.ValidateClaims(sessionID)
	if err != nil {
		return "", nil, err
	}
	tokenString, _ := sessionID.(string)
	return claims.TenantID, &identity.Session{
		ID:         tokenString,
		IdentityID: claims.Subject,
		ExpiresAt:  claims.ExpiresAt.Time,
		IssuedAt:   claims.IssuedAt.Time,
		Active:     true,
	}, nil
}

// ValidateClaims verifies a token and returns its full claims, including
// TokenVersion — the piece ValidateWithTenant's identity.Session shape has
// no field for, and which Service.ValidateForCaller needs to enforce a
// lock or a forced logout.
func (s *TenantJWTStrategy) ValidateClaims(sessionID any) (*TenantClaims, error) {
	tokenString, ok := sessionID.(string)
	if !ok {
		return nil, errors.New("auth: invalid token format")
	}

	claims, err := s.parse(tokenString)
	if err != nil {
		return nil, err
	}
	if claims.TenantID == "" {
		return nil, ErrNoTenantClaim
	}

	// A revoked session's token is still cryptographically valid, so this is the
	// only place the difference can be caught. A store error fails the request
	// rather than defaulting to "not revoked": treating an unreachable database
	// as permission would make a database outage restore every revoked session.
	if s.revocations != nil {
		revoked, err := s.revocations.IsRevoked(context.Background(), claims.SessionID)
		if err != nil {
			return nil, err
		}
		if revoked {
			return nil, ErrSessionRevoked
		}
	}

	return claims, nil
}

// Refresh exchanges a valid refresh token for a new token pair, preserving the
// tenant binding of the original session.
func (s *TenantJWTStrategy) Refresh(ctx context.Context, refreshToken string) (*identity.Session, error) {
	claims, err := s.parse(refreshToken)
	if err != nil {
		return nil, err
	}
	if claims.TenantID == "" {
		return nil, ErrNoTenantClaim
	}

	// Refresh carries the session ID forward, so a refresh token outlives the
	// access token it was issued with. Without this check a client could log
	// out and immediately mint a new pair from the refresh token it still
	// holds — undoing its own logout, and leaving a revoked session alive for
	// as long as it kept refreshing.
	if s.revocations != nil {
		revoked, err := s.revocations.IsRevoked(ctx, claims.SessionID)
		if err != nil {
			return nil, err
		}
		if revoked {
			return nil, ErrSessionRevoked
		}
	}

	return s.CreateForTenant(claims.SessionID, claims.Subject, claims.TenantID, claims.TokenVersion)
}

// Delete revokes a session so its token stops validating before it expires.
//
// Without a revocation store this is a no-op, which is what it was before one
// existed: a stateless token cannot be withdrawn. The error says so rather than
// returning nil, because a logout that silently does nothing is worse than one
// that reports it cannot.
func (s *TenantJWTStrategy) Delete(ctx context.Context, sessionID any) error {
	if s.revocations == nil {
		return ErrRevocationUnavailable
	}

	tokenString, ok := sessionID.(string)
	if !ok {
		return errors.New("auth: invalid token format")
	}

	// Parsed rather than trusted: the session ID, tenant, and expiry all come
	// from the signed claims. Taking them from the caller would let anyone
	// revoke anyone else's session by naming it.
	claims, err := s.parse(tokenString)
	if err != nil {
		return err
	}
	if claims.SessionID == "" {
		return errors.New("auth: token carries no session ID and cannot be revoked")
	}

	expiry := time.Now().Add(s.refreshExpiry)
	if claims.ExpiresAt != nil {
		// The refresh token outlives the access token and carries the same
		// session ID, so the entry must survive until the longest-lived token
		// bound to this session would have expired. Using the access token's
		// own expiry would un-revoke the session the moment it passed, leaving
		// the refresh token able to mint a new pair.
		if refreshDeadline := claims.IssuedAt.Add(s.refreshExpiry); refreshDeadline.After(expiry) {
			expiry = refreshDeadline
		}
	}

	return s.revocations.Revoke(ctx, claims.SessionID, claims.TenantID,
		expiry, ReasonLogout)
}

// ErrRevocationUnavailable is returned by Delete when no revocation store is
// wired. It is an error rather than a silent success so a deployment cannot
// believe it has logout when it does not.
var ErrRevocationUnavailable = errors.New("auth: revocation is not configured; sessions remain valid until they expire")

func (s *TenantJWTStrategy) sign(sessionID, identityID any, tenantID string, tokenVersion int, issued, expires time.Time) (string, error) {
	claims := TenantClaims{
		SessionID:    fmt.Sprintf("%v", sessionID),
		TenantID:     tenantID,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%v", identityID),
			ExpiresAt: jwt.NewNumericDate(expires),
			IssuedAt:  jwt.NewNumericDate(issued),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return signed, nil
}

// parse verifies the signature and pins the algorithm to HS256, rejecting a
// token that asks to be verified with a different one.
//
// It tries the active secret first, then each previous one in order, so the
// common case — every token still signed with the current secret — costs a
// single parse. A token only reaches a previous secret at all in the window
// right after a rotation, before it naturally expires.
func (s *TenantJWTStrategy) parse(tokenString string) (*TenantClaims, error) {
	var lastErr error
	for _, key := range append([][]byte{s.secret}, s.previousSecrets...) {
		claims, err := s.parseWithKey(tokenString, key)
		if err == nil {
			return claims, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("auth: parse token: %w", lastErr)
}

func (s *TenantJWTStrategy) parseWithKey(tokenString string, key []byte) (*TenantClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &TenantClaims{}, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}
		return key, nil
	})
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*TenantClaims)
	if !ok || !token.Valid {
		return nil, errors.New("auth: invalid token")
	}
	return claims, nil
}
