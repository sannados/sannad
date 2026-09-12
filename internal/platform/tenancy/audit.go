package tenancy

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// Tenant isolation is opt-in: a model is scoped because it implements
// TenantAware, or because RegisterScopedModels named it. A model that does
// neither is not refused — it is simply queried unscoped, across every tenant,
// and the isolation callback returns early without comment.
//
// That is the right runtime behaviour, because plenty of tables are legitimately
// global: migrations, feature flags, the tenant directory itself. But it means
// the difference between "deliberately global" and "someone forgot" is invisible
// at runtime, and the second one is a silent cross-tenant read.
//
// The audit closes that gap by asking the database rather than the code. A table
// with a tenant_id column is a table whose author intended it to be per-tenant;
// if nothing registered it as scoped, that intent is not being enforced.
//
// It cannot run at compile time: the set of scoped models is assembled at
// startup from modules the kernel does not know about until they install.

// AuditFinding is one table that carries a tenant column but is not scoped.
type AuditFinding struct {
	// Table is the table name as the database reports it.
	Table string

	// Column is the tenant-identifying column found on it.
	Column string
}

func (f AuditFinding) String() string {
	return fmt.Sprintf("%s.%s", f.Table, f.Column)
}

// tenantColumnNames are the column names taken as evidence that a table was
// meant to be per-tenant.
//
// Deliberately short. A longer list would catch more, but every extra name is a
// chance to flag a legitimately global table and train operators to ignore the
// warning — which costs more than the case it catches.
var tenantColumnNames = []string{"tenant_id", "tenantid"}

// AuditScopedModels reports tables that carry a tenant column but are not
// registered as tenant-scoped.
//
// Call it at startup, after every module has installed. A finding is not proof
// of a bug — a table may carry a tenant column and still be intentionally
// queried globally — but each one is a place where isolation is assumed and not
// enforced, and that is worth a human deciding rather than nobody knowing.
//
// The audit reads schema metadata only. It never reads rows, so it is safe to
// run against production data and cheap enough to run on every boot.
func AuditScopedModels(ctx context.Context, db *gorm.DB, exempt ...string) ([]AuditFinding, error) {
	if db == nil {
		return nil, fmt.Errorf("tenancy: audit needs a database handle")
	}

	// Exemptions are passed in rather than hard-coded so the list lives with the
	// deployment that owns the tables. Each one is a table whose author decided
	// it cannot be scoped, and the audit's job is to make that a decision
	// somebody recorded rather than a warning everybody ignores.
	exemptions := make(map[string]struct{}, len(exempt))
	for _, table := range exempt {
		exemptions[table] = struct{}{}
	}

	// The audit inspects the whole schema, which is by definition not one
	// tenant's data.
	//
	// This is not currently load-bearing: GORM's migrator issues schema
	// introspection outside the query callbacks, so it is not refused for
	// lacking a tenant. It is set because the intent is exactly what the system
	// context means, and because a future GORM that routes introspection through
	// the callbacks would otherwise turn every boot into a failed audit.
	ctx = WithSystemContext(ctx)
	migrator := db.WithContext(ctx).Migrator()

	tables, err := migrator.GetTables()
	if err != nil {
		return nil, fmt.Errorf("tenancy: list tables: %w", err)
	}

	var findings []AuditFinding
	for _, table := range tables {
		if isScopedTable(table) {
			continue
		}
		if _, ok := exemptions[table]; ok {
			continue
		}
		column, ok := tenantColumnOn(migrator, table)
		if !ok {
			continue
		}
		findings = append(findings, AuditFinding{Table: table, Column: column})
	}

	// Sorted so the output is stable across boots. An audit whose order changes
	// run to run is one nobody can diff.
	sort.Slice(findings, func(i, j int) bool { return findings[i].Table < findings[j].Table })
	return findings, nil
}

// tenantColumnOn reports whether a table carries a tenant-identifying column.
func tenantColumnOn(migrator gorm.Migrator, table string) (string, bool) {
	columns, err := migrator.ColumnTypes(table)
	if err != nil {
		// A table whose columns cannot be read is not evidence of a problem —
		// it may be a view, or dropped between the two calls. Staying quiet is
		// right here: a false finding on every boot is how an audit gets muted.
		return "", false
	}

	for _, column := range columns {
		name := strings.ToLower(column.Name())
		for _, candidate := range tenantColumnNames {
			if name == candidate {
				return column.Name(), true
			}
		}
	}
	return "", false
}

// LogSelfManagedStorage reports the modules the audit could not inspect.
//
// AuditScopedModels asks the database which tables carry a tenant column, so a
// module keeping its data anywhere else — its own database, a remote API, memory
// — is invisible to it. Reporting "every table with a tenant column is scoped"
// while three modules hold tenant data nobody looked at reads as a clean bill of
// health, which is worse than saying nothing.
//
// These modules are not doing anything wrong. They declared StorageOwn, which is
// a supported choice. What is reported is the shape of the trust: their isolation
// is the author's claim rather than a mechanism the kernel enforces.
func LogSelfManagedStorage(ctx context.Context, moduleIDs []string) {
	if len(moduleIDs) == 0 {
		return
	}
	for _, id := range moduleIDs {
		slog.WarnContext(ctx, "tenancy audit: module manages its own storage",
			"module", id,
			"consequence", "tenant isolation is this module's responsibility and the audit cannot inspect its data",
			"action", "review this module's tenant handling; the kernel cannot verify it")
	}
	slog.WarnContext(ctx, "tenancy audit: modules outside kernel storage", "count", len(moduleIDs))
}

// LogAuditFindings writes the audit result to the log at startup.
//
// Warn, not error: the kernel must still boot. An operator upgrading a kernel
// should not be locked out because a module they did not write has a table this
// audit does not like, and a refusal would make the safe move — running the
// audit at all — the one that breaks production.
//
// Each finding names the table and the fix, because a warning an operator cannot
// act on is a warning they will learn to skip.
func LogAuditFindings(ctx context.Context, findings []AuditFinding) {
	if len(findings) == 0 {
		slog.InfoContext(ctx, "tenancy audit: every table with a tenant column is scoped")
		return
	}

	for _, f := range findings {
		slog.WarnContext(ctx, "tenancy audit: table carries a tenant column but is not scoped",
			"table", f.Table,
			"column", f.Column,
			"consequence", "queries against this table run across every tenant",
			"fix", "implement tenant.TenantAware on the model, or pass it to RegisterScopedModels")
	}
	slog.WarnContext(ctx, "tenancy audit: unscoped tables found", "count", len(findings))
}
