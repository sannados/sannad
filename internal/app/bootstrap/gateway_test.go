package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sannados/sannad/examples/embedded/crm"
	"github.com/sannados/sannad/internal/app/bootstrap"
	"github.com/sannados/sannad/pkg/config"
)

func gatewayApp(t *testing.T) *bootstrap.App {
	t.Helper()
	cfg := config.Config{
		Env:         "development",
		KernelAddr:  ":0",
		GatewayAddr: ":0",
		// A file-backed database, not ":memory:": registration writes across more
		// than one pooled connection, and each would otherwise get its own
		// private database.
		DatabaseDSN:   filepath.Join(t.TempDir(), "gateway-test.db"),
		SessionSecret: "gateway-test-session-secret-long-enough",
		KayanIssuer:   "http://localhost:8080",
		CORSOrigins:   "*",
		// These tests register into tenants nobody provisioned, which is the
		// self-service signup shape. A deployment that provisions out of band
		// leaves this off and an unknown tenant is refused.
		TenantSelfProvision: true,
	}
	app, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	// Wire the example module exactly as an entrypoint does: scoped model,
	// module, routes. The kernel names none of these.
	if err := app.RegisterScopedModels(&crm.Contact{}); err != nil {
		t.Fatalf("RegisterScopedModels: %v", err)
	}
	if err := app.RegisterModule(crm.New(app.Store(), cfg.Env)); err != nil {
		t.Fatalf("RegisterModule crm: %v", err)
	}
	app.RegisterAuthenticatedRoutes(crm.MountRoutes(app.Bus))
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() {
		_ = app.Shutdown(t.Context())
	})
	return app
}

func doGateway(t *testing.T, app *bootstrap.App, req *http.Request) *http.Response {
	t.Helper()
	resp, err := app.TestGateway(req)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	return resp
}

func jsonBody(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}

func TestGateway_Register(t *testing.T) {
	app := gatewayApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", jsonBody(t, map[string]string{
		"tenant":   "demo",
		"email":    "alice@example.com",
		"password": "StrongPass1234!",
	}))
	req.Header.Set("Content-Type", "application/json")
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("register: got %d, want %d", resp.StatusCode, http.StatusCreated)
	}
}

func TestGateway_Login(t *testing.T) {
	app := gatewayApp(t)
	creds := map[string]string{"tenant": "demo", "email": "bob@example.com", "password": "StrongPass1234!"}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", jsonBody(t, creds))
	req.Header.Set("Content-Type", "application/json")
	doGateway(t, app, req)

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", jsonBody(t, creds))
	req2.Header.Set("Content-Type", "application/json")
	resp := doGateway(t, app, req2)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if body["access_token"] == "" {
		t.Error("expected non-empty access_token in login response")
	}
}

func TestGateway_Me_RequiresAuth(t *testing.T) {
	app := gatewayApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/me without token: got %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestGateway_Me_WithValidToken(t *testing.T) {
	app := gatewayApp(t)
	token := registerAndLogin(t, app, "demo", "carol@example.com", "StrongPass1234!")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/me: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode me response: %v", err)
	}
	if body["subject"] == "" {
		t.Error("expected non-empty subject in /me response")
	}
}

func TestGateway_CRMContacts_RequiresAuth(t *testing.T) {
	app := gatewayApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/crm/contacts", nil)
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/crm/contacts without token: got %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestGateway_CRMContacts_ReturnsContacts(t *testing.T) {
	app := gatewayApp(t)
	token := registerAndLogin(t, app, "demo", "dave@example.com", "StrongPass1234!")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/crm/contacts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/crm/contacts: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body struct {
		Contacts []map[string]string `json:"contacts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode contacts response: %v", err)
	}
	if len(body.Contacts) == 0 {
		t.Error("expected at least one seeded demo contact")
	}
}

// TestGateway_CRMContacts_CannotReachAnotherTenant is the end-to-end form of
// the defect ADR 0005 describes. A caller authenticated into "demo" asks for
// "enterprise" data using every input the old code trusted: the X-Tenant-ID
// header and the ?tenant= query parameter.
//
// Both are now ignored — the tenant comes from the signed token — so the
// request succeeds and returns the caller's own rows. The assertion is on the
// rows themselves, not the status code: a 200 carrying another tenant's
// contacts is the exact failure being guarded against.
func TestGateway_CRMContacts_CannotReachAnotherTenant(t *testing.T) {
	app := gatewayApp(t)
	token := registerAndLogin(t, app, "demo", "eve@example.com", "StrongPass1234!")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/crm/contacts?tenant=enterprise", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Tenant-ID", "enterprise")
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/crm/contacts: got %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body struct {
		Contacts []map[string]string `json:"contacts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode contacts response: %v", err)
	}
	if len(body.Contacts) == 0 {
		t.Fatal("expected the caller's own demo contacts")
	}
	for _, c := range body.Contacts {
		if c["tenant_id"] != "demo" {
			t.Fatalf("cross-tenant read succeeded: got a contact in tenant %q", c["tenant_id"])
		}
	}
}

// registerAndLogin registers a user in tenantID and returns their access token.
// The token carries the tenant, so callers never pass one on later requests.
func registerAndLogin(t *testing.T, app *bootstrap.App, tenantID, email, password string) string {
	t.Helper()
	creds := map[string]string{"tenant": tenantID, "email": email, "password": password}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", jsonBody(t, creds))
	req.Header.Set("Content-Type", "application/json")
	doGateway(t, app, req)

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", jsonBody(t, creds))
	req2.Header.Set("Content-Type", "application/json")
	resp := doGateway(t, app, req2)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login for %s: got %d", email, resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return body["access_token"]
}
