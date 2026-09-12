package bootstrap

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/sannados/sannad/internal/platform/auth"
)

// Service account management and login. A service account is a machine
// identity that authenticates like any account and receives the same
// signed, tenant-bound session a human login does — see
// internal/platform/auth/service_account.go's package comment for why this
// closes the "service identity, and service-to-service tenancy" item ADR
// 0005 deferred.

type serviceLoginPayload struct {
	APIKey string `json:"api_key"`
}

func (app *App) handleServiceLogin(ctx *fiber.Ctx) error {
	var payload serviceLoginPayload
	if err := ctx.BodyParser(&payload); err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}
	if payload.APIKey == "" {
		return writeError(ctx, fiber.StatusBadRequest, errors.New("api_key is required"))
	}

	result, err := app.Auth.LoginWithAPIKey(ctx.UserContext(), payload.APIKey)
	if err != nil {
		// Intentionally vague, the same rule handleLogin follows: do not
		// reveal whether a key exists versus is merely wrong or revoked.
		return writeError(ctx, fiber.StatusUnauthorized, errors.New("invalid or revoked api key"))
	}
	return ctx.Status(fiber.StatusOK).JSON(result)
}

type createServiceAccountPayload struct {
	Name string `json:"name"`
}

func (app *App) handleCreateServiceAccount(ctx *fiber.Ctx) error {
	var payload createServiceAccountPayload
	if err := ctx.BodyParser(&payload); err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}

	account, rawKey, err := app.Auth.CreateServiceAccount(ctx.UserContext(), payload.Name)
	if err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}
	// The raw key is returned exactly once, here — see CreateServiceAccount's
	// own doc comment. The response embeds it alongside the account rather
	// than making the caller correlate two responses.
	return ctx.Status(fiber.StatusCreated).JSON(map[string]any{
		"service_account": account,
		"api_key":         rawKey,
	})
}

func (app *App) handleListServiceAccounts(ctx *fiber.Ctx) error {
	accounts, err := app.Auth.ListServiceAccounts(ctx.UserContext())
	if err != nil {
		return writeError(ctx, fiber.StatusInternalServerError, err)
	}
	return ctx.Status(fiber.StatusOK).JSON(accounts)
}

func (app *App) handleRevokeServiceAccount(ctx *fiber.Ctx) error {
	if err := app.Auth.RevokeServiceAccount(ctx.UserContext(), ctx.Params("id")); err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			return writeError(ctx, fiber.StatusNotFound, err)
		}
		return writeError(ctx, fiber.StatusBadRequest, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}
