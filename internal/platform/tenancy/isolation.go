// Package tenancy enforces row-level tenant isolation at the storage layer.
//
// Kayan's core/tenant package supplies context propagation, the TenantAware
// model interface, and request resolvers. It does not supply GORM enforcement,
// so that piece lives here. This package is bootstrap-layer infrastructure: it
// depends on *gorm.DB and must never be imported from pkg/modulekit or from a
// module. See ADR 0005 and ADR 0006.
//
// # Guarantee
//
// Once RegisterIsolation is installed, any query, insert, update, or delete
// touching a model that implements tenant.TenantAware is either scoped to the
// tenant in the request context or fails. There is no path that silently
// returns or mutates another tenant's rows, and no per-query opt-in for a
// module author to forget.
//
// Models that do not implement TenantAware are untouched. That is deliberate —
// system tables (accounts, tenants, migrations) are global by nature — but it
// also means a model that *should* be scoped and forgets the interface gets no
// isolation. Auditable reports that: see ADR 0005's review checklist.
//
// # Escaping the scope
//
// Legitimate cross-tenant work — migrations, platform administration,
// background jobs that span tenants — must opt in explicitly with
// WithSystemContext. The escape is loud, greppable, and never the default.
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrNoTenantContext is returned when an operation touches a tenant-scoped
// model without a tenant in context and without an explicit system-context
// escape. It is deliberately a hard failure: the alternative is a query that
// silently spans every tenant in the deployment.
var ErrNoTenantContext = errors.New("tenancy: operation on a tenant-scoped model without tenant context")

// ErrTenantMismatch is returned when a write carries a tenant ID that differs
// from the tenant in context — an attempt, deliberate or accidental, to write
// into another tenant's data.
var ErrTenantMismatch = errors.New("tenancy: record tenant does not match context tenant")

// ErrRawSQLOnScopedTable is returned when raw SQL names a tenant-scoped table
// outside a system context. Raw statements are opaque, so isolation cannot be
// applied to them — only refused.
var ErrRawSQLOnScopedTable = errors.New("tenancy: raw SQL on a tenant-scoped table")

// ErrJoinOnScopedTable is returned when a query joins a tenant-scoped table it
// does not primarily target. The isolation predicate names the primary table,
// so a joined scoped table would otherwise be read unscoped.
var ErrJoinOnScopedTable = errors.New("tenancy: query joins a tenant-scoped table")

// ColumnName is the database column used for row-level isolation. Every scoped
// table must have this column indexed; the composite index should lead with it.
const ColumnName = "tenant_id"

// WithSystemContext marks a context as permitted to cross tenant boundaries.
//
// Use it for migrations, platform administration, and cross-tenant background
// jobs. Never derive it from request input. Every call site is a deliberate
// decision to disable the primary isolation guarantee and should read like one.
func WithSystemContext(ctx context.Context) context.Context {
	return modulekit.WithSystemContext(ctx)
}

// IsSystemContext reports whether ctx may cross tenant boundaries.
func IsSystemContext(ctx context.Context) bool {
	return modulekit.IsSystemContext(ctx)
}

// RegisterIsolation installs tenant isolation callbacks on db.
//
// Call this once during bootstrap, immediately after opening the connection and
// before any module is constructed. Registering per-request or per-query is a
// bug: the guarantee depends on there being no unhooked path to the database.
func RegisterIsolation(db *gorm.DB) error {
	callbacks := []struct {
		name     string
		register func() error
	}{
		{"tenancy:query", func() error {
			return db.Callback().Query().Before("gorm:query").Register("tenancy:query", scopeQuery)
		}},
		{"tenancy:row_query", func() error {
			return db.Callback().Row().Before("gorm:row").Register("tenancy:row_query", scopeQuery)
		}},
		{"tenancy:update", func() error {
			return db.Callback().Update().Before("gorm:update").Register("tenancy:update", scopeQuery)
		}},
		{"tenancy:delete", func() error {
			return db.Callback().Delete().Before("gorm:delete").Register("tenancy:delete", scopeQuery)
		}},
		{"tenancy:create", func() error {
			return db.Callback().Create().Before("gorm:create").Register("tenancy:create", stampCreate)
		}},
		{"tenancy:raw", func() error {
			return db.Callback().Raw().Before("gorm:raw").Register("tenancy:raw", guardRaw)
		}},
	}

	for _, cb := range callbacks {
		if err := cb.register(); err != nil {
			return fmt.Errorf("tenancy: register %s callback: %w", cb.name, err)
		}
	}
	return nil
}

