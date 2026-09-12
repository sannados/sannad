package auth

import kayanidentity "github.com/getkayan/kayan/core/identity"

// Account is the authenticated principal.
//
// TenantID is the account's tenant membership and is the authoritative source
// for the tenant claim placed in every issued session. Nothing derived from
// request input may override it. See ADR 0005.
//
// Email is unique per tenant rather than globally: two tenants may each have a
// user at the same address, and a global unique index would leak the existence
// of an account in one tenant to another.
type Account struct {
	ID           string `gorm:"primaryKey"`
	TenantID     string `gorm:"index;uniqueIndex:idx_accounts_tenant_email;not null"`
	Email        string `gorm:"uniqueIndex:idx_accounts_tenant_email"`
	PasswordHash string
	Traits       kayanidentity.JSON

	// Active blocks future logins when false. Reversible and auditable,
	// distinct from deleting the row — the same shape as a tenant suspension,
	// scoped to one account instead of every account in it.
	Active bool `gorm:"default:true"`

	// TokenVersion is signed into every session this account is issued. See
	// LockAccount and RevokeAllSessions: incrementing it invalidates every
	// token issued before the increment, on every device, without this
	// deployment needing to know what those tokens are or where they are —
	// the server-side session store this whole design deliberately avoids.
	TokenVersion int
}

func (account *Account) GetID() any {
	return account.ID
}

func (account *Account) SetID(id any) {
	account.ID = id.(string)
}

// Account deliberately does NOT implement tenant.TenantAware.
//
// Login must look an account up by email before any session — and therefore any
// tenant context — exists. A scoped Account would make that lookup fail closed
// and no one could ever log in. Isolation for accounts is enforced explicitly
// in the auth service instead, which is the one place it can be.
