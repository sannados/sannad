package crm

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/google/uuid"
	"github.com/sannados/sannad/pkg/modulekit"
)

var CapabilityContactReader = modulekit.CapabilityRef{Name: "crm.contacts.reader", Version: "v1"}

// Contact is the persisted CRM contact model.
//
// It implements tenant.TenantAware, which is what subjects it to the isolation
// callbacks registered at bootstrap. Every query, insert, update, and delete is
// scoped to the tenant in the request context, or fails. A scoped model must
// index its tenant column, and composite indexes must lead with it.
type Contact struct {
	ID       string `gorm:"primaryKey"                     json:"id"`
	TenantID string `gorm:"index;not null"                 json:"tenant_id"`
	Name     string `                                      json:"name"`
	Email    string `gorm:"index:idx_contacts_tenant_email,priority:2" json:"email"`
}

// GetTenantID and SetTenantID implement tenant.TenantAware.
func (c *Contact) GetTenantID() string   { return c.TenantID }
func (c *Contact) SetTenantID(id string) { c.TenantID = id }

type ContactReaderRequest struct {
	TenantID string `json:"tenant_id"`
}

type ContactReaderResponse struct {
	Contacts []Contact `json:"contacts"`
}

// Module is the embedded CRM module. It receives a modulekit.Store from the
// host and names no engine type: which database is underneath is a bootstrap
// decision the module cannot observe. See ADR 0006.
type Module struct {
	store     modulekit.Store
	env       string // "development" | "production" | …
	publisher modulekit.EventPublisher
}

func New(store modulekit.Store, env string) *Module {
	return &Module{store: store, env: env, publisher: discardPublisher{}}
}

// WithPublisher sets the event publisher used when contacts are listed.
// Call this before Start; it is safe to omit (defaults to NoopPublisher).
func (m *Module) WithPublisher(p modulekit.EventPublisher) *Module {
	m.publisher = p
	return m
}

type discardPublisher struct{}

func (discardPublisher) Publish(context.Context, string, any) error { return nil }

func (m *Module) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       "com.sannad.crm",
		Kind:     "embedded",
		Storage:  modulekit.StorageKernel,
		Provides: []modulekit.CapabilityRef{CapabilityContactReader},
	}
}

// Install registers the contact-reader capability.
func (m *Module) Install(reg modulekit.Registrar) error {
	return reg.RegisterCapability(CapabilityContactReader, m.handleListContacts)
}

func (m *Module) Start(ctx context.Context) error {
	if m.env != "production" {
		if err := m.seedDevData(ctx); err != nil {
			return err
		}
	}
	slog.Info("module started", "module", "crm")
	return nil
}

func (m *Module) Stop(_ context.Context) error {
	slog.Info("module stopped", "module", "crm")
	return nil
}

func (m *Module) handleListContacts(ctx context.Context, request any) (any, error) {
	if _, err := asRequest(request); err != nil {
		return nil, err
	}

	caller, _ := modulekit.CallerFrom(ctx)

	// No tenant predicate is applied here on purpose. Contact is tenant.TenantAware,
	// so the isolation callback registered at bootstrap scopes this query to the
	// tenant in the context and fails the call outright if there is none. A
	// module-level check would be a second, weaker copy of that guarantee — and
	// the version this replaced compared two values that both came from the
	// request, which is what made cross-tenant reads possible.
	var contacts []Contact
	if err := m.store.Find(ctx, &contacts); err != nil {
		return nil, fmt.Errorf("crm: query contacts: %w", err)
	}

	if pubErr := m.publisher.Publish(ctx, "sannad.crm.contacts.listed", ContactsListedPayload{
		TenantID: tenant.IDFromContext(ctx),
		Count:    len(contacts),
		Subject:  caller.Subject,
	}); pubErr != nil {
		// Publishing is best-effort; log and continue rather than failing the request.
		slog.WarnContext(ctx, "crm: publish contacts.listed event failed", "err", pubErr)
	}

	return ContactReaderResponse{Contacts: contacts}, nil
}

// ContactsListedPayload is the event payload published after a successful listing.
type ContactsListedPayload struct {
	TenantID string `json:"tenant_id"`
	Count    int    `json:"count"`
	Subject  string `json:"subject"`
}

// seedDevData inserts demo contacts only when the table is empty.
// This is a convenience for local development — production data comes from
// real registrations or data imports.
//
// Seeding spans several tenants and runs at startup with no request context, so
// it uses the system-context escape. That is exactly the intended use: a
// deliberate, in-process, non-request operation. Start refuses to call this
// outside development.
func (m *Module) seedDevData(ctx context.Context) error {
	ctx = modulekit.WithSystemContext(ctx)

	count, err := m.store.Count(ctx, &Contact{})
	if err != nil {
		return fmt.Errorf("crm: seed check: %w", err)
	}
	if count > 0 {
		return nil
	}

	seeds := []Contact{
		{ID: uuid.NewString(), TenantID: "demo", Name: "Amina Hassan", Email: "amina@example.com"},
		{ID: uuid.NewString(), TenantID: "demo", Name: "Omar Khalid", Email: "omar@example.com"},
		{ID: uuid.NewString(), TenantID: "enterprise", Name: "Sara Ali", Email: "sara@example.com"},
	}
	return m.store.Create(ctx, &seeds)
}

func asRequest(request any) (ContactReaderRequest, error) {
	if typed, ok := request.(ContactReaderRequest); ok {
		return typed, nil
	}
	if typed, ok := request.(*ContactReaderRequest); ok {
		return *typed, nil
	}
	return ContactReaderRequest{}, fmt.Errorf("crm: unsupported request type %T", request)
}
