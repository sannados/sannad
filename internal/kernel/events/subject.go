package events

import (
	"context"
	"fmt"
	"strings"

	"github.com/getkayan/kayan/core/tenant"
)

// A subject carries the tenant, and that is a security property rather than a
// naming convention.
//
// A consumer subscribing to `sannad.*.contacts.created` and filtering by a
// tenant field in the payload has already received every tenant's events. The
// filtering happens after delivery, in code somebody has to write correctly, in
// every consumer, forever. Putting the tenant in the subject moves the boundary
// into the broker: a consumer bound to one tenant's subject is never handed
// another's, whatever its code does.
//
// The layout is:
//
//	sannad.<tenant>.<event>
//
// so a per-tenant binding is `sannad.acme.>` and a cross-tenant one is
// `sannad.*.orders.created`, which a deployment grants deliberately rather than
// by accident.

// StreamName is the JetStream stream every Sannad event is published to.
const StreamName = "SANNAD"

// SubjectPrefix is the root of the subject space.
const SubjectPrefix = "sannad"

// StreamSubjects is the wildcard the stream binds, covering every tenant and
// every event.
const StreamSubjects = SubjectPrefix + ".>"

// ErrInvalidSubject is returned when an event name cannot be turned into a
// subject.
var ErrInvalidSubject = fmt.Errorf("events: invalid subject")

// Subject builds the tenant-scoped subject for an event.
//
// The tenant comes from the context, never from a parameter: a caller that could
// name the tenant could publish into another tenant's stream, which is the
// cross-tenant write this design exists to prevent. Publishing with no tenant in
// context fails rather than defaulting to a shared subject.
func Subject(ctx context.Context, event string) (string, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return "", fmt.Errorf("%w: no tenant in context for event %q", ErrInvalidSubject, event)
	}
	return SubjectFor(tenantID, event)
}

// SubjectFor builds a subject for an explicit tenant.
//
// Used by infrastructure that legitimately spans tenants — a replay tool, a
// webhook dispatcher rebuilding a subscription. Module code should use Subject
// and let the context supply the tenant.
func SubjectFor(tenantID, event string) (string, error) {
	if err := validToken(tenantID, "tenant"); err != nil {
		return "", err
	}
	if event == "" {
		return "", fmt.Errorf("%w: event name is required", ErrInvalidSubject)
	}
	for _, segment := range strings.Split(event, ".") {
		if err := validToken(segment, "event"); err != nil {
			return "", err
		}
	}
	return SubjectPrefix + "." + tenantID + "." + event, nil
}

// TenantSubjects is the subscription filter for one tenant's events.
//
// This is what a per-tenant consumer binds to. A consumer bound here cannot
// receive another tenant's events even if its handler ignores every field.
func TenantSubjects(tenantID string) (string, error) {
	if err := validToken(tenantID, "tenant"); err != nil {
		return "", err
	}
	return SubjectPrefix + "." + tenantID + ".>", nil
}

// AllTenantsSubject is the filter for one event across every tenant.
//
// Deliberately named to be conspicuous at the call site: a deployment granting
// this is granting cross-tenant read, and that should be visible in review
// rather than hidden behind a string literal.
func AllTenantsSubject(event string) (string, error) {
	if event == "" {
		return "", fmt.Errorf("%w: event name is required", ErrInvalidSubject)
	}
	return SubjectPrefix + ".*." + event, nil
}

// TenantOf extracts the tenant from a subject, for a consumer that legitimately
// spans tenants and needs to know which one an event belongs to.
func TenantOf(subject string) (string, bool) {
	parts := strings.SplitN(subject, ".", 3)
	if len(parts) < 3 || parts[0] != SubjectPrefix || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// validToken rejects the characters that would let a value break out of its
// position in the subject.
//
// A tenant containing a dot would silently become two segments and land in a
// different part of the subject space; one containing a wildcard would subscribe
// to more than itself. Both are refused rather than escaped, because a tenant
// identifier has no business containing either.
func validToken(value, kind string) error {
	if value == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidSubject, kind)
	}
	if strings.ContainsAny(value, ".*> \t") {
		return fmt.Errorf("%w: %s %q contains a subject delimiter or wildcard",
			ErrInvalidSubject, kind, value)
	}
	return nil
}
