package crm

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/sannados/sannad/pkg/modulekit"
)

// MountRoutes mounts the CRM gateway endpoints. Pass it to
// App.RegisterAuthenticatedRoutes from an entrypoint.
//
// It lives beside the module rather than in bootstrap because an HTTP endpoint
// is a projection of one module's contract. The kernel supplies the router and
// the auth middleware; what is exposed is the module's decision.
func MountRoutes(caller modulekit.Caller) func(fiber.Router, fiber.Handler) {
	return func(router fiber.Router, requireAuth fiber.Handler) {
		router.Get("/crm/contacts", requireAuth, handleListContactsHTTP(caller))
	}
}

func handleListContactsHTTP(caller modulekit.Caller) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		// No tenant is read from the request. It travels in the context, placed
		// there by requireAuth from the signed token, and the storage layer
		// scopes the query to it.
		result, err := caller.Call(ctx.UserContext(), CapabilityContactReader, ContactReaderRequest{})
		if err != nil {
			if errors.Is(err, modulekit.ErrForbidden) {
				return ctx.Status(fiber.StatusForbidden).JSON(map[string]string{
					"error": "access to this tenant is not allowed",
				})
			}
			return ctx.Status(fiber.StatusInternalServerError).JSON(map[string]string{
				"error": err.Error(),
			})
		}
		return ctx.Status(fiber.StatusOK).JSON(result)
	}
}
