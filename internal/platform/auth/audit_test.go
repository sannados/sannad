package auth_test

import (
	"context"
	"testing"

	"github.com/getkayan/kayan/core/audit"
)

// TestLoginSuccessIsAudited: the point of the whole feature — a compliance
// review must be able to answer "who logged in, when" without any custom
// logging call sites scattered across the codebase.
func TestLoginSuccessIsAudited(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "alice@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.Login(ctx, "acme", "alice@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("login: %v", err)
	}

	events, err := svc.QueryAuditEvents(ctx, audit.Filter{TenantID: "acme", Types: []string{audit.EventLoginSuccess}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d login-success events, want 1", len(events))
	}
	if events[0].Status != "success" {
		t.Fatalf("status = %q, want success", events[0].Status)
	}
}

// TestLoginFailureIsAudited: the security-relevant half — failed attempts
// must be visible too, or the audit trail cannot answer "was this account
// under attack."
func TestLoginFailureIsAudited(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "bob@example.com", "correct-horse-battery-staple"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.Login(ctx, "acme", "bob@example.com", "wrong-password"); err == nil {
		t.Fatal("wrong password logged in")
	}

	events, err := svc.QueryAuditEvents(ctx, audit.Filter{TenantID: "acme", Types: []string{audit.EventLoginFailure}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d login-failure events, want 1", len(events))
	}
	if events[0].Status != "failure" {
		t.Fatalf("status = %q, want failure", events[0].Status)
	}
}

// TestAuditEventsAreTenantScoped: a compliance query for one tenant must
// never surface another tenant's security events — an audit endpoint that
// leaked across tenants would be a worse problem than the one it exists to
// solve.
func TestAuditEventsAreTenantScoped(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.Register(ctx, "acme", "alice@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register acme: %v", err)
	}
	if err := svc.Register(ctx, "globex", "carol@example.com", "hunter2-hunter2-hunter2"); err != nil {
		t.Fatalf("register globex: %v", err)
	}

	acmeEvents, err := svc.QueryAuditEvents(ctx, audit.Filter{TenantID: "acme", Types: []string{audit.EventUserCreated}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, e := range acmeEvents {
		if e.TenantID != "acme" {
			t.Fatalf("acme's query returned an event for tenant %q", e.TenantID)
		}
	}
	if len(acmeEvents) != 1 {
		t.Fatalf("got %d events for acme, want 1 (its own registration only)", len(acmeEvents))
	}
}

// TestRoleAssignmentIsAudited: RBAC changes are exactly the kind of event a
// SOC 2 review asks for by name.
func TestRoleAssignmentIsAudited(t *testing.T) {
	svc := newTestService(t)
	ctx := tenantCtx("acme")

	if err := svc.SeedDefaultRoles(ctx, "acme"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.AssignRole(ctx, "user-1", "admin"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	events, err := svc.QueryAuditEvents(ctx, audit.Filter{TenantID: "acme", Types: []string{audit.EventRoleAssigned}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d role-assigned events, want 1", len(events))
	}
	if events[0].SubjectID != "user-1" {
		t.Fatalf("subject = %q, want user-1", events[0].SubjectID)
	}
}
