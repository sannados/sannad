package webhooks_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sannados/sannad/internal/platform/webhooks"
	"github.com/sannados/sannad/pkg/modulekit"
)

// TestCreateEndpointGeneratesARandomSecret: the secret must not be caller
// input — a caller-chosen secret could be short, reused, or simply guessable,
// which defeats the point of signing deliveries at all.
func TestCreateEndpointGeneratesARandomSecret(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)
	ctx := tenantContext("acme")

	first, err := runner.CreateEndpoint(ctx, webhooks.CreateEndpointRequest{URL: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(first.Secret, "whsec_") || len(first.Secret) < 20 {
		t.Fatalf("secret %q does not look randomly generated", first.Secret)
	}

	second, err := runner.CreateEndpoint(ctx, webhooks.CreateEndpointRequest{URL: "https://example.com/hook2"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if first.Secret == second.Secret {
		t.Fatal("two endpoints got the same secret")
	}
}

// TestCreateEndpointRejectsAMissingOrInvalidURL: table stakes validation — an
// endpoint with nowhere to deliver to is a misconfiguration caught here,
// rather than surfacing as silent DrainOnce failures forever.
func TestCreateEndpointRejectsAMissingOrInvalidURL(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)
	ctx := tenantContext("acme")

	if _, err := runner.CreateEndpoint(ctx, webhooks.CreateEndpointRequest{}); err == nil {
		t.Fatal("an endpoint with no URL was accepted")
	}
	if _, err := runner.CreateEndpoint(ctx, webhooks.CreateEndpointRequest{URL: "not-a-url"}); err == nil {
		t.Fatal("an endpoint with a non-http(s) URL was accepted")
	}
}

// TestListEndpointsIsTenantScoped: the isolation callbacks, exercised through
// the management surface rather than only through Enqueue/DrainOnce — an
// integrator listing their own endpoints must never see another tenant's.
func TestListEndpointsIsTenantScoped(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)

	if _, err := runner.CreateEndpoint(tenantContext("acme"), webhooks.CreateEndpointRequest{URL: "https://acme.example/hook"}); err != nil {
		t.Fatalf("create acme endpoint: %v", err)
	}
	if _, err := runner.CreateEndpoint(tenantContext("globex"), webhooks.CreateEndpointRequest{URL: "https://globex.example/hook"}); err != nil {
		t.Fatalf("create globex endpoint: %v", err)
	}

	acmeEndpoints, err := runner.ListEndpoints(tenantContext("acme"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(acmeEndpoints) != 1 || acmeEndpoints[0].URL != "https://acme.example/hook" {
		t.Fatalf("acme saw %+v, want only its own endpoint", acmeEndpoints)
	}
}

// TestGetEndpointAcrossTenantsIsNotFound: reading another tenant's endpoint by
// ID must fail the same way a nonexistent ID does — a distinguishable error
// would leak whether the ID exists at all.
func TestGetEndpointAcrossTenantsIsNotFound(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)

	endpoint, err := runner.CreateEndpoint(tenantContext("acme"), webhooks.CreateEndpointRequest{URL: "https://acme.example/hook"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = runner.GetEndpoint(tenantContext("globex"), endpoint.ID)
	if !errors.Is(err, modulekit.ErrNotFound) {
		t.Fatalf("expected ErrNotFound reading another tenant's endpoint, got %v", err)
	}
}

// TestUpdateEndpointLeavesUnsetFieldsUnchanged: a partial update must not
// clear a field the caller never mentioned — the reason UpdateEndpointRequest
// uses pointers rather than plain values.
func TestUpdateEndpointLeavesUnsetFieldsUnchanged(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)
	ctx := tenantContext("acme")

	endpoint, err := runner.CreateEndpoint(ctx, webhooks.CreateEndpointRequest{
		URL:    "https://example.com/hook",
		Events: "orders.created",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	inactive := false
	if err := runner.UpdateEndpoint(ctx, endpoint.ID, webhooks.UpdateEndpointRequest{Active: &inactive}); err != nil {
		t.Fatalf("update: %v", err)
	}

	updated, err := runner.GetEndpoint(ctx, endpoint.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Active {
		t.Fatal("Active was not applied")
	}
	if updated.Events != "orders.created" {
		t.Fatalf("Events changed to %q despite not being in the update", updated.Events)
	}
	if updated.URL != "https://example.com/hook" {
		t.Fatalf("URL changed to %q despite not being in the update", updated.URL)
	}
}

// TestRotateSecretChangesTheSigningSecret: the table-stakes response to a
// leaked secret, and the delivered signature must reflect it immediately.
func TestRotateSecretChangesTheSigningSecret(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)
	ctx := tenantContext("acme")

	endpoint, err := runner.CreateEndpoint(ctx, webhooks.CreateEndpointRequest{URL: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newSecret, err := runner.RotateSecret(ctx, endpoint.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newSecret == endpoint.Secret {
		t.Fatal("rotation returned the same secret")
	}

	reloaded, err := runner.GetEndpoint(ctx, endpoint.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reloaded.Secret != newSecret {
		t.Fatal("the stored secret does not match what rotation returned")
	}
}

// TestDeleteEndpointLeavesDeliveryHistory: history answers "what did this
// integration miss" after the endpoint itself is gone, the same reasoning
// that keeps dead deliveries around after exhausting retries.
func TestDeleteEndpointLeavesDeliveryHistory(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)
	ctx := tenantContext("acme")

	endpoint, err := runner.CreateEndpoint(ctx, webhooks.CreateEndpointRequest{URL: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := runner.Enqueue(ctx, busEvent("acme", "orders.created", 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if err := runner.DeleteEndpoint(ctx, endpoint.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	deliveries, err := runner.ListDeliveries(ctx, endpoint.ID, 0)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("got %d deliveries, want the pre-deletion delivery preserved", len(deliveries))
	}

	if _, err := runner.GetEndpoint(ctx, endpoint.ID); !errors.Is(err, modulekit.ErrNotFound) {
		t.Fatalf("expected the endpoint itself to be gone, got %v", err)
	}
}

// TestListDeliveriesOrdersMostRecentFirst: the question an integrator
// debugging a missing webhook actually asks first.
func TestListDeliveriesOrdersMostRecentFirst(t *testing.T) {
	store, _ := runnerStore(t)
	runner := webhooks.NewRunner(store)
	ctx := tenantContext("acme")

	endpoint, err := runner.CreateEndpoint(ctx, webhooks.CreateEndpointRequest{URL: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := runner.Enqueue(ctx, busEvent("acme", "orders.created", 1)); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if err := runner.Enqueue(ctx, busEvent("acme", "orders.updated", 2)); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}

	deliveries, err := runner.ListDeliveries(ctx, endpoint.ID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("got %d deliveries, want 2", len(deliveries))
	}
	if deliveries[0].CreatedAt.Before(deliveries[1].CreatedAt) {
		t.Fatal("deliveries are not ordered most-recent-first")
	}
}