// guardRaw refuses raw SQL that names a scoped table without a system context.
//
// A predicate cannot be injected into arbitrary SQL, so raw statements are the
// one path where isolation cannot be enforced — only refused. Silently allowing
// them would leave a hole precisely where a module author reaches for a hand
// written query, which is where cross-tenant reporting queries get written.
//
// Deliberate cross-tenant raw SQL remains possible under WithSystemContext,
// where it is explicit and greppable.
func guardRaw(db *gorm.DB) {
	if db.Error != nil || db.Statement == nil {
		return
	}
	if IsSystemContext(db.Statement.Context) {
		return
	}
	if table, ok := scopedTableInSQL(db.Statement.SQL.String()); ok {
		// Refused even when a tenant is in context. The statement is opaque, so
		// there is no way to tell a correctly scoped query from one missing its
		// predicate — and treating "a tenant exists" as sufficient would accept
		// exactly the unscoped hand-written query this is meant to catch.
		//
		// Scoped work belongs on the query builder, where the predicate is added
		// automatically. Deliberate cross-tenant raw SQL goes under
		// WithSystemContext, where it is explicit and greppable.
		_ = db.AddError(fmt.Errorf("%w: raw SQL touching scoped table %q; use the query builder, or WithSystemContext for deliberate cross-tenant access",
			ErrRawSQLOnScopedTable, table))
	}
}

// scopedTableInSQL reports whether raw SQL mentions a table known to be scoped.
// The match is deliberately coarse — it errs toward refusing.
func scopedTableInSQL(sql string) (string, bool) {
	if sql == "" {
		return "", false
	}
	lowered := strings.ToLower(sql)
	var found string
	scopedTables.Range(func(key, _ any) bool {
		table, _ := key.(string)
		if table != "" && strings.Contains(lowered, strings.ToLower(table)) {
			found = table
			return false
		}
		return true
	})
	return found, found != ""
}

// scopeQuery adds a tenant predicate to reads, updates, and deletes against
// scoped models. It runs before GORM builds the statement, so the predicate is
// part of the emitted SQL rather than a filter applied afterwards.
func scopeQuery(db *gorm.DB) {
	if db.Error != nil || db.Statement == nil {
		return
	}

	ctx := db.Statement.Context
	if IsSystemContext(ctx) {
		return
	}

	// Raw().Scan() and Raw().Row() arrive here rather than through the Raw
	// callback, carrying pre-built SQL and usually no model. GORM will not add a
	// predicate to a statement it did not build, so these are refused on the
	// same grounds as Exec.
	if db.Statement.SQL.Len() > 0 {
		if table, ok := scopedTableInSQL(db.Statement.SQL.String()); ok {
			_ = db.AddError(fmt.Errorf("%w: raw SQL touching scoped table %q; use the query builder, or WithSystemContext for deliberate cross-tenant access",
				ErrRawSQLOnScopedTable, table))
		}
		return
	}

	// A scoped table reached through a JOIN is refused rather than scoped: the
	// predicate would have to name the joined table, and inferring that from an
	// arbitrary join expression is not worth getting subtly wrong. This is
	// checked before isScoped, because the dangerous case is precisely the one
	// where the primary table is unscoped and the check would otherwise pass.
	if joined, ok := joinedScopedTable(db.Statement); ok {
		_ = db.AddError(fmt.Errorf("%w: query joins scoped table %q; query it directly, or use WithSystemContext for deliberate cross-tenant access",
			ErrJoinOnScopedTable, joined))
		return
	}

	if !isScoped(db.Statement) {
		return
	}

	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		_ = db.AddError(ErrNoTenantContext)
		return
	}

	// Qualify the column with the table name: an unqualified tenant_id is
	// ambiguous the moment a caller joins two scoped tables.
	db.Statement.AddClause(clauseForTenant(db.Statement.Table, tenantID))
}

// stampCreate sets tenant_id on inserted records from the context tenant, and
// rejects any record that arrives carrying a different tenant. Callers never
// need to set the field themselves, and cannot use it to write across the
// boundary if they do.
func stampCreate(db *gorm.DB) {
	if db.Error != nil || db.Statement == nil {
		return
	}
	if !isScoped(db.Statement) {
		return
	}

	ctx := db.Statement.Context
	if IsSystemContext(ctx) {
		return
	}

	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		_ = db.AddError(ErrNoTenantContext)
		return
	}

	if err := eachRecord(db.Statement.ReflectValue, func(aware tenant.TenantAware) error {
		existing := aware.GetTenantID()
		if existing != "" && existing != tenantID {
			return fmt.Errorf("%w: record %q, context %q", ErrTenantMismatch, existing, tenantID)
		}
		aware.SetTenantID(tenantID)
		return nil
	}); err != nil {
		_ = db.AddError(err)
	}
}

