package events_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/internal/kernel/events"
)

// The tenant lives in the subject, which is a security property rather than a
// naming convention: a consumer bound to one tenant's subject is never handed
// another's, whatever its handler does. A consumer filtering on a payload field
// has already received the data before it decides to ignore it.

func TestSubjectCarriesTheTenant(t *testing.T) {
	ctx := tenant.WithTenantID(context.Background(), "acme")

	subject, err := events.Subject(ctx, "orders.created")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}
	if subject != "sannad.acme.orders.created" {
		t.Fatalf("got %q, want sannad.acme.orders.created", subject)
	}
}

// TestSubjectRequiresATenant: publishing with no tenant would put the event on a
// subject no per-tenant consumer is bound to, so it would be silently
// undeliverable — or worse, land somewhere shared.
func TestSubjectRequiresATenant(t *testing.T) {
	_, err := events.Subject(context.Background(), "orders.created")
	if !errors.Is(err, events.ErrInvalidSubject) {
		t.Fatalf("expected ErrInvalidSubject, got %v", err)
	}
}

// TestTenantCannotEscapeItsSegment is the injection case. A tenant containing a
// dot would become two segments and land elsewhere in the subject space; one
// containing a wildcard would match more than itself. Both are refused rather
// than escaped, because a tenant identifier has no business containing either.
func TestTenantCannotEscapeItsSegment(t *testing.T) {
	for _, tenantID := range []string{
		"acme.evil", // extra segment
		"*",         // matches every tenant
		"acme.*",    // ditto
		">",         // matches everything below
		"acme evil", // whitespace
		"",          // empty
	} {
		if _, err := events.SubjectFor(tenantID, "orders.created"); !errors.Is(err, events.ErrInvalidSubject) {
			t.Errorf("tenant %q was accepted: %v", tenantID, err)
		}
		if _, err := events.TenantSubjects(tenantID); !errors.Is(err, events.ErrInvalidSubject) {
			t.Errorf("filter for tenant %q was accepted: %v", tenantID, err)
		}
	}
}

// TestEventNameCannotEscapeItsSegments: the same injection through the other
// parameter. An event name containing a wildcard would let a publisher write to
// a subject a consumer of a different event is bound to.
func TestEventNameCannotEscapeItsSegments(t *testing.T) {
	for _, event := range []string{"orders.*", "orders.>", "orders created", ""} {
		if _, err := events.SubjectFor("acme", event); !errors.Is(err, events.ErrInvalidSubject) {
			t.Errorf("event %q was accepted: %v", event, err)
		}
	}
}

// TestTenantSubjectsMatchOnlyThatTenant pins the filter shape. If this ever
// widened, every consumer using it would begin receiving other tenants' events
// with nothing in their own code changing.
func TestTenantSubjectsMatchOnlyThatTenant(t *testing.T) {
	filter, err := events.TenantSubjects("acme")
	if err != nil {
		t.Fatalf("TenantSubjects: %v", err)
	}
	if filter != "sannad.acme.>" {
		t.Fatalf("got %q, want sannad.acme.>", filter)
	}
	// The wildcard must be scoped under the tenant, never above it.
	if strings.HasPrefix(filter, "sannad.*") || strings.HasPrefix(filter, "sannad.>") {
		t.Fatalf("filter %q spans tenants", filter)
	}
}

func TestTenantOfExtractsTheTenant(t *testing.T) {
	tenantID, ok := events.TenantOf("sannad.acme.orders.created")
	if !ok || tenantID != "acme" {
		t.Fatalf("got (%q, %v), want (acme, true)", tenantID, ok)
	}

	// A subject that is not ours must not yield a tenant, or a stray message
	// would be attributed to whatever happened to sit in that position.
	for _, subject := range []string{"other.acme.orders", "sannad", "sannad.acme", ""} {
		if _, ok := events.TenantOf(subject); ok {
			t.Errorf("subject %q yielded a tenant", subject)
		}
	}
}

// TestCrossTenantSubjectIsExplicit: a filter spanning tenants exists, because
// infrastructure legitimately needs one, but it must be named at the call site
// so granting it is visible in review.
func TestCrossTenantSubjectIsExplicit(t *testing.T) {
	filter, err := events.AllTenantsSubject("orders.created")
	if err != nil {
		t.Fatalf("AllTenantsSubject: %v", err)
	}
	if filter != "sannad.*.orders.created" {
		t.Fatalf("got %q, want sannad.*.orders.created", filter)
	}
}
