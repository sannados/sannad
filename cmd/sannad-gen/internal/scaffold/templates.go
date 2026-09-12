package scaffold

// The templates below are the real output of this tool. Each rule they encode is
// one an author would otherwise have to know from an ADR, and each has a failure
// mode that does not show up in the module's own tests.

const contractTemplate = `package {{.Package}}

import "github.com/sannados/sannad/pkg/modulekit"

// The contract: the types this module exchanges with callers it does not know.
//
// These are kept in their own file because they are the module's public surface.
// Everything else here can be rewritten freely; changing these breaks callers.

// {{.CapabilityConst}} is this module's capability. Callers resolve it by ref and
// never import this package, which is what lets the same capability later be
// served from WASM or an external app without a caller changing.
var {{.CapabilityConst}} = modulekit.CapabilityRef{
	Name:    "{{.Capability}}",
	Version: "{{.Version}}",
}

// Hook{{.TypePrefix}}BeforeWrite lets other modules participate in this one's
// flow without this module knowing they exist.
//
// Offering a hook point is a public API commitment: removing or changing it is a
// breaking change. It is declared from the first commit because retrofitting an
// extension point after modules depend on the flow is the change that is not
// possible to make compatibly.
var Hook{{.TypePrefix}}BeforeWrite = modulekit.HookRef{
	Name:    "{{.HookRef}}",
	Version: "{{.Version}}",
}

// {{.TypePrefix}}Request is the request contract.
//
// It carries no tenant ID, deliberately. The tenant travels in the context and is
// resolved from the caller's credentials at the edge; a tenant field in a request
// is a field a caller can set to someone else's tenant.
type {{.TypePrefix}}Request struct {
	// Limit caps the result set. Replace these fields with the real contract.
	Limit int ` + "`json:\"limit\"`" + `
}

// {{.TypePrefix}}Response is the response contract.
type {{.TypePrefix}}Response struct {
	Items []{{.TypePrefix}} ` + "`json:\"items\"`" + `
}

// {{.TypePrefix}} is the record this module owns.
//
// It implements the tenant-aware interface, which is what subjects it to the isolation
// callbacks. Every query is scoped to the tenant in the context, or fails. A
// model that omits this compiles and runs completely unscoped, and nothing
// reports it — so it is generated rather than left to be remembered.
//
// No engine struct tags. Schema belongs in a migration: a tag names an engine
// without importing one, which couples the module to a database the SDK is
// specifically designed not to name.
type {{.TypePrefix}} struct {
	ID       string ` + "`json:\"id\"`" + `
	TenantID string ` + "`json:\"tenant_id\"`" + `
	Name     string ` + "`json:\"name\"`" + `
}

func (r *{{.TypePrefix}}) GetTenantID() string   { return r.TenantID }
func (r *{{.TypePrefix}}) SetTenantID(id string) { r.TenantID = id }

// TableName pins the storage table. Prefixed with the module so two modules
// cannot collide in a shared database.
func ({{.TypePrefix}}) TableName() string { return "{{.Table}}" }
`