// isScoped reports whether the statement targets tenant-scoped data.
//
// The primary check is on the model type, not the value, so it holds for queries
// that carry no instantiated record. A statement that names its table as a bare
// string carries no model type at all, so the table name is checked against the
// registry of scoped tables as a fallback — otherwise Table("contacts") would be
// an unscoped read of a scoped table.
func isScoped(stmt *gorm.Statement) bool {
	if stmt.Model != nil {
		if _, ok := stmt.Model.(tenant.TenantAware); ok {
			return true
		}
	}
	if stmt.Dest != nil {
		if implementsTenantAware(reflect.TypeOf(stmt.Dest)) {
			return true
		}
	}
	if stmt.Schema != nil && stmt.Schema.ModelType != nil {
		if implementsTenantAware(reflect.PointerTo(stmt.Schema.ModelType)) {
			return true
		}
	}
	return isScopedTable(stmt.Table)
}

// joinedScopedTable reports a scoped table pulled in by a JOIN.
//
// A query whose primary table is unscoped can still reach scoped rows through a
// join — `Table("global_records").Joins("CROSS JOIN contacts")` reads every
// tenant's contacts, because the primary-table check sees only the unscoped
// left side. Such a query is refused: the correct predicate would have to name
// the joined table, and guessing it from an arbitrary join expression is not
// something to get subtly wrong.
func joinedScopedTable(stmt *gorm.Statement) (string, bool) {
	if stmt == nil {
		return "", false
	}

	// At callback time joins are still on Statement.Joins — a join written as a
	// string keeps the whole expression in Name — and have not yet been folded
	// into the FROM clause. Both are checked so the guard does not depend on
	// when in the build the callback happens to run.
	for _, join := range stmt.Joins {
		if table, found := scopedTableInSQL(join.Name); found && table != stmt.Table {
			return table, true
		}
	}

	if fromClause, ok := stmt.Clauses["FROM"]; ok {
		if from, ok := fromClause.Expression.(clause.From); ok {
			for _, join := range from.Joins {
				if table, found := scopedTableInSQL(join.Table.Name); found && table != stmt.Table {
					return table, true
				}
			}
		}
	}
	return "", false
}

// scopedTables records the table name of every model registered as scoped. It
// closes the gap where a query names its table as a string and therefore has no
// model type for the interface check to inspect.
var scopedTables sync.Map // map[string]struct{}

// RegisterScopedModels records the tables belonging to tenant-scoped models, so
// that string-addressed queries against them are still recognised as scoped.
//
// Call this at bootstrap for every scoped model in the deployment, alongside
// RegisterIsolation. A model that is scoped but not registered here is still
// protected on every typed query path; only the Table("name") path degrades.
func RegisterScopedModels(db *gorm.DB, models ...tenant.TenantAware) error {
	for _, m := range models {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(m); err != nil {
			return fmt.Errorf("tenancy: parse model %T: %w", m, err)
		}
		scopedTables.Store(stmt.Table, struct{}{})
	}
	return nil
}

func isScopedTable(table string) bool {
	if table == "" {
		return false
	}
	_, ok := scopedTables.Load(table)
	return ok
}

var tenantAwareType = reflect.TypeOf((*tenant.TenantAware)(nil)).Elem()

// implementsTenantAware unwraps pointers, slices, and arrays to find the
// element type, then reports whether a pointer to it satisfies TenantAware.
// Dest is commonly *[]Model or *Model, and the interface is implemented on the
// pointer receiver in both cases.
func implementsTenantAware(t reflect.Type) bool {
	if t == nil {
		return false
	}
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	return reflect.PointerTo(t).Implements(tenantAwareType)
}

// eachRecord applies fn to every record in v, which may be a single struct or a
// slice of them. Records that do not satisfy TenantAware are skipped rather
// than erroring: isScoped has already established the model type does, and a
// mixed collection is not reachable through GORM's create path.
func eachRecord(v reflect.Value, fn func(tenant.TenantAware) error) error {
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := eachRecord(v.Index(i), fn); err != nil {
				return err
			}
		}
		return nil
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return eachRecord(v.Elem(), fn)
	case reflect.Struct:
		if !v.CanAddr() {
			return nil
		}
		aware, ok := v.Addr().Interface().(tenant.TenantAware)
		if !ok {
			return nil
		}
		return fn(aware)
	default:
		return nil
	}
}
