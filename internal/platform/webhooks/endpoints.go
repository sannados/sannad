package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/sannados/sannad/pkg/modulekit"
)

// Endpoint registration existed only as a model — Enqueue and DrainOnce read
// rows nothing wrote. This is the management surface: an integrator's own
// path for registering a receiver, and an operator's path for pausing,
// rotating, or removing one.
//
// Every method here goes through the store, whose isolation callbacks read
// the tenant from ctx (see internal/platform/tenancy). There is no tenant
// parameter to any of them: passing one would be a second, forgeable path to
// the same thing the context already carries, and ADR 0005 exists precisely
// so that path does not exist.

// CreateEndpointRequest is what a caller supplies. TenantID and Secret are
// deliberately absent: the tenant comes from context, and the secret is
// generated here rather than accepted from a caller, because a caller-chosen
// secret could be short, reused across endpoints, or simply forgotten to be
// random.
type CreateEndpointRequest struct {
	// URL is where deliveries are POSTed. Required.
	URL string

	// Events is the comma-separated event list this endpoint receives. Empty
	// means every event in the tenant — see Endpoint.Events.
	Events string
}

// CreateEndpoint registers a new endpoint and returns it with its generated
// secret. The secret is returned exactly once: it is not otherwise
// retrievable, the same rule most API key systems apply, because a value
// that can be re-read is a value that can be read by whoever compromises the
// datastore later rather than only whoever had it at creation time.
func (r *Runner) CreateEndpoint(ctx context.Context, req CreateEndpointRequest) (Endpoint, error) {
	if strings.TrimSpace(req.URL) == "" {
		return Endpoint{}, fmt.Errorf("webhooks: endpoint URL is required")
	}
	if !strings.HasPrefix(req.URL, "https://") && !strings.HasPrefix(req.URL, "http://") {
		return Endpoint{}, fmt.Errorf("webhooks: endpoint URL must be http(s)")
	}

	secret, err := generateSecret()
	if err != nil {
		return Endpoint{}, fmt.Errorf("webhooks: generate secret: %w", err)
	}

	now := r.now()
	endpoint := Endpoint{
		ID:        uuid.NewString(),
		URL:       req.URL,
		Events:    req.Events,
		Secret:    secret,
		Active:    true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.store.Create(ctx, &endpoint); err != nil {
		return Endpoint{}, fmt.Errorf("webhooks: create endpoint: %w", err)
	}
	return endpoint, nil
}

// ListEndpoints returns every endpoint for the tenant in context.
func (r *Runner) ListEndpoints(ctx context.Context) ([]Endpoint, error) {
	var endpoints []Endpoint
	if err := r.store.Find(ctx, &endpoints); err != nil {
		return nil, fmt.Errorf("webhooks: list endpoints: %w", err)
	}
	return endpoints, nil
}

// GetEndpoint returns one endpoint. Its secret is included: an integrator who
// lost it has no other way to re-verify what they configured, and unlike
// creation this is a read the tenant is already authorized for by owning the
// endpoint.
func (r *Runner) GetEndpoint(ctx context.Context, id string) (Endpoint, error) {
	var endpoint Endpoint
	if err := r.store.First(ctx, &endpoint, modulekit.Where("id", id)); err != nil {
		return Endpoint{}, fmt.Errorf("webhooks: get endpoint %s: %w", id, err)
	}
	return endpoint, nil
}

// UpdateEndpointRequest carries the fields a caller may change. A pointer
// field left nil is left unchanged, so a partial update cannot accidentally
// clear a field the caller never meant to touch.
type UpdateEndpointRequest struct {
	URL    *string
	Events *string
	Active *bool
}

// UpdateEndpoint applies a partial update to an endpoint already owned by the
// tenant in context — the isolation callbacks refuse the write outright if it
// is not.
func (r *Runner) UpdateEndpoint(ctx context.Context, id string, req UpdateEndpointRequest) error {
	changes := map[string]any{"updated_at": r.now()}
	if req.URL != nil {
		if strings.TrimSpace(*req.URL) == "" {
			return fmt.Errorf("webhooks: endpoint URL cannot be empty")
		}
		changes["url"] = *req.URL
	}
	if req.Events != nil {
		changes["events"] = *req.Events
	}
	if req.Active != nil {
		changes["active"] = *req.Active
	}

	if err := r.store.Update(ctx, &Endpoint{}, changes, modulekit.Where("id", id)); err != nil {
		return fmt.Errorf("webhooks: update endpoint %s: %w", id, err)
	}
	return nil
}

// RotateSecret replaces an endpoint's signing secret and returns the new one.
//
// The table-stakes response to a leaked secret: an integrator who suspects
// theirs was exposed needs a way to invalidate it without deleting and
// re-registering the endpoint, which would also lose its delivery history.
func (r *Runner) RotateSecret(ctx context.Context, id string) (string, error) {
	secret, err := generateSecret()
	if err != nil {
		return "", fmt.Errorf("webhooks: generate secret: %w", err)
	}
	changes := map[string]any{
		"secret":     secret,
		"updated_at": r.now(),
	}
	if err := r.store.Update(ctx, &Endpoint{}, changes, modulekit.Where("id", id)); err != nil {
		return "", fmt.Errorf("webhooks: rotate secret for %s: %w", id, err)
	}
	return secret, nil
}

// DeleteEndpoint removes an endpoint. Its delivery history is left in place:
// "which events did this integration miss before it was removed" remains
// answerable, the same reasoning that keeps dead deliveries around after
// exhausting retries (see Delivery's doc comment).
func (r *Runner) DeleteEndpoint(ctx context.Context, id string) error {
	if err := r.store.Delete(ctx, &Endpoint{}, modulekit.Where("id", id)); err != nil {
		return fmt.Errorf("webhooks: delete endpoint %s: %w", id, err)
	}
	return nil
}

// ListDeliveries returns an endpoint's recent deliveries, most recent first —
// the question an integrator debugging a missing webhook actually asks.
//
// modulekit.Store has no ORDER BY or LIMIT clause — the query surface is
// deliberately small (see Store's doc comment) — so ordering and truncation
// happen here, over what is in practice a bounded result set: one endpoint's
// deliveries, not the whole table.
func (r *Runner) ListDeliveries(ctx context.Context, endpointID string, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var deliveries []Delivery
	if err := r.store.Find(ctx, &deliveries, modulekit.Where("endpoint_id", endpointID)); err != nil {
		return nil, fmt.Errorf("webhooks: list deliveries for %s: %w", endpointID, err)
	}

	sort.Slice(deliveries, func(i, j int) bool {
		return deliveries[i].CreatedAt.After(deliveries[j].CreatedAt)
	})
	if len(deliveries) > limit {
		deliveries = deliveries[:limit]
	}
	return deliveries, nil
}

func generateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "whsec_" + hex.EncodeToString(buf), nil
}