const moduleTemplate = `package {{.Package}}

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"time"

	"github.com/sannados/sannad/pkg/modulekit"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is an embedded Go module: it runs in the kernel process with full
// privileges. See ADR 0001 for what that means for trust.
//
// It imports pkg/modulekit and nothing else from the kernel. That is not a style
// preference — internal packages carry no compatibility promise, and a module
// importing one is a module that breaks on a kernel release. boundary_test.go
// enforces it.
type Module struct {
	store    modulekit.Store
	dispatch modulekit.HookDispatcher
}

// New builds the module. The store is handed in: which database is underneath is
// a bootstrap decision this module cannot observe, and must not be able to.
func New(store modulekit.Store) *Module {
	return &Module{store: store}
}

func (m *Module) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:   "{{.ModuleID}}",
		Kind: "embedded",
		// StorageKernel: this module uses the Store handed to it, so tenant
		// isolation is applied underneath every query and cannot be forgotten.
		// See the README for what changes if you manage your own storage.
		Storage: modulekit.StorageKernel,
		Provides: []modulekit.CapabilityRef{ {{.CapabilityConst}} },
		Offers: []modulekit.HookPoint{
			{
				Ref:  Hook{{.TypePrefix}}BeforeWrite,
				Kind: modulekit.KindValidator,
				// fail_closed: a validator exists to refuse, so a subscriber
				// that errors must block the write. fail_open here would mean a
				// broken guard silently permits what it was added to prevent.
				Policy:      modulekit.FailClosed,
				Timeout:     time.Second,
				Description: "Veto a {{.TypePrefix}} before it is written.",
			},
		},
	}
}

// ScopedModels enrolls this module's tenant-owned tables in isolation when the
// application registers the module. No separate bootstrap call is required.
func (m *Module) ScopedModels() []modulekit.TenantAware {
	return []modulekit.TenantAware{&{{.TypePrefix}}{}}
}

// Install registers the capability and the hook surface.
func (m *Module) Install(reg modulekit.Registrar) error {
	// The dispatch surface is how this module fires the hook point it owns.
	// A Registrar without one means hooks are unavailable in this host, which
	// Write handles rather than assuming.
	if dispatch, ok := modulekit.HookDispatcherFrom(reg); ok {
		m.dispatch = dispatch
	}

	// HandleTyped, not a raw handler. The typed value is what crosses a tier
	// boundary, so writing the handler this way is what keeps the module
	// portable to WASM or an external app later.
	return modulekit.HandleTyped(reg, {{.CapabilityConst}}, m.handleList)
}

// Migrations returns this module's schema, which travels with the module rather
// than living in the kernel repository. The kernel applies it and never reads
// what is inside.
//
// fs.Sub strips the directory prefix: goose reads from the root of the
// filesystem it is handed, so passing the embed.FS directly would find nothing
// and silently apply no migrations.
func (m *Module) Migrations() (fs.FS, error) {
	return fs.Sub(migrationFiles, "migrations")
}

func (m *Module) Start(ctx context.Context) error { return nil }
func (m *Module) Stop(ctx context.Context) error  { return nil }

// handleList is the capability implementation.
func (m *Module) handleList(ctx context.Context, req {{.TypePrefix}}Request) ({{.TypePrefix}}Response, error) {
	// No tenant predicate here on purpose. {{.TypePrefix}} is tenant-aware, so
	// the isolation callback scopes this query to the context's tenant and fails
	// the call if there is none. A module-level check would be a second, weaker
	// copy of that guarantee.
	var items []{{.TypePrefix}}
	if err := m.store.Find(ctx, &items); err != nil {
		return {{.TypePrefix}}Response{}, fmt.Errorf("{{.Package}}: query: %w", err)
	}
	return {{.TypePrefix}}Response{Items: items}, nil
}

// Write persists a record, giving other modules a chance to veto it first.
//
// The hook runs inside the transaction so a veto and the write cannot disagree:
// a subscriber that refuses must leave nothing behind.
func (m *Module) Write(ctx context.Context, item *{{.TypePrefix}}) error {
	return m.store.Transact(ctx, func(tx modulekit.Store) error {
		if m.dispatch != nil {
			if err := modulekit.ValidateTyped(ctx, m.dispatch, Hook{{.TypePrefix}}BeforeWrite, item); err != nil {
				return err
			}
		}
		return tx.Create(ctx, item)
	})
}
`

const moduleTestTemplate = `package {{.Package}}_test

import (
	"testing"

	"github.com/sannados/sannad/pkg/modulekit"

	{{.Package}} "{{.ModuleID}}"
)

// TestContractsSurviveATierChange is the test most worth keeping.
//
// This module runs in-process today, where payloads are handed over by pointer
// and never encoded. A contract holding a func, a channel, or unexported state
// passes every other test here and fails only when the module moves to WASM or
// behind the gateway — the one moment the design promised nothing would change.
func TestContractsSurviveATierChange(t *testing.T) {
	for _, contract := range []any{
		{{.Package}}.{{.TypePrefix}}Request{},
		{{.Package}}.{{.TypePrefix}}Response{},
		{{.Package}}.{{.TypePrefix}}{},
	} {
		if err := modulekit.AssertPortable(contract); err != nil {
			t.Errorf("%T cannot cross a tier boundary: %v", contract, err)
		}
	}
}

// TestModelIsTenantAware fails if the model stops being scoped.
//
// A model that does not implement the interface is queried completely unscoped,
// across every tenant, and nothing at runtime reports it. That is the failure
// this test exists to make loud.
func TestModelIsTenantAware(t *testing.T) {
	var model any = &{{.Package}}.{{.TypePrefix}}{}
	if _, ok := model.(interface {
		GetTenantID() string
		SetTenantID(string)
	}); !ok {
		t.Fatal("{{.TypePrefix}} is not tenant-aware, so every query against it runs across all tenants")
	}
}

// TestRequestCarriesNoTenant fails if a tenant field is added to the request.
//
// The tenant is resolved from the caller's credentials at the edge. A tenant in
// the request is a value the caller controls, which is the shape of a
// cross-tenant read.
func TestRequestCarriesNoTenant(t *testing.T) {
	req := {{.Package}}.{{.TypePrefix}}Request{}
	if hasField(req, "TenantID") || hasField(req, "Tenant") {
		t.Fatal("request carries a tenant field; the tenant must come from context, not the caller")
	}
}
`

