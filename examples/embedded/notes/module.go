// Package notes is a module that ships its own schema.
//
// It exists to demonstrate the one thing a community module could not do until
// now: create its own tables. Migrations used to be embedded in the kernel
// package, so a module's schema had to be added to the kernel repository — which
// meant forking the framework to ship a module, the one thing the framework
// model forbids.
//
// The migration lives beside this file, travels with the module, and the kernel
// applies it without reading what is inside.
package notes

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"time"

	"github.com/google/uuid"
	"github.com/sannados/sannad/pkg/modulekit"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var CapabilityNoteReader = modulekit.CapabilityRef{Name: "notes.reader", Version: "v1"}

// Note is this module's record.
//
// Tenant-aware, so the isolation callbacks scope every query and fail closed
// with no tenant in context. The module writes no tenant predicate itself: that
// is what StorageKernel buys.
type Note struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func (n *Note) GetTenantID() string   { return n.TenantID }
func (n *Note) SetTenantID(id string) { n.TenantID = id }

func (Note) TableName() string { return "notes" }

type ListRequest struct {
	Limit int `json:"limit"`
}

type ListResponse struct {
	Notes []Note `json:"notes"`
}

type Module struct {
	store modulekit.Store
}

func New(store modulekit.Store) *Module {
	return &Module{store: store}
}

func (m *Module) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       "com.example.notes",
		Kind:     "embedded",
		Storage:  modulekit.StorageKernel,
		Provides: []modulekit.CapabilityRef{CapabilityNoteReader},
	}
}

// ScopedModels lets the host enroll this module's tenant-owned tables in the
// isolation layer as part of RegisterModule.
func (m *Module) ScopedModels() []modulekit.TenantAware {
	return []modulekit.TenantAware{&Note{}}
}

// Migrations returns this module's schema.
//
// fs.Sub strips the directory prefix, because goose reads migrations from the
// root of the filesystem it is handed. Handing it the embed.FS directly would
// find nothing and silently apply no migrations — the module would start against
// a database with no tables.
func (m *Module) Migrations() (fs.FS, error) {
	return fs.Sub(migrationFiles, "migrations")
}

func (m *Module) Install(reg modulekit.Registrar) error {
	return modulekit.HandleTyped(reg, CapabilityNoteReader, m.list)
}

func (m *Module) Start(context.Context) error { return nil }
func (m *Module) Stop(context.Context) error  { return nil }

func (m *Module) list(ctx context.Context, req ListRequest) (ListResponse, error) {
	var notes []Note
	// No tenant predicate: Note is tenant-aware, so the adapter scopes this and
	// fails the call outright if there is no tenant in context.
	if err := m.store.Find(ctx, &notes); err != nil {
		return ListResponse{}, fmt.Errorf("notes: query: %w", err)
	}
	if req.Limit > 0 && req.Limit < len(notes) {
		notes = notes[:req.Limit]
	}
	return ListResponse{Notes: notes}, nil
}

// Add writes a note, for tests and for callers that need one.
func (m *Module) Add(ctx context.Context, body string) (Note, error) {
	note := Note{ID: uuid.NewString(), Body: body, CreatedAt: time.Now()}
	if err := m.store.Create(ctx, &note); err != nil {
		return Note{}, fmt.Errorf("notes: create: %w", err)
	}
	return note, nil
}
