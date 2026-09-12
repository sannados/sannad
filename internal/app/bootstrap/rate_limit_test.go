package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sannados/sannad/internal/app/bootstrap"
	"github.com/sannados/sannad/pkg/config"
)

// gatewayAppWithTenantLimit is gatewayApp with a low, deterministic
// per-tenant rate limit instead of the production default — a test cannot
// afford to fire 300 requests to prove the limit exists.
func gatewayAppWithTenantLimit(t *testing.T, max int) *bootstrap.App {
	t.Helper()
	cfg := config.Config{
		Env:                 "development",
		KernelAddr:          ":0",
		GatewayAddr:         ":0",
		DatabaseDSN:         filepath.Join(t.TempDir(), "gateway-test.db"),
		SessionSecret:       "gateway-test-session-secret-long-enough",
		KayanIssuer:         "http://localhost:8080",
		CORSOrigins:         "*",
		TenantSelfProvision: true,
		RateLimitPerTenant:  max,
	}
	app, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() {
		_ = app.Shutdown(t.Context())
	})
	return app
}

// TestGateway_PerTenantRateLimitBlocksAfterMax: an account exhausting its
// own budget gets 429, not silently throttled or ignored.
func TestGateway_PerTenantRateLimitBlocksAfterMax(t *testing.T) {
	app := gatewayAppWithTenantLimit(t, 3)
	token := registerAndLogin(t, app, "acme", "owner@example.com", "StrongPass1234!")

	var lastStatus int
	for range 5 {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		lastStatus = doGateway(t, app, req).StatusCode
	}
	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("got %d after exceeding the per-tenant limit, want 429", lastStatus)
	}
}

// TestGateway_PerTenantRateLimitIsIsolatedPerTenant: one tenant burning
// through its own budget must not affect another tenant's requests — the
// whole point of keying by tenant+subject instead of by IP, since both
// requests in this test share the same client IP in httptest.
func TestGateway_PerTenantRateLimitIsIsolatedPerTenant(t *testing.T) {
	app := gatewayAppWithTenantLimit(t, 2)
	exhaustedToken := registerAndLogin(t, app, "acme", "owner@example.com", "StrongPass1234!")
	otherToken := registerAndLogin(t, app, "globex", "owner@example.com", "StrongPass1234!")

	for range 4 {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+exhaustedToken)
		doGateway(t, app, req)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+otherToken)
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a fresh tenant's first request got %d, want 200 — one tenant's limit leaked into another's", resp.StatusCode)
	}
}
