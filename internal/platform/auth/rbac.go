package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/getkayan/kayan/core/audit"
	"github.com/getkayan/kayan/core/rbac"
	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

// Nothing in this codebase limited what an authenticated caller could do
// inside its own tenant — any account or service account could reach every
// route requireAuth admitted. Kayan v0.3.0's core/rbac supplies hierarchical
// roles, wildcard permissions ("webhooks:*"), and inheritance; this file is
// the storage adapter and the tenant-scoping it needs, the same shape as
// every other Kayan integration in this package.
//
// **Not kayan-gorm's own RBACRepository.** It was inspected before writing
// this and turned out to have a real gap: GetIdentityRoles and GetRole issue
// no tenant_id predicate at all, despite role_definitions being keyed on
// (name, tenant_id) — an assignment or definition from one tenant is visible
// to a query from any other.
//
// **Also not this codebase's own isolation callback**, tried first and
// reverted: RoleAssignment/RoleDefinition implementing tenant.TenantAware and
// registered as scoped models looked right, but rbacRepo.AssignRole went
// through GORM's FirstOrCreate, and the created row came back with an empty
// tenant_id — the isolation callback's Create hook did not stamp it the way
// it does for a plain Create. Rather than chase exactly which GORM query
// shapes the callback does and does not intercept, every method here filters
// and stamps tenant_id explicitly, the same proven pattern ServiceAccount and
// the OIDC state store already use. RoleAssignment and RoleDefinition are
// listed in bootstrap's auditExemptTables for the identical reason
// ServiceAccount is: tenant isolation for them is enforced here, in
// application code, not by the database layer.

// RoleAssignment records that identityID holds role within one tenant.
type RoleAssignment struct {
	ID         uint   `gorm:"primaryKey;autoIncrement"`
	TenantID   string `gorm:"column:tenant_id;index"`
	IdentityID string `gorm:"index:idx_role_assignments_identity;not null"`
	Role       string `gorm:"index:idx_role_assignments_identity;not null"`
	CreatedAt  time.Time
}

func (RoleAssignment) TableName() string { return "role_assignments" }

// RoleDefinition is one tenant's definition of a role: what it grants, and
// what other roles it inherits from. Definitions are tenant-scoped, not
// platform-global — a tenant may define "billing_admin" however it needs to
// without touching any other tenant's catalog. See SeedDefaultRoles for the
// starter catalog every tenant gets.
type RoleDefinition struct {
	Name        string   `gorm:"primaryKey"`
	TenantID    string   `gorm:"column:tenant_id;primaryKey"`
	Permissions []string `gorm:"type:text;serializer:json"`
	Inherits    []string `gorm:"type:text;serializer:json"`
	Description string
}

func (RoleDefinition) TableName() string { return "role_definitions" }

// rbacRepo implements rbac.RBACStorage and rbac.RoleStore. Every method
// filters and stamps tenant_id explicitly from tenant.IDFromContext(ctx) —
// see the package comment above for why this is deliberate rather than
// isolation-callback-based.
type rbacRepo struct {
	db *gorm.DB
}

func (r *rbacRepo) GetIdentityRoles(ctx context.Context, identityID any) ([]string, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, ErrNoTenant
	}
	id := fmt.Sprintf("%v", identityID)
	var assignments []RoleAssignment
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND identity_id = ?", tenantID, id).
		Find(&assignments).Error
	if err != nil {
		return nil, fmt.Errorf("auth: get identity roles: %w", err)
	}
	roles := make([]string, len(assignments))
	for i, a := range assignments {
		roles[i] = a.Role
	}
	return roles, nil
}

// SetIdentityRoles replaces every role held by identityID within the tenant
// in ctx. Required to satisfy rbac.RBACStorage; AssignRole/RevokeRole below
// are the finer-grained surface this service actually uses.
func (r *rbacRepo) SetIdentityRoles(ctx context.Context, identityID any, roles []string) error {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return ErrNoTenant
	}
	id := fmt.Sprintf("%v", identityID)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ? AND identity_id = ?", tenantID, id).
			Delete(&RoleAssignment{}).Error; err != nil {
			return fmt.Errorf("auth: delete old roles: %w", err)
		}
		for _, role := range roles {
			assignment := RoleAssignment{TenantID: tenantID, IdentityID: id, Role: role, CreatedAt: time.Now()}
			if err := tx.Create(&assignment).Error; err != nil {
				return fmt.Errorf("auth: assign role %q: %w", role, err)
			}
		}
		return nil
	})
}

