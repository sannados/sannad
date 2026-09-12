package bootstrap

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/sannados/sannad/internal/platform/webhooks"
	"github.com/sannados/sannad/pkg/modulekit"
)

// The webhook endpoint management surface: an integrator's own path for
// registering and inspecting a receiver, and an operator's path for pausing,
// rotating, or replaying one. Every handler reads the tenant from
// ctx.UserContext() — the signed-token context requireAuth already
// established — and never from anything a caller supplied, the same rule
// every other authenticated route in this file follows.

type createWebhookEndpointPayload struct {
	URL    string `json:"url"`
	Events string `json:"events,omitempty"`
}

func (app *App) handleCreateWebhookEndpoint(ctx *fiber.Ctx) error {
	var payload createWebhookEndpointPayload
	if err := ctx.BodyParser(&payload); err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}

	endpoint, err := app.Webhooks.CreateEndpoint(ctx.UserContext(), webhooks.CreateEndpointRequest{
		URL:    payload.URL,
		Events: payload.Events,
	})
	if err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}
	// The secret is returned exactly once, on creation — see CreateEndpoint's
	// doc comment. Every other response omits it.
	return ctx.Status(fiber.StatusCreated).JSON(endpoint)
}

func (app *App) handleListWebhookEndpoints(ctx *fiber.Ctx) error {
	endpoints, err := app.Webhooks.ListEndpoints(ctx.UserContext())
	if err != nil {
		return writeError(ctx, fiber.StatusInternalServerError, err)
	}
	redactSecrets(endpoints)
	return ctx.Status(fiber.StatusOK).JSON(endpoints)
}

func (app *App) handleGetWebhookEndpoint(ctx *fiber.Ctx) error {
	endpoint, err := app.Webhooks.GetEndpoint(ctx.UserContext(), ctx.Params("id"))
	if err != nil {
		return writeWebhookError(ctx, err)
	}
	endpoint.Secret = ""
	return ctx.Status(fiber.StatusOK).JSON(endpoint)
}

type updateWebhookEndpointPayload struct {
	URL    *string `json:"url,omitempty"`
	Events *string `json:"events,omitempty"`
	Active *bool   `json:"active,omitempty"`
}

func (app *App) handleUpdateWebhookEndpoint(ctx *fiber.Ctx) error {
	var payload updateWebhookEndpointPayload
	if err := ctx.BodyParser(&payload); err != nil {
		return writeError(ctx, fiber.StatusBadRequest, err)
	}

	err := app.Webhooks.UpdateEndpoint(ctx.UserContext(), ctx.Params("id"), webhooks.UpdateEndpointRequest{
		URL:    payload.URL,
		Events: payload.Events,
		Active: payload.Active,
	})
	if err != nil {
		return writeWebhookError(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (app *App) handleDeleteWebhookEndpoint(ctx *fiber.Ctx) error {
	if err := app.Webhooks.DeleteEndpoint(ctx.UserContext(), ctx.Params("id")); err != nil {
		return writeWebhookError(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (app *App) handleRotateWebhookSecret(ctx *fiber.Ctx) error {
	secret, err := app.Webhooks.RotateSecret(ctx.UserContext(), ctx.Params("id"))
	if err != nil {
		return writeWebhookError(ctx, err)
	}
	return ctx.Status(fiber.StatusOK).JSON(map[string]string{"secret": secret})
}

func (app *App) handleReplayWebhookEndpoint(ctx *fiber.Ctx) error {
	n, err := app.Webhooks.ReplayEndpoint(ctx.UserContext(), ctx.Params("id"))
	if err != nil {
		return writeWebhookError(ctx, err)
	}
	return ctx.Status(fiber.StatusOK).JSON(map[string]int{"requeued": n})
}

func (app *App) handleReplayWebhookDelivery(ctx *fiber.Ctx) error {
	if err := app.Webhooks.Replay(ctx.UserContext(), ctx.Params("id")); err != nil {
		return writeWebhookError(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (app *App) handleListWebhookDeliveries(ctx *fiber.Ctx) error {
	deliveries, err := app.Webhooks.ListDeliveries(ctx.UserContext(), ctx.Params("id"), ctx.QueryInt("limit", 0))
	if err != nil {
		return writeWebhookError(ctx, err)
	}
	return ctx.Status(fiber.StatusOK).JSON(deliveries)
}

// writeWebhookError maps a not-found row to 404 rather than the runner's
// wrapped "get endpoint X: record not found" reaching the client as a 500. A
// missing endpoint under this tenant is an ordinary client error, not an
// operational one.
func writeWebhookError(ctx *fiber.Ctx, err error) error {
	if errors.Is(err, modulekit.ErrNotFound) {
		return writeError(ctx, fiber.StatusNotFound, err)
	}
	return writeError(ctx, fiber.StatusBadRequest, err)
}

// redactSecrets clears every endpoint's secret before a list response. A
// list is the shape most likely to be logged, screen-shared, or piped
// somewhere a single GetEndpoint response would not be.
func redactSecrets(endpoints []webhooks.Endpoint) {
	for i := range endpoints {
		endpoints[i].Secret = ""
	}
}
