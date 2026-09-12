package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/pressly/goose/v3"
	"github.com/sannados/sannad/pkg/modulekit"
)

// RunModuleMigrations applies one module's schema.
//
// The kernel executes the SQL and records the version; it never inspects what
// the migration creates. ADR 0004 forbids one module reading another's tables,
// and the kernel is not an exception — a module's schema is private to it, and
// this is the mechanism that lets it stay that way while still existing.
//
// Each module gets its own version table, derived from its ID. Two modules from
// unrelated authors will both number a migration 00001; sharing a version table
// would make the second look already-applied, and the module would start against
// a database with none of its tables.
func RunModuleMigrations(ctx context.Context, db *sql.DB, dsn string, module modulekit.Module) error {
	migrator, ok := module.(modulekit.Migrator)
	if !ok {
		// Shipping schema is optional. A module using StorageNone or StorageOwn
		// has nothing for the kernel to migrate.
		return nil
	}

	descriptor := module.Descriptor()

	table, err := modulekit.MigrationTableFor(descriptor.ID)
	if err != nil {
		return err
	}

	files, err := migrator.Migrations()
	if err != nil {
		// A module whose schema cannot be read must not run against a database
		// it expects to have tables in.
		return fmt.Errorf("migrations: read schema for module %s: %w", descriptor.ID, err)
	}
	if files == nil {
		return nil
	}

	dialect, err := gooseDialect(dsn)
	if err != nil {
		return err
	}

	// A provider rather than the package-level API: goose's globals carry the
	// base filesystem and table name process-wide, so migrating two modules in
	// sequence would have the second inherit the first's settings.
	provider, err := goose.NewProvider(dialect, db, files, goose.WithTableName(table))
	if err != nil {
		return fmt.Errorf("migrations: prepare module %s: %w", descriptor.ID, err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrations: apply module %s: %w", descriptor.ID, err)
	}

	if len(results) > 0 {
		slog.InfoContext(ctx, "module migrations applied",
			"module", descriptor.ID, "count", len(results), "version_table", table)
	}
	return nil
}

// gooseDialect maps a DSN to the goose dialect constant.
//
// Separate from detectDialect because the provider API takes a typed dialect
// while the package-level API takes a string, and having one function return
// both invites the two paths to disagree about what a DSN means.
func gooseDialect(dsn string) (goose.Dialect, error) {
	switch detectDialect(dsn) {
	case "sqlite3":
		return goose.DialectSQLite3, nil
	case "postgres":
		return goose.DialectPostgres, nil
	default:
		return "", fmt.Errorf("migrations: no goose dialect for %q", dsn)
	}
}