func (r *rbacRepo) GetRole(ctx context.Context, name string) (*rbac.Role, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, ErrNoTenant
	}
	var model RoleDefinition
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND name = ?", tenantID, name).
		First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %q", rbac.ErrRoleNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("auth: get role %q: %w", name, err)
	}
	return &rbac.Role{
		Name:        model.Name,
		Permissions: model.Permissions,
		Inherits:    model.Inherits,
		Description: model.Description,
	}, nil
}

func (r *rbacRepo) SaveRole(ctx context.Context, role *rbac.Role) error {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return ErrNoTenant
	}
	if role == nil || role.Name == "" {
		return errors.New("auth: role must have a name")
	}
	model := &RoleDefinition{
		Name:        role.Name,
		TenantID:    tenantID,
		Permissions: role.Permissions,
		Inherits:    role.Inherits,
		Description: role.Description,
	}
	if err := r.db.WithContext(ctx).Save(model).Error; err != nil {
		return fmt.Errorf("auth: save role %q: %w", role.Name, err)
	}
	return nil
}

func (r *rbacRepo) DeleteRole(ctx context.Context, name string) error {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return ErrNoTenant
	}
	return r.db.WithContext(ctx).
		Where("tenant_id = ? AND name = ?", tenantID, name).
		Delete(&RoleDefinition{}).Error
}

func (r *rbacRepo) ListRoles(ctx context.Context) ([]*rbac.Role, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, ErrNoTenant
	}
	var models []RoleDefinition
	if err := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Find(&models).Error; err != nil {
		return nil, fmt.Errorf("auth: list roles: %w", err)
	}
	roles := make([]*rbac.Role, 0, len(models))
	for _, model := range models {
		roles = append(roles, &rbac.Role{
			Name:        model.Name,
			Permissions: model.Permissions,
			Inherits:    model.Inherits,
			Description: model.Description,
		})
	}
	return roles, nil
}

var (
	_ rbac.RBACStorage = (*rbacRepo)(nil)
	_ rbac.RoleStore   = (*rbacRepo)(nil)
)

// DefaultRoleCatalog is what SeedDefaultRoles installs for a new tenant. It
// is deliberately small and kernel-scoped: modules that need their own
// permissions declare and check their own, using the same rbac.Manager this
// package exposes — this catalog only covers the resources the kernel itself
// owns (webhooks, service accounts, role management) so a tenant is
// immediately usable without an operator hand-authoring roles first.
var DefaultRoleCatalog = []rbac.Role{
	{
		Name: "owner",
		// "**" (WildcardSuffix), not "*": a bare "*" (WildcardSegment) matches
		// exactly one segment, so it would grant "webhooks" but not
		// "webhooks:write" — found the hard way, chasing a permission check
		// that resolved the role, found the assignment, and still denied.
		// "**" matches one or more remaining segments, which is the "every
		// kernel-managed resource" this role is meant to grant.
		Permissions: []string{"**"},
		Description: "Full access to every kernel-managed resource in this tenant.",
	},
	{
		Name:        "admin",
		Permissions: []string{"webhooks:*", "service_accounts:*", "roles:*"},
		Description: "Manages webhooks, service accounts, and role assignments.",
	},
	{
		Name:        "member",
		Permissions: []string{"webhooks:read", "service_accounts:read"},
		Description: "Ordinary authenticated member: read access to kernel-managed resources.",
	},
	{
		Name:        "viewer",
		Permissions: []string{},
		Description: "Authenticated, no kernel-management permissions.",
	},
}

