package modulekit

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

// A module needs its tables to exist before it can run, and until now only the
// kernel could create them: migrations were embedded in the kernel package, so a
// community module's schema had to be added to the kernel repository. That is
// the one thing the framework model forbids — Sannad is a dependency in
// somebody's go.mod, not a project they fork.
//
// So a module carries its own migrations and the kernel applies them. The kernel
// never reads what is inside: ADR 0004 forbids one module reading another's
// tables, and that includes the kernel reading a module's. It executes the SQL
// and records the version.
//
// Each module gets its own version table. Two modules from unrelated authors
// will both have a migration numbered 00001, and a shared version table would
// make the second one look already-applied — the module would start against a
// database with no tables and fail on its first query.

// Migrator is implemented by a module that ships its own schema.
//
// Optional: a module using StorageNone or StorageOwn has nothing for the kernel
// to migrate, and a StorageKernel module whose tables are created some other way
// need not implement it either.
type Migrator interface {
	// Migrations returns the module's migration files.
	//
	// Typically an embed.FS:
	//
	//	//go:embed migrations/*.sql
	//	var migrationFiles embed.FS
	//
	//	func (m *Module) Migrations() (fs.FS, error) {
	//	    return fs.Sub(migrationFiles, "migrations")
	//	}
	//
	// The files follow goose's format and numbering, which the kernel already
	// uses for its own schema. Returning an error stops startup: a module whose
	// schema cannot be read must not run against a database it expects to have
	// tables in.
	Migrations() (fs.FS, error)
}

// MigrationTableFor returns the version-tracking table name for a module.
//
// Derived from the module ID rather than chosen by the module, so two modules
// cannot pick the same name — accidentally or otherwise. A module that could
// name its own version table could claim another's and make its migrations look
// applied.
//
// com.acme.parties becomes schema_version_com_acme_parties.
func MigrationTableFor(moduleID string) (string, error) {
	if moduleID == "" {
		return "", fmt.Errorf("modulekit: module ID is required to derive a migration table")
	}
	if !moduleIDPattern.MatchString(moduleID) {
		return "", fmt.Errorf("modulekit: module ID %q must be lowercase reverse-DNS "+
			"(letters, digits, dots, hyphens) to derive a migration table", moduleID)
	}

	safe := strings.NewReplacer(".", "_", "-", "_").Replace(moduleID)
	name := "schema_version_" + safe

	// Postgres truncates identifiers at 63 bytes, and a truncated name could
	// collide with another module's. Refusing is better than two modules quietly
	// sharing a version table.
	if len(name) > 63 {
		return "", fmt.Errorf("modulekit: module ID %q is too long: its version table "+
			"name would exceed the 63-character identifier limit", moduleID)
	}
	return name, nil
}

// moduleIDPattern constrains what may appear in a derived table name.
//
// The module ID reaches SQL as an identifier, so it is validated rather than
// escaped: a module ID has no business containing a quote, a space, or a
// semicolon, and refusing is safer than trying to render one harmless.
var moduleIDPattern = regexp.MustCompile(`^[a-z0-9]+([.-][a-z0-9]+)*$`)
