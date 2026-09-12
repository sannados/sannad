package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sannados/sannad/internal/app/bootstrap"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/pkg/config"
)

// Before the tenant directory, any string reaching the edge was a working
// tenant. These tests cover the refusal, and the one deliberate exception.

// strictTenantApp builds an app with self-provisioning off, which is the
// default and the shape a deployment that provisions out of band runs.
func strictTenantApp(t *testing.T) *bootstrap.App {
	t.Helper()
	cfg := config.Config{
		Env:           "development",
		KernelAddr:    ":0",
		GatewayAddr:   ":0",
		DatabaseDSN:   filepath.Join(t.TempDir(), "tenant-edge-test.db"),
		SessionSecret: "tenant-edge-test-session-secret-long-enough",
		KayanIssuer:   "http://localhost:8080",
		CORSOrigins:   "*",
	}
	app, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(t.Context()) })
	return app
}

// TestRegisterIntoUnknownTenantIsRefused is the gap this closes. It used to
// succeed, creating data under an identifier nobody had provisioned.
func TestRegisterIntoUnknownTenantIsRefused(t *testing.T) {
	app := strictTenantApp(t)

	req := jsonRequest(t, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email":    "someone@example.com",
		"password": "correct-horse-battery-staple",
		"tenant":   "never-provisioned",
	})
	resp := doGateway(t, app, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("registering into an unprovisioned tenant returned %d, want 404", resp.StatusCode)
	}
}

// TestRegisterIntoProvisionedTenantSucceeds: the refusal must be about the
// tenant being unknown, not about the check breaking registration.
func TestRegisterIntoProvisionedTenantSucceeds(t *testing.T) {
	app := strictTenantApp(t)

	if err := app.Tenants().Provision(t.Context(), "tenant-real", "Real Tenant"); err != nil {
		t.Fatalf("provision: %v", err)
	}

	req := jsonRequest(t, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email":    "someone@example.com",
		"password": "correct-horse-battery-staple",
		"tenant":   "tenant-real",
	})
	resp := doGateway(t, app, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("registering into a provisioned tenant returned %d, want 201", resp.StatusCode)
	}
}

// TestLoginIntoSuspendedTenantIsRefused is what makes a suspension real. The
// account exists and the password is correct; the tenant is off.
func TestLoginIntoSuspendedTenantIsRefused(t *testing.T) {
	app := strictTenantApp(t)
	ctx := t.Context()

	if err := app.Tenants().Provision(ctx, "tenant-real", "Real Tenant"); err != nil {
		t.Fatalf("provision: %v", err)
	}

	register := jsonRequest(t, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email":    "someone@example.com",
		"password": "correct-horse-battery-staple",
		"tenant":   "tenant-real",
	})
	resp := doGateway(t, app, register)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: got %d, want 201", resp.StatusCode)
	}

	// Login works while the tenant is active.
	login := jsonRequest(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email":    "someone@example.com",
		"password": "correct-horse-battery-staple",
		"tenant":   "tenant-real",
	})
	resp = doGateway(t, app, login)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login before suspension: got %d, want 200", resp.StatusCode)
	}

	if err := app.Tenants().SetStatus(ctx, "tenant-real", tenancy.StatusSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	login = jsonRequest(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email":    "someone@example.com",
		"password": "correct-horse-battery-staple",
		"tenant":   "tenant-real",
	})
	resp = doGateway(t, app, login)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a suspended tenant's user still logged in; the suspension does nothing")
	}
}

// TestSelfProvisionCreatesTheTenant covers the opt-in path: a deployment that
// wants self-service signup turns this on knowingly.
func TestSelfProvisionCreatesTheTenant(t *testing.T) {
	cfg := config.Config{
		Env:                 "development",
		KernelAddr:          ":0",
		GatewayAddr:         ":0",
		DatabaseDSN:         filepath.Join(t.TempDir(), "self-provision-test.db"),
		SessionSecret:       "self-provision-test-session-secret-long",
		KayanIssuer:         "http://localhost:8080",
		CORSOrigins:         "*",
		TenantSelfProvision: true,
	}
	app, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(t.Context()) })

	req := jsonRequest(t, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email":    "founder@example.com",
		"password": "correct-horse-battery-staple",
		"tenant":   "brand-new",
	})
	resp := doGateway(t, app, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("self-service registration returned %d, want 201", resp.StatusCode)
	}

	// The tenant must now exist for real, not merely have been waved through.
	if err := app.Tenants().Check(t.Context(), "brand-new"); err != nil {
		t.Fatalf("self-provisioned tenant is not in the directory: %v", err)
	}
}

// TestSelfProvisionDoesNotReviveASuspendedTenant: the opt-in creates unknown
// tenants, and must never override an operator's suspension.
func TestSelfProvisionDoesNotReviveASuspendedTenant(t *testing.T) {
	cfg := config.Config{
		Env:                 "development",
		KernelAddr:          ":0",
		GatewayAddr:         ":0",
		DatabaseDSN:         filepath.Join(t.TempDir(), "self-provision-suspended.db"),
		SessionSecret:       "self-provision-test-session-secret-long",
		KayanIssuer:         "http://localhost:8080",
		CORSOrigins:         "*",
		TenantSelfProvision: true,
	}
	app, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(t.Context()) })

	ctx := t.Context()
	if err := app.Tenants().Provision(ctx, "paused", "Paused"); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := app.Tenants().SetStatus(ctx, "paused", tenancy.StatusSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	req := jsonRequest(t, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email":    "someone@example.com",
		"password": "correct-horse-battery-staple",
		"tenant":   "paused",
	})
	resp := doGateway(t, app, req)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("registration revived a suspended tenant")
	}
}

// jsonRequest builds a JSON POST for the gateway test harness.
func jsonRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, jsonBody(t, body))
	req.Header.Set("Content-Type", "application/json")
	return req
}