// SeedDefaultRoles installs DefaultRoleCatalog for tenantID, skipping any
// role name that already has a definition — a tenant that customised
// "admin" must not have that customisation overwritten by a later call.
func (service *Service) SeedDefaultRoles(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return ErrNoTenant
	}
	for i := range DefaultRoleCatalog {
		role := DefaultRoleCatalog[i]
		var existing RoleDefinition
		err := service.db.WithContext(ctx).
			Where("tenant_id = ? AND name = ?", tenantID, role.Name).
			First(&existing).Error
		if err == nil {
			continue // already defined; a customisation is not overwritten.
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("auth: seed default roles: %w", err)
		}
		model := &RoleDefinition{
			Name:        role.Name,
			TenantID:    tenantID,
			Permissions: role.Permissions,
			Inherits:    role.Inherits,
			Description: role.Description,
		}
		if err := service.db.WithContext(ctx).Create(model).Error; err != nil {
			return fmt.Errorf("auth: seed role %q: %w", role.Name, err)
		}
	}
	return nil
}

// AssignRole grants role to identityID within the tenant in ctx. A duplicate
// assignment is not an error: granting a role someone already holds is a
// no-op, not a conflict a caller needs to handle specially.
func (service *Service) AssignRole(ctx context.Context, identityID, role string) error {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return ErrNoTenant
	}
	var existing RoleAssignment
	err := service.db.WithContext(ctx).
		Where("tenant_id = ? AND identity_id = ? AND role = ?", tenantID, identityID, role).
		First(&existing).Error
	if err == nil {
		return nil // already held.
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("auth: assign role %q to %s: %w", role, identityID, err)
	}
	assignment := &RoleAssignment{TenantID: tenantID, IdentityID: identityID, Role: role, CreatedAt: time.Now()}
	if err := service.db.WithContext(ctx).Create(assignment).Error; err != nil {
		return fmt.Errorf("auth: assign role %q to %s: %w", role, identityID, err)
	}
	caller, _ := modulekit.CallerFrom(ctx)
	service.logAudit(ctx, audit.EventRoleAssigned, tenantID, caller.Subject, identityID, true,
		fmt.Sprintf("role %q assigned", role))
	return nil
}

// RevokeRole removes role from identityID within the tenant in ctx.
func (service *Service) RevokeRole(ctx context.Context, identityID, role string) error {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return ErrNoTenant
	}
	err := service.db.WithContext(ctx).
		Where("tenant_id = ? AND identity_id = ? AND role = ?", tenantID, identityID, role).
		Delete(&RoleAssignment{}).Error
	if err != nil {
		return fmt.Errorf("auth: revoke role %q from %s: %w", role, identityID, err)
	}
	caller, _ := modulekit.CallerFrom(ctx)
	service.logAudit(ctx, audit.EventRoleRevoked, tenantID, caller.Subject, identityID, true,
		fmt.Sprintf("role %q revoked", role))
	return nil
}

// ListIdentityRoles returns the roles identityID holds within the tenant in
// ctx.
func (service *Service) ListIdentityRoles(ctx context.Context, identityID string) ([]string, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, ErrNoTenant
	}
	var assignments []RoleAssignment
	err := service.db.WithContext(ctx).
		Where("tenant_id = ? AND identity_id = ?", tenantID, identityID).
		Find(&assignments).Error
	if err != nil {
		return nil, fmt.Errorf("auth: list roles for %s: %w", identityID, err)
	}
	roles := make([]string, len(assignments))
	for i, a := range assignments {
		roles[i] = a.Role
	}
	return roles, nil
}

// HasPermission reports whether identityID holds permission within the
// tenant in ctx, resolving role inheritance and wildcards.
func (service *Service) HasPermission(ctx context.Context, identityID, permission string) (bool, error) {
	return service.rbac.AuthorizePermission(ctx, identityID, permission)
}

// ErrPermissionDenied is returned by RequirePermission when the check
// resolves to false rather than erroring — the caller is known and simply
// lacks the permission, as opposed to a broken role definition.
var ErrPermissionDenied = errors.New("auth: permission denied")

// RequirePermission is HasPermission with an error contract a caller can
// return directly from a request handler.
func (service *Service) RequirePermission(ctx context.Context, identityID, permission string) error {
	ok, err := service.HasPermission(ctx, identityID, permission)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s needs %q", ErrPermissionDenied, identityID, permission)
	}
	return nil
}
