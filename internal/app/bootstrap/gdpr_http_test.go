package bootstrap_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGateway_ExportAndDeleteMe: a caller can read and then erase their own
// data through the gateway, and the erased account can no longer log in.
func TestGateway_ExportAndDeleteMe(t *testing.T) {
	app := gatewayApp(t)
	token := registerAndLogin(t, app, "acme", "kate@example.com", "StrongPass1234!")

	exportReq := httptest.NewRequest(http.MethodGet, "/api/v1/me/export", nil)
	exportReq.Header.Set("Authorization", "Bearer "+token)
	exportResp := doGateway(t, app, exportReq)
	if exportResp.StatusCode != http.StatusOK {
		t.Fatalf("export: got %d", exportResp.StatusCode)
	}
	var export struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(exportResp.Body).Decode(&export); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if export.Email != "kate@example.com" {
		t.Fatalf("exported email = %q, want kate@example.com", export.Email)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/me", nil)
	deleteReq.Header.Set("Authorization", "Bearer "+token)
	deleteResp := doGateway(t, app, deleteReq)
	if deleteResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete me: got %d", deleteResp.StatusCode)
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		jsonBody(t, map[string]string{"tenant": "acme", "email": "kate@example.com", "password": "StrongPass1234!"}))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp := doGateway(t, app, loginReq)
	if loginResp.StatusCode == http.StatusOK {
		t.Fatal("login succeeded against a deleted account")
	}
}

// TestGateway_OwnerCanDeleteAMember: the operator-initiated deletion path,
// gated the same as lock/unlock — identities:write, owner only by default.
func TestGateway_OwnerCanDeleteAMember(t *testing.T) {
	app := gatewayApp(t)
	ownerToken := registerAndLogin(t, app, "acme", "owner4@example.com", "StrongPass1234!")
	memberToken := registerAndLogin(t, app, "acme", "member4@example.com", "StrongPass1234!")
	memberID := meSubject(t, app, memberToken)

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/identities/"+memberID, nil)
	deleteReq.Header.Set("Authorization", "Bearer "+ownerToken)
	deleteResp := doGateway(t, app, deleteReq)
	if deleteResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got %d", deleteResp.StatusCode)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+memberToken)
	meResp := doGateway(t, app, meReq)
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("deleted member's token still works: got %d", meResp.StatusCode)
	}
}

// TestGateway_MemberCannotDeleteAnAccount: identities:write is owner-only by
// default, same as lock/unlock/revoke-sessions.
func TestGateway_MemberCannotDeleteAnAccount(t *testing.T) {
	app := gatewayApp(t)
	_ = registerAndLogin(t, app, "acme", "owner5@example.com", "StrongPass1234!")
	memberToken := registerAndLogin(t, app, "acme", "member5@example.com", "StrongPass1234!")
	memberID := meSubject(t, app, memberToken)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/identities/"+memberID, nil)
	req.Header.Set("Authorization", "Bearer "+memberToken)
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member deleting itself: got %d, want 403", resp.StatusCode)
	}
}

// TestGateway_ExportAndDeleteMeRequireAuth: neither self-service endpoint
// works without a bearer token.
func TestGateway_ExportAndDeleteMeRequireAuth(t *testing.T) {
	app := gatewayApp(t)

	exportResp := doGateway(t, app, httptest.NewRequest(http.MethodGet, "/api/v1/me/export", nil))
	if exportResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("export without auth: got %d, want 401", exportResp.StatusCode)
	}

	deleteResp := doGateway(t, app, httptest.NewRequest(http.MethodDelete, "/api/v1/me", nil))
	if deleteResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("delete without auth: got %d, want 401", deleteResp.StatusCode)
	}
}
