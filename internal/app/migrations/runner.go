package migrations

import (
	"database/sql"
	"embed"
	"fmt"
	"strings"

	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var migrationFiles embed.FS

// RunMigrations applies all pending goose migrations using the provided *sql.DB.
// The dialect is inferred from the dsn: "sqlite" for SQLite (memory/file), "postgres" otherwise.
func RunMigrations(db *sql.DB, dsn string) error {
	dialect := detectDialect(dsn)
	goose.SetBaseFS(migrationFiles)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("migrations: set dialect %q: %w", dialect, err)
	}
	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("migrations: apply: %w", err)
	}
	return nil
}

func detectDialect(dsn string) string {
	lower := strings.ToLower(dsn)
	if lower == ":memory:" || strings.HasPrefix(lower, "file:") || strings.HasSuffix(lower, ".db") || strings.HasSuffix(lower, ".sqlite") {
		return "sqlite3"
	}
	return "postgres"
}
