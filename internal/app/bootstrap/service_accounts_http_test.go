package bootstrap_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGateway_ServiceAccountLifecycle exercises the whole path end to end:
// an authenticated human creates a service account, the returned API key logs
// in as that service account through the unauthenticated service-login
// endpoint, and the resulting token authenticates an ordinary requireAuth
// route exactly like a human session would.
func TestGateway_ServiceAccountLifecycle(t *testing.T) {
	app := gatewayApp(t)
	token := registerAndLogin(t, app, "demo", "owner@example.com", "StrongPass1234!")

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/service-accounts",
		jsonBody(t, map[string]string{"name": "ci-runner"}))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+token)
	createResp := doGateway(t, app, createReq)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create service account: got %d", createResp.StatusCode)
	}
	var created struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.APIKey == "" {
		t.Fatal("no api_key in create response")
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/service-login",
		jsonBody(t, map[string]string{"api_key": created.APIKey}))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp := doGateway(t, app, loginReq)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("service login: got %d", loginResp.StatusCode)
	}
	var session struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(loginResp.Body).Decode(&session); err != nil {
		t.Fatalf("decode service login: %v", err)
	}
	if session.AccessToken == "" {
		t.Fatal("no access_token in service login response")
	}

	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+session.AccessToken)
	meResp := doGateway(t, app, meReq)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("service account token rejected by an ordinary authenticated route: got %d", meResp.StatusCode)
	}
}

// TestGateway_ServiceLoginRejectsAWrongKey: the unauthenticated login
// endpoint must not authenticate a key nobody issued.
func TestGateway_ServiceLoginRejectsAWrongKey(t *testing.T) {
	app := gatewayApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/service-login",
		jsonBody(t, map[string]string{"api_key": "whsec_forged"}))
	req.Header.Set("Content-Type", "application/json")
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
}

// TestGateway_CreateServiceAccountRequiresAuth: creating a service account is
// a management action and must sit behind the same requireAuth every other
// tenant-scoped write does.
func TestGateway_CreateServiceAccountRequiresAuth(t *testing.T) {
	app := gatewayApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/service-accounts",
		jsonBody(t, map[string]string{"name": "ci-runner"}))
	req.Header.Set("Content-Type", "application/json")
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
}