const boundaryTestTemplate = `package {{.Package}}_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestModuleImportsOnlyThePublicSDK keeps this module installable across kernel
// releases: internal/ packages carry no compatibility promise, so a module that
// imports one breaks on a kernel upgrade, in the operator's build rather than
// the author's.
//
// When this module lives in its own Go module, the compiler already refuses an
// internal/ import and this test is a second line of defence. It becomes the
// only one if the module is ever vendored into the kernel repository, where
// internal/ is importable and the compiler stops objecting — which is exactly
// the move that makes the mistake easy to commit without noticing.
func TestModuleImportsOnlyThePublicSDK(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}

	fset := token.NewFileSet()
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s", name, imp.Path.Value)
			}
			if strings.Contains(path, "/internal/") {
				t.Errorf("%s imports %s; modules may use pkg/modulekit only", name, path)
			}
		}
	}
}

// hasField reports whether a struct has a field by name.
func hasField(v any, name string) bool {
	t := reflect.TypeOf(v)
	if t.Kind() != reflect.Struct {
		return false
	}
	_, ok := t.FieldByName(name)
	return ok
}
`

const readmeTemplate = `# {{.ModuleID}}

Embedded Sannad module providing ` + "`{{.Capability}}@{{.Version}}`" + `.

## Install

` + "```go" + `
mod := {{.Package}}.New(store)
if err := app.RegisterModule(mod); err != nil {
	return err
}
` + "```" + `

## What the generated tests protect

These are not boilerplate. Each covers a failure that does not appear in ordinary
use and shows up later, somewhere else:

- **Tier portability** — a contract holding a func, a channel, or unexported state
  works perfectly in-process and cannot cross to WASM or an external app.
- **Tenant awareness** — a model that stops implementing the interface is queried
  across every tenant, and nothing at runtime reports it.
- **No tenant in the request** — a tenant field is a value the caller controls.
- **Public SDK only** — importing ` + "`internal/`" + ` compiles now and breaks on a
  kernel release, in the operator's build rather than yours.

## Storage

This module declares StorageKernel, meaning it uses the Store the kernel hands it. That
buys two things:

- **Tenant isolation you cannot forget.** Every query is scoped to the tenant in the
  context, and an operation with no tenant fails rather than running unscoped.
- **The SDK helpers.** Idempotent, RunSaga, and Project all write their state in the same
  transaction as your work, which requires the kernel store.

Switching to StorageOwn is supported and sometimes right — wrapping an existing
datastore, or data living in a system the kernel has no business knowing about. Be clear
about what it costs:

- Tenant isolation becomes **your** responsibility, and nothing outside your module can
  verify you got it right.
- The SDK helpers above are unavailable.
- The startup audit inspects database tables, so it cannot see your data at all. It
  reports your module as unverified rather than pretending otherwise.

StorageNone is for a module that persists nothing.

## Schema

This module names no engine and carries no engine struct tags. Its table
(` + "`{{.Table}}`" + `) belongs in a migration. A struct tag would couple the
module to one database without importing it — which is the coupling the storage
boundary exists to prevent.

## Extension

` + "`{{.HookRef}}@{{.Version}}`" + ` is offered as a **fail_closed validator**:
subscribers may veto a write, and a subscriber that errors blocks it. Offering it
is a public API commitment — removing or changing it is a breaking change.

## Other tiers

This generator scaffolds an embedded Go module — full process trust, in-process. The same
capability contract also runs sandboxed (WASM) or out-of-process (an external app calling
in over HTTP/gRPC); see ` + "`docs/architecture/plugin-tiers.md`" + ` for how the three
compare and ` + "`docs/architecture/wasm-authoring.md`" + ` for a complete guide to writing,
building, and registering a WASM guest if untrusted third-party code is what you need to run.
`

const migrationTemplate = `-- +goose Up
-- This module's own schema. It ships with the module; the kernel applies it and
-- never reads what is inside.
--
-- Numbering starts at 00001 for every module. Two modules from unrelated authors
-- will collide here, which is harmless: each gets its own version table derived
-- from its module ID.
CREATE TABLE IF NOT EXISTS {{.Table}} (
    id        TEXT NOT NULL,
    -- Every scoped table needs this column and an index leading with it, or the
    -- isolation callbacks have nothing to scope on.
    tenant_id TEXT NOT NULL,
    name      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_{{.Table}}_tenant ON {{.Table}} (tenant_id);

-- +goose Down
DROP INDEX IF EXISTS idx_{{.Table}}_tenant;
DROP TABLE IF EXISTS {{.Table}};
`
