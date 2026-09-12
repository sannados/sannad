package modulekit

import "context"

// Store is the storage handle a module receives. It names no engine: no
// *gorm.DB, no *sql.DB, no driver, no DSN. Bootstrap selects the engine and
// constructs an adapter; the module holds this interface. See ADR 0006.
//
// # Transaction scope
//
// A module gets ACID guarantees within its own data and no transaction spanning
// another module's data. Transact runs against the calling module's tables only.
// Cross-module consistency is reached through capability calls for authoritative
// reads, and events with local projections or a saga for everything else — see
// ADR 0004.
//
// This is a deliberate limit, not an omission. A shared unit of work would
// require a common engine and a common connection, and mutual visibility of
// uncommitted state across modules that are meant to deploy independently.
//
// # Tenancy
//
// Every method takes a context, and the tenant travels in it. Modules never
// name a tenant and never write a tenant predicate: isolation is applied
// underneath by the adapter, and a scoped operation with no tenant in context
// fails rather than running unscoped. See ADR 0005.
type Store interface {
	// Find loads all records matching the query into dest, which must be a
	// pointer to a slice of the model type.
	Find(ctx context.Context, dest any, conditions ...Condition) error

	// First loads the single record matching the query into dest. It returns
	// ErrNotFound when nothing matches, which callers should test with
	// errors.Is rather than by comparing strings.
	First(ctx context.Context, dest any, conditions ...Condition) error

	// Count reports how many records match the query. model is a pointer to a
	// zero value of the model type, which is how the target table is named.
	Count(ctx context.Context, model any, conditions ...Condition) (int64, error)

	// Create inserts value, which may be a pointer to a single record or to a
	// slice of them. Fields the adapter owns — the tenant, generated defaults —
	// are populated on the passed value.
	Create(ctx context.Context, value any) error

	// Update applies changes to every record matching the query. model names the
	// table; changes maps column names to their new values. An Update with no
	// conditions returns ErrUnconditional rather than rewriting the table.
	Update(ctx context.Context, model any, changes map[string]any, conditions ...Condition) error

	// Delete removes every record matching the query. A Delete with no
	// conditions returns ErrUnconditional rather than emptying the table.
	Delete(ctx context.Context, model any, conditions ...Condition) error

	// Transact runs fn inside a transaction over the module's own data. The
	// Store passed to fn is the transactional handle; using the outer Store
	// inside fn escapes the transaction. Returning an error rolls back.
	//
	// Nested calls join the enclosing transaction rather than opening a second.
	Transact(ctx context.Context, fn func(tx Store) error) error
}

// Condition is one predicate in a query. Conditions are combined with AND.
//
// This is deliberately a small, declarative set rather than a general query
// language. It is the shape modules actually need, and it keeps the interface
// implementable on more than one engine — a raw SQL string in the contract
// would bind every module to SQL. A module whose query genuinely cannot be
// expressed here should own a projection or a read model, which ADR 0004
// already requires for anything join-heavy.
type Condition struct {
	Field string
	Op    Operator
	Value any
}

// Operator enumerates the comparisons a Condition may express.
type Operator string

const (
	OpEqual        Operator = "="
	OpNotEqual     Operator = "<>"
	OpGreaterThan  Operator = ">"
	OpGreaterEqual Operator = ">="
	OpLessThan     Operator = "<"
	OpLessEqual    Operator = "<="
	OpIn           Operator = "IN"
	OpLike         Operator = "LIKE"
)

// Where builds an equality condition, the common case.
func Where(field string, value any) Condition {
	return Condition{Field: field, Op: OpEqual, Value: value}
}

// Compare builds a condition with an explicit operator.
func Compare(field string, op Operator, value any) Condition {
	return Condition{Field: field, Op: op, Value: value}
}

// In builds a membership condition. values must be a slice.
func In(field string, values any) Condition {
	return Condition{Field: field, Op: OpIn, Value: values}
}
