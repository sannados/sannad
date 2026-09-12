package auth

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/getkayan/kayan/core/audit"
	"github.com/getkayan/kayan/core/identity"
)

// Audit returns the underlying audit logger, for writing events. Logging
// happens through this package's own call sites (Register, Login, service
// account lifecycle, role changes), never by a caller reaching in to build
// events itself: the fields that matter — tenant, actor, resource — are only
// reliably known where the action actually happens.
//
// Not exposed for querying: see QueryAuditEvents.
func (service *Service) Audit() *audit.Logger {
	return service.audit
}

// gormAuditEvent mirrors kayan-gorm's own model for the audit_events table.
// Duplicated deliberately — see QueryAuditEvents.
type gormAuditEvent struct {
	ID           string `gorm:"primaryKey"`
	Type         string
	ActorID      string
	SubjectID    string
	Status       string
	Message      string
	Metadata     identity.JSON `gorm:"type:json"`
	CreatedAt    time.Time
	TenantID     string `gorm:"column:tenant_id"`
	IPAddress    string
	UserAgent    string
	DeviceID     string
	SessionID    string
	ResourceType string
	ResourceID   string
	OldValue     identity.JSON `gorm:"type:json"`
	NewValue     identity.JSON `gorm:"type:json"`
	Risk         string
	RequestID    string
	GeoCountry   string
	GeoRegion    string
	GeoCity      string
	GeoLat       float64
	GeoLong      float64
}

func (gormAuditEvent) TableName() string { return "audit_events" }

// QueryAuditEvents reads the audit trail directly, rather than through
// gormstore.Repository.Query (audit.AuditStore's own implementation):
// that method failed under this deployment's SQLite driver with "unsupported
// Scan, storing driver.Value type string into type *time.Time" on
// created_at — found by this package's own audit tests, not by inspection.
// SaveEvent, which goes through GORM's ordinary struct-mapped Create, was and
// is unaffected; only that one read path was broken. Filed as a finding
// rather than chased further: this package's own model, mapped the same way
// every other table in this codebase already reads time.Time columns
// successfully, sidesteps whatever that path does differently.
func (service *Service) QueryAuditEvents(ctx context.Context, filter audit.Filter) ([]audit.AuditEvent, error) {
	q := service.db.WithContext(ctx).Model(&gormAuditEvent{})
	if filter.TenantID != "" {
		q = q.Where("tenant_id = ?", filter.TenantID)
	}
	if filter.ActorID != "" {
		q = q.Where("actor_id = ?", filter.ActorID)
	}
	if filter.SubjectID != "" {
		q = q.Where("subject_id = ?", filter.SubjectID)
	}
	if len(filter.Types) > 0 {
		q = q.Where("type IN ?", filter.Types)
	}
	if len(filter.Statuses) > 0 {
		q = q.Where("status IN ?", filter.Statuses)
	}
	if !filter.StartTime.IsZero() {
		q = q.Where("created_at >= ?", filter.StartTime)
	}
	if !filter.EndTime.IsZero() {
		q = q.Where("created_at <= ?", filter.EndTime)
	}
	if filter.OrderBy != "" {
		q = q.Order(parseOrderBy(filter.OrderBy))
	} else {
		q = q.Order("created_at DESC")
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}

	var rows []gormAuditEvent
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("auth: query audit events: %w", err)
	}

	events := make([]audit.AuditEvent, len(rows))
	for i, r := range rows {
		events[i] = audit.AuditEvent{
			ID: r.ID, Type: r.Type, ActorID: r.ActorID, SubjectID: r.SubjectID,
			Status: r.Status, Message: r.Message, Metadata: r.Metadata, CreatedAt: r.CreatedAt,
			TenantID: r.TenantID, IPAddress: r.IPAddress, UserAgent: r.UserAgent,
			DeviceID: r.DeviceID, SessionID: r.SessionID,
			ResourceType: r.ResourceType, ResourceID: r.ResourceID,
			OldValue: r.OldValue, NewValue: r.NewValue,
			Risk: audit.RiskLevel(r.Risk), RequestID: r.RequestID,
		}
	}
	return events, nil
}

// parseOrderBy converts audit.Filter's "-created_at" convention
// (leading dash means descending) into GORM's own ORDER BY syntax.
func parseOrderBy(orderBy string) string {
	if len(orderBy) > 0 && orderBy[0] == '-' {
		return orderBy[1:] + " DESC"
	}
	return orderBy + " ASC"
}

// logAudit records a security-relevant event best-effort. A failure to write
// an audit record does not fail the action it describes — a login must not
// be refused because the audit table is briefly unreachable — but it is
// never silent: an operator who cannot find an expected audit trail needs a
// log line pointing at why.
func (service *Service) logAudit(ctx context.Context, eventType, tenantID, actorID, subjectID string, success bool, message string) {
	builder := audit.NewEvent(eventType).
		Tenant(tenantID).
		Actor(actorID).
		Subject(subjectID).
		Message(message)
	if success {
		builder = builder.Success()
	} else {
		builder = builder.Failure()
	}
	if err := service.audit.Log(ctx, builder.Build()); err != nil {
		slog.WarnContext(ctx, "auth: audit log write failed",
			"type", eventType, "tenant", tenantID, "err", err)
	}
}
