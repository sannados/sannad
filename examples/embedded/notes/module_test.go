package notes_test

import (
	"path/filepath"
	"testing"

	"github.com/getkayan/kayan/core/tenant"
	sannad "github.com/sannados/sannad"
	"github.com/sannados/sannad/examples/embedded/notes"
	"github.com/sannados/sannad/pkg/modulekit"
)

// Until this module existed, a community module could not create its own tables:
// migrations were embedded in the kernel package, so shipping schema meant
// adding it to the kernel repository. These tests go through real bootstrap
// because that is where the migration is applied — a unit test of the migrator
// would not prove a module can actually start with tables it brought itself.

func appWithNotes(t *testing.T) (*sannad.App, *notes.Module) {
	t.Helper()
	cfg := sannad.Config{
		Env:           "development",
		KernelAddr:    ":0",
		GatewayAddr:   ":0",
		DatabaseDSN:   filepath.Join(t.TempDir(), "notes-test.db"),
		SessionSecret: "notes-test-session-secret-long-enough",
		KayanIssuer:   "http://localhost:8080",
		CORSOrigins:   "*",
	}
	app, err := sannad.New(cfg)
	if err != nil {
		t.Fatalf("sannad.New: %v", err)
	}

	module := notes.New(app.Store())
	// Registration is what applies the module's migration.
	if err := app.RegisterModule(module); err != nil {
		t.Fatalf("RegisterModule: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(t.Context()) })
	return app, module
}

// TestModuleCreatesItsOwnTables is the blocker this closes. Nothing in the
// kernel repository knows the notes table exists; the module brought it.
func TestModuleCreatesItsOwnTables(t *testing.T) {
	app, module := appWithNotes(t)
	ctx := tenant.WithTenantID(t.Context(), "acme")

	// A query against a table the kernel never declared. Before module-shipped
	// migrations this failed with "no such table".
	if _, err := module.Add(ctx, "first note"); err != nil {
		t.Fatalf("write to a module-owned table: %v", err)
	}

	resp, err := modulekit.CallTyped[notes.ListRequest, notes.ListResponse](
		ctx, app, notes.CapabilityNoteReader, notes.ListRequest{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(resp.Notes) != 1 || resp.Notes[0].Body != "first note" {
		t.Fatalf("unexpected result: %+v", resp.Notes)
	}
}

// TestModuleTablesAreTenantScoped: a module shipping its own schema still gets
// the kernel's isolation, because it declared StorageKernel and its model is
// tenant-aware. Bringing the schema does not mean bringing the isolation.
func TestModuleTablesAreTenantScoped(t *testing.T) {
	app, module := appWithNotes(t)

	acme := tenant.WithTenantID(t.Context(), "acme")
	other := tenant.WithTenantID(t.Context(), "other")

	if _, err := module.Add(acme, "acme's note"); err != nil {
		t.Fatalf("add: %v", err)
	}

	resp, err := modulekit.CallTyped[notes.ListRequest, notes.ListResponse](
		other, app, notes.CapabilityNoteReader, notes.ListRequest{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(resp.Notes) != 0 {
		t.Fatalf("another tenant read %d notes from a module-owned table", len(resp.Notes))
	}
}

// TestMigrationsAreIdempotent. Registration runs them every start, so a restart
// must not re-apply or fail.
func TestMigrationsAreIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "restart-test.db")
	cfg := sannad.Config{
		Env: "development", KernelAddr: ":0", GatewayAddr: ":0",
		DatabaseDSN:   dsn,
		SessionSecret: "notes-test-session-secret-long-enough",
		KayanIssuer:   "http://localhost:8080", CORSOrigins: "*",
	}

	for run := 0; run < 2; run++ {
		app, err := sannad.New(cfg)
		if err != nil {
			t.Fatalf("run %d: sannad.New: %v", run, err)
		}
		if err := app.RegisterModule(notes.New(app.Store())); err != nil {
			t.Fatalf("run %d: a second start re-applied the module's migration: %v", run, err)
		}
		if err := app.Start(t.Context()); err != nil {
			t.Fatalf("run %d: Start: %v", run, err)
		}
		if err := app.Shutdown(t.Context()); err != nil {
			t.Fatalf("run %d: Shutdown: %v", run, err)
		}
	}
}

// TestVersionTableIsPerModule is what makes colliding version numbers harmless.
//
// Every module numbers its first migration 00001. With one shared version table
// the second module's 00001 would look already-applied, and it would start
// against a database with none of its tables — a failure that surfaces as a
// missing table far from its cause.
func TestVersionTableIsPerModule(t *testing.T) {
	first, err := modulekit.MigrationTableFor("com.example.notes")
	if err != nil {
		t.Fatalf("MigrationTableFor: %v", err)
	}
	second, err := modulekit.MigrationTableFor("com.acme.parties")
	if err != nil {
		t.Fatalf("MigrationTableFor: %v", err)
	}
	if first == second {
		t.Fatalf("two modules share the version table %q", first)
	}
	if first != "schema_version_com_example_notes" {
		t.Fatalf("got %q, want schema_version_com_example_notes", first)
	}
}

// TestMigrationTableRejectsUnsafeModuleIDs. The module ID reaches SQL as an
// identifier, so it is validated rather than escaped: an ID has no business
// containing a quote, a space, or a semicolon.
func TestMigrationTableRejectsUnsafeModuleIDs(t *testing.T) {
	for _, id := range []string{
		"",
		"com.acme.parties; DROP TABLE users",
		`com."acme".parties`,
		"com.acme parties",
		"COM.ACME.PARTIES",
	} {
		if _, err := modulekit.MigrationTableFor(id); err == nil {
			t.Errorf("module ID %q was accepted as a table name", id)
		}
	}
}
