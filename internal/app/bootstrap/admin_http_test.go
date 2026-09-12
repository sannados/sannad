package bootstrap_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sannados/sannad/internal/app/bootstrap"
)

// meSubject reads the caller's own subject ID via /api/v1/me.
func meSubject(t *testing.T, app *bootstrap.App, token string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/me: got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	return body["subject"]
}

// TestGateway_OwnerCanLockAndUnlockAMember: the owner locks a second
// account in the same tenant; the locked account can no longer log in, and
// unlocking restores it.
func TestGateway_OwnerCanLockAndUnlockAMember(t *testing.T) {
	app := gatewayApp(t)
	ownerToken := registerAndLogin(t, app, "acme", "owner@example.com", "StrongPass1234!")
	memberToken := registerAndLogin(t, app, "acme", "member@example.com", "StrongPass1234!")
	memberID := meSubject(t, app, memberToken)

	lockReq := httptest.NewRequest(http.MethodPost, "/api/v1/identities/"+memberID+"/lock", nil)
	lockReq.Header.Set("Authorization", "Bearer "+ownerToken)
	lockResp := doGateway(t, app, lockReq)
	if lockResp.StatusCode != http.StatusNoContent {
		t.Fatalf("lock: got %d", lockResp.StatusCode)
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		jsonBody(t, map[string]string{"tenant": "acme", "email": "member@example.com", "password": "StrongPass1234!"}))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp := doGateway(t, app, loginReq)
	if loginResp.StatusCode == http.StatusOK {
		t.Fatalf("login for a locked account succeeded: got %d", loginResp.StatusCode)
	}

	unlockReq := httptest.NewRequest(http.MethodPost, "/api/v1/identities/"+memberID+"/unlock", nil)
	unlockReq.Header.Set("Authorization", "Bearer "+ownerToken)
	unlockResp := doGateway(t, app, unlockReq)
	if unlockResp.StatusCode != http.StatusNoContent {
		t.Fatalf("unlock: got %d", unlockResp.StatusCode)
	}

	loginReq2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		jsonBody(t, map[string]string{"tenant": "acme", "email": "member@example.com", "password": "StrongPass1234!"}))
	loginReq2.Header.Set("Content-Type", "application/json")
	loginResp2 := doGateway(t, app, loginReq2)
	if loginResp2.StatusCode != http.StatusOK {
		t.Fatalf("login after unlock: got %d", loginResp2.StatusCode)
	}
}

// TestGateway_MemberCannotLockAnAccount: identities:write is granted to
// owner only, not the default member role, so a member locking anyone
// (including itself) is forbidden.
func TestGateway_MemberCannotLockAnAccount(t *testing.T) {
	app := gatewayApp(t)
	_ = registerAndLogin(t, app, "acme", "owner2@example.com", "StrongPass1234!")
	memberToken := registerAndLogin(t, app, "acme", "member2@example.com", "StrongPass1234!")
	memberID := meSubject(t, app, memberToken)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/identities/"+memberID+"/lock", nil)
	req.Header.Set("Authorization", "Bearer "+memberToken)
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member locking itself: got %d, want 403", resp.StatusCode)
	}
}

// TestGateway_RevokeAllSessionsInvalidatesWithoutLocking: the owner revokes
// a member's sessions; the member's old token stops working but they can
// still log back in.
func TestGateway_RevokeAllSessionsInvalidatesWithoutLocking(t *testing.T) {
	app := gatewayApp(t)
	ownerToken := registerAndLogin(t, app, "acme", "owner3@example.com", "StrongPass1234!")
	memberToken := registerAndLogin(t, app, "acme", "member3@example.com", "StrongPass1234!")
	memberID := meSubject(t, app, memberToken)

	revokeReq := httptest.NewRequest(http.MethodPost, "/api/v1/identities/"+memberID+"/revoke-sessions", nil)
	revokeReq.Header.Set("Authorization", "Bearer "+ownerToken)
	revokeResp := doGateway(t, app, revokeReq)
	if revokeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke-sessions: got %d", revokeResp.StatusCode)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+memberToken)
	meResp := doGateway(t, app, meReq)
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pre-revoke token still works: got %d", meResp.StatusCode)
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		jsonBody(t, map[string]string{"tenant": "acme", "email": "member3@example.com", "password": "StrongPass1234!"}))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp := doGateway(t, app, loginReq)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login after revoke-sessions should still succeed: got %d", loginResp.StatusCode)
	}
}

// TestGateway_LockAccountCrossTenantIsNotFound: an owner in one tenant
// cannot lock an account in another tenant by ID.
func TestGateway_LockAccountCrossTenantIsNotFound(t *testing.T) {
	app := gatewayApp(t)
	victimToken := registerAndLogin(t, app, "acme", "victim@example.com", "StrongPass1234!")
	victimID := meSubject(t, app, victimToken)
	attackerToken := registerAndLogin(t, app, "globex", "attacker@example.com", "StrongPass1234!")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/identities/"+victimID+"/lock", nil)
	req.Header.Set("Authorization", "Bearer "+attackerToken)
	resp := doGateway(t, app, req)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant lock: got %d, want 404", resp.StatusCode)
	}
}

// TestGateway_AdminOpsRequireAuth: none of the three routes work without a
// bearer token.
func TestGateway_AdminOpsRequireAuth(t *testing.T) {
	app := gatewayApp(t)
	for _, path := range []string{"/lock", "/unlock", "/revoke-sessions"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/identities/whatever"+path, nil)
		resp := doGateway(t, app, req)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without auth: got %d, want 401", path, resp.StatusCode)
		}
	}
}
