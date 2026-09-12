package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
)

// A JWT is valid because it is signed, not because a server remembers it. That
// is the property that makes it cheap — no lookup per request — and it is also
// why logout did nothing: nobody was asking whether the session still existed.
//
// The obvious fix, a server-side session store, gives back revocation by taking
// away the reason JWTs were chosen. Every request would read the session row,
// and the token's signature would stop being the authority.
//
// So this stores the opposite: not which sessions are valid, but which have been
// revoked. The list holds only sessions killed before their natural expiry,
// entries are dropped once the token they revoke has expired anyway, and the
// common path — a token nobody revoked — is answered from memory.
//
// This is a real trade, not a free win. A revocation is visible to other
// processes only after their cache window passes, so this is eventual within
// that window rather than immediate. That is stated on RevocationTTL rather
// than hidden, because an operator responding to a compromised token needs to
// know how long it takes to take effect.

// RevokedSession records a session invalidated before its token expires.
//
// The primary key is (session_id): revocation is per session, and a session
// belongs to exactly one tenant and one identity. TenantID is carried for
// auditing — "which tenant revoked what" is a question operators ask — and not
// as part of the key.
type RevokedSession struct {
	SessionID string `gorm:"primaryKey"`
	TenantID  string `gorm:"index"`
	// ExpiresAt is the moment the revoked token would have expired on its own.
	// After that the entry is redundant: the token fails signature validation
	// regardless, so keeping the row only grows the table forever.
	ExpiresAt time.Time `gorm:"index"`
	RevokedAt time.Time
	// Reason distinguishes an ordinary logout from a security response. An
	// operator investigating an incident needs to tell those apart.
	Reason string
}

func (RevokedSession) TableName() string { return "auth_revoked_sessions" }

// Revocation reasons. These are values rather than free text so a query can
// count them.
const (
	ReasonLogout     = "logout"
	ReasonCompromise = "compromise"
	ReasonAdmin      = "admin"
)

// ErrSessionRevoked is returned when a token validates cryptographically but its
// session has been revoked. It is deliberately distinct from a signature or
// expiry failure: the token is genuine, and the difference matters to anyone
// reading an auth log.
var ErrSessionRevoked = errors.New("auth: session has been revoked")

// RevocationTTL is how long a negative answer — "this session is not revoked" —
// is trusted without re-reading the database.
//
// This is the knob that trades latency for immediacy. Zero would make every
// request a database read, which is what a session store would have cost. The
// default accepts that a revocation takes effect within this window in a
// process that has already cached the answer, in exchange for the common path
// staying in memory.
//
// A deployment responding to compromise faster than this should lower it and
// pay the read cost knowingly.
var RevocationTTL = 30 * time.Second

// RevocationStore answers whether a session has been revoked.
//
// It is safe for concurrent use: every request path touches it.
type RevocationStore struct {
	db *gorm.DB

	mu    sync.RWMutex
	cache map[string]cachedRevocation
	now   func() time.Time
}

type cachedRevocation struct {
	revoked   bool
	checkedAt time.Time
}

// NewRevocationStore builds a store over db.
func NewRevocationStore(db *gorm.DB) *RevocationStore {
	return &RevocationStore{
		db:    db,
		cache: make(map[string]cachedRevocation),
		now:   time.Now,
	}
}

// Revoke marks a session invalid until its token would have expired.
//
// expiresAt must be the revoked token's own expiry. Passing a later time keeps a
// useless row; passing an earlier one un-revokes the session the moment it
// passes, which is the failure worth being careful about.
//
// Revoking an already-revoked session succeeds rather than erroring: logout is
// something a client may retry, and a second logout is not a failure.
func (s *RevocationStore) Revoke(ctx context.Context, sessionID, tenantID string, expiresAt time.Time, reason string) error {
	if sessionID == "" {
		return fmt.Errorf("auth: cannot revoke a session with no ID")
	}

	record := RevokedSession{
		SessionID: sessionID,
		TenantID:  tenantID,
		ExpiresAt: expiresAt,
		RevokedAt: s.now(),
		Reason:    reason,
	}

	// The system context: revocation is written during logout, where the request
	// carries a tenant, but also by an operator responding to compromise, where
	// it may not. The table is keyed by session rather than scoped by tenant, so
	// isolation has nothing to add here.
	err := s.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Assign(record).
		FirstOrCreate(&RevokedSession{SessionID: sessionID}).Error
	if err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}

	// Update the cache immediately so the process that performed the revocation
	// honours it on the very next request rather than after the TTL. A user who
	// clicks logout and is still logged in has not experienced a logout.
	s.mu.Lock()
	s.cache[sessionID] = cachedRevocation{revoked: true, checkedAt: s.now()}
	s.mu.Unlock()

	return nil
}

// IsRevoked reports whether a session has been revoked.
//
// A database error is reported rather than swallowed, and callers must fail the
// request on it. Treating an unreachable database as "not revoked" would mean a
// database outage silently restores every revoked session — the failure mode
// this whole mechanism exists to prevent.
func (s *RevocationStore) IsRevoked(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		// A token with no session ID cannot be revoked individually, which is
		// itself a reason to refuse it: it would be unrevokable forever.
		return false, fmt.Errorf("auth: token carries no session ID")
	}

	s.mu.RLock()
	entry, ok := s.cache[sessionID]
	s.mu.RUnlock()
	if ok && s.now().Sub(entry.checkedAt) < RevocationTTL {
		return entry.revoked, nil
	}

	var count int64
	err := s.db.WithContext(ctx).
		Model(&RevokedSession{}).
		Where("session_id = ?", sessionID).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("auth: check revocation: %w", err)
	}

	revoked := count > 0
	s.mu.Lock()
	s.cache[sessionID] = cachedRevocation{revoked: revoked, checkedAt: s.now()}
	s.mu.Unlock()
	return revoked, nil
}

// PurgeExpired deletes revocation entries whose tokens have expired on their
// own, and drops their cache entries.
//
// Without this the table grows for the lifetime of the deployment. An entry is
// only useful while the token it revokes would otherwise still validate.
func (s *RevocationStore) PurgeExpired(ctx context.Context) (int64, error) {
	cutoff := s.now()

	// Read the IDs first so the cache can be cleared for exactly the rows
	// removed. Clearing the whole cache instead would send every live session
	// back to the database at once.
	var expired []RevokedSession
	if err := s.db.WithContext(ctx).
		Where("expires_at <= ?", cutoff).
		Find(&expired).Error; err != nil {
		return 0, fmt.Errorf("auth: list expired revocations: %w", err)
	}
	if len(expired) == 0 {
		return 0, nil
	}

	result := s.db.WithContext(ctx).
		Where("expires_at <= ?", cutoff).
		Delete(&RevokedSession{})
	if result.Error != nil {
		return 0, fmt.Errorf("auth: purge revocations: %w", result.Error)
	}

	s.mu.Lock()
	for _, row := range expired {
		delete(s.cache, row.SessionID)
	}
	s.mu.Unlock()

	return result.RowsAffected, nil
}

// RevokeAllForIdentity is deliberately absent.
//
// Killing every session an identity holds requires knowing what those sessions
// are, which is the session store this design avoids. A deployment that needs
// it — a password reset invalidating other devices — should carry a per-identity
// token generation in the claim and compare it, rather than this table growing
// into the store it was written not to be.
