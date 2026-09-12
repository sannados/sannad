package bootstrap

import (
	"errors"

	"github.com/getkayan/kayan/core/audit"
	"github.com/gofiber/fiber/v2"
	"github.com/sannados/sannad/internal/platform/auth"
	"github.com/sannados/sannad/pkg/modulekit"
)

// Role management and the audit trail. Every handler reads the tenant and
// caller from ctx.UserContext(), the same signed-token context every other
// authenticated route uses — an identity ID in the path names *whose* roles
// are being changed, never which tenant the change happens in.

type assignRolePayload struct {
	Role string `json:"role"`
}

func (app *App) handleAssignRole(ctx *fiber.Ctx) error {
	var payload assignRolePayload
	if err := ctx.BodyParser(&payload); err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}
	if payload.Role == "" {
		return writeError(ctx, fiber.StatusBadRequest, fiber.NewError(fiber.StatusBadRequest, "role is required"))
	}
	if err := app.Auth.AssignRole(ctx.UserContext(), ctx.Params("id"), payload.Role); err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (app *App) handleRevokeRole(ctx *fiber.Ctx) error {
	if err := app.Auth.RevokeRole(ctx.UserContext(), ctx.Params("id"), ctx.Params("role")); err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (app *App) handleListIdentityRoles(ctx *fiber.Ctx) error {
	roles, err := app.Auth.ListIdentityRoles(ctx.UserContext(), ctx.Params("id"))
	if err != nil {
		return writeError(ctx, fiber.StatusInternalServerError, err)
	}
	return ctx.Status(fiber.StatusOK).JSON(roles)
}

// handleListAuditEvents queries the audit trail for the caller's own tenant.
// The tenant filter is set here, from the signed token, never accepted as a
// query parameter — an audit endpoint that trusted a caller-supplied tenant
// would let any authenticated caller read another tenant's compliance
// history, which is a worse leak than an ordinary cross-tenant data read.
func (app *App) handleListAuditEvents(ctx *fiber.Ctx) error {
	caller, _ := modulekit.CallerFrom(ctx.UserContext())
	filter := audit.Filter{
		TenantID: caller.TenantID,
		Limit:    queryIntDefault(ctx, "limit", 50),
		OrderBy:  "-created_at",
	}
	if actor := ctx.Query("actor_id"); actor != "" {
		filter.ActorID = actor
	}
	if t := ctx.Query("type"); t != "" {
		filter.Types = []string{t}
	}
	events, err := app.Auth.QueryAuditEvents(ctx.UserContext(), filter)
	if err != nil {
		return writeError(ctx, fiber.StatusInternalServerError, err)
	}
	return ctx.Status(fiber.StatusOK).JSON(events)
}

func queryIntDefault(ctx *fiber.Ctx, key string, def int) int {
	v := ctx.QueryInt(key, def)
	if v <= 0 || v > 500 {
		return def
	}
	return v
}

// Admin operations on a single account: lock, unlock, and force logout
// everywhere. See internal/platform/auth/admin.go for what each does and
// why locking also revokes existing sessions rather than only blocking new
// logins.

func (app *App) handleLockAccount(ctx *fiber.Ctx) error {
	if err := app.Auth.LockAccount(ctx.UserContext(), ctx.Params("id")); err != nil {
		return writeAdminError(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (app *App) handleUnlockAccount(ctx *fiber.Ctx) error {
	if err := app.Auth.UnlockAccount(ctx.UserContext(), ctx.Params("id")); err != nil {
		return writeAdminError(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (app *App) handleRevokeAllSessions(ctx *fiber.Ctx) error {
	if err := app.Auth.RevokeAllSessions(ctx.UserContext(), ctx.Params("id")); err != nil {
		return writeAdminError(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// handleDeleteAccount erases another account's personal data within the
// caller's own tenant — the operator-initiated counterpart to
// handleDeleteMe, for handling a data subject access request on behalf of
// someone who can no longer sign in to make it themselves.
func (app *App) handleDeleteAccount(ctx *fiber.Ctx) error {
	if err := app.Auth.DeleteAccount(ctx.UserContext(), ctx.Params("id")); err != nil {
		return writeAdminError(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func writeAdminError(ctx *fiber.Ctx, err error) error {
	if errors.Is(err, auth.ErrNotFound) {
		return writeError(ctx, fiber.StatusNotFound, err)
	}
	return writeError(ctx, fiber.StatusBadRequest, err)
}
