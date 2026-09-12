package tenancy

import (
	"gorm.io/gorm/clause"
)

// clauseForTenant builds a `WHERE <table>.tenant_id = ?` condition.
//
// The column is qualified with the table name because an unqualified
// tenant_id becomes ambiguous as soon as a query joins two scoped tables,
// and the resulting SQL error would surface far from its cause.
//
// clause.Where is additive in GORM: an existing user predicate is preserved
// and this condition is ANDed onto it, so a caller's own filters cannot
// displace the isolation predicate.
func clauseForTenant(table, tenantID string) clause.Where {
	col := clause.Column{Name: ColumnName}
	if table != "" && table != clause.CurrentTable {
		col.Table = table
	}
	return clause.Where{
		Exprs: []clause.Expression{
			clause.Eq{Column: col, Value: tenantID},
		},
	}
}
