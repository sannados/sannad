// Package storage implements the modulekit.Store interface over GORM.
//
// This is the one adapter Sannad ships (ADR 0006). It is bootstrap-layer code:
// it names GORM, and nothing above it may. A module holds modulekit.Store and
// cannot discover which engine is underneath.
//
// Tenant isolation is not implemented here. It lives in the callbacks
// registered on the connection by internal/platform/tenancy, so it applies to
// every query this adapter issues without the adapter restating it — and so
// that a second adapter cannot forget it. See ADR 0005.
package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

// GormStore adapts a *gorm.DB to modulekit.Store.
type GormStore struct {
	db *gorm.DB
}

// compile-time check that *GormStore satisfies the SDK interface.
var _ modulekit.Store = (*GormStore)(nil)

// NewGormStore returns a Store backed by db.
//
// Call this from bootstrap only. The db passed here must already carry the
// tenant isolation callbacks; a raw connection would produce a Store that
// silently crosses tenants.
func NewGormStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) Find(ctx context.Context, dest any, conditions ...modulekit.Condition) error {
	query, err := s.apply(ctx, conditions)
	if err != nil {
		return err
	}
	return translateError(query.Find(dest).Error)
}

func (s *GormStore) First(ctx context.Context, dest any, conditions ...modulekit.Condition) error {
	query, err := s.apply(ctx, conditions)
	if err != nil {
		return err
	}
	return translateError(query.First(dest).Error)
}

func (s *GormStore) Count(ctx context.Context, model any, conditions ...modulekit.Condition) (int64, error) {
	query, err := s.apply(ctx, conditions)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := query.Model(model).Count(&n).Error; err != nil {
		return 0, translateError(err)
	}
	return n, nil
}

func (s *GormStore) Create(ctx context.Context, value any) error {
	return translateError(s.db.WithContext(ctx).Create(value).Error)
}

func (s *GormStore) Update(ctx context.Context, model any, changes map[string]any, conditions ...modulekit.Condition) error {
	if len(conditions) == 0 {
		// Same reasoning as Delete: an unconditional update is far more often a
		// forgotten predicate than an intended whole-table rewrite.
		return fmt.Errorf("%w: update", modulekit.ErrUnconditional)
	}
	query, err := s.apply(ctx, conditions)
	if err != nil {
		return err
	}
	return translateError(query.Model(model).Updates(changes).Error)
}

func (s *GormStore) Delete(ctx context.Context, model any, conditions ...modulekit.Condition) error {
	if len(conditions) == 0 {
		return fmt.Errorf("%w: delete", modulekit.ErrUnconditional)
	}
	query, err := s.apply(ctx, conditions)
	if err != nil {
		return err
	}
	return translateError(query.Delete(model).Error)
}

// Transact runs fn inside a transaction over this module's data.
//
// GORM joins an existing transaction when Transaction is called on a handle
// that is already transactional, so a nested Transact does not open a second
// one and the outer rollback still covers everything.
func (s *GormStore) Transact(ctx context.Context, fn func(tx modulekit.Store) error) error {
	return translateError(s.db.WithContext(ctx).Transaction(func(txDB *gorm.DB) error {
		return fn(&GormStore{db: txDB})
	}))
}

// apply builds a query with the conditions attached. The tenant predicate is
// not added here — the isolation callbacks add it to every statement.
func (s *GormStore) apply(ctx context.Context, conditions []modulekit.Condition) (*gorm.DB, error) {
	query := s.db.WithContext(ctx)
	for _, condition := range conditions {
		expr, args, err := buildCondition(condition)
		if err != nil {
			return nil, err
		}
		query = query.Where(expr, args...)
	}
	return query, nil
}

// buildCondition renders one Condition as a parameterised fragment.
//
// The column name is interpolated because it is not a bindable parameter, so it
// is validated first: field names come from module code rather than request
// input, but an unvalidated identifier reaching SQL is not a property worth
// relying on module authors to preserve.
func buildCondition(condition modulekit.Condition) (string, []any, error) {
	if err := validateFieldName(condition.Field); err != nil {
		return "", nil, err
	}

	switch condition.Op {
	case modulekit.OpEqual, modulekit.OpNotEqual,
		modulekit.OpGreaterThan, modulekit.OpGreaterEqual,
		modulekit.OpLessThan, modulekit.OpLessEqual,
		modulekit.OpLike:
		return fmt.Sprintf("%s %s ?", condition.Field, condition.Op), []any{condition.Value}, nil
	case modulekit.OpIn:
		return fmt.Sprintf("%s IN ?", condition.Field), []any{condition.Value}, nil
	default:
		return "", nil, fmt.Errorf("%w: %q", modulekit.ErrUnsupportedOperator, condition.Op)
	}
}

// validateFieldName accepts only bare column identifiers: letters, digits, and
// underscores, not starting with a digit. Anything else — a dotted name, a
// function call, an expression — is refused, since the interface deliberately
// does not offer raw SQL.
func validateFieldName(field string) error {
	if field == "" {
		return fmt.Errorf("%w: empty field name", modulekit.ErrUnsupportedOperator)
	}
	for i, r := range field {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if isLetter || r == '_' || (isDigit && i > 0) {
			continue
		}
		return fmt.Errorf("%w: invalid field name %q", modulekit.ErrUnsupportedOperator, field)
	}
	return nil
}

// GormConfig returns the GORM configuration this adapter requires.
//
// TranslateError is not optional: without it a constraint violation arrives as
// the driver's raw message and translateError cannot recognise it, so
// modulekit.Idempotent would treat a duplicate delivery as an unknown failure
// and retry forever. Opening a connection this adapter will wrap means using
// this config.
func GormConfig() *gorm.Config {
	return &gorm.Config{TranslateError: true}
}

// translateError maps engine errors onto the SDK's sentinels, so that module
// code tests conditions without importing GORM.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return modulekit.ErrNotFound
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// Recognising this depends on gorm.Config.TranslateError being set when
		// the connection is opened; without it the driver's raw message arrives
		// instead and nothing here matches. TestDuplicateKeyIsTranslated fails
		// if that config is ever dropped.
		return fmt.Errorf("%w: %v", modulekit.ErrDuplicate, err)
	}
	return err
}
