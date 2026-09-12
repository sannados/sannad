package announcements

import (
	"context"
	"fmt"

	"github.com/getkayan/kayan/core/tenant"

	"github.com/sannados/sannad/pkg/modulekit"
)

// CapabilityBoardReader shows how a community module can expose an in-process capability.
var CapabilityBoardReader = modulekit.CapabilityRef{Name: "announcements.board.reader", Version: "v1"}

type Announcement struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

type BoardReaderRequest struct {
	TenantID string `json:"tenant_id"`
}

type BoardReaderResponse struct {
	Announcements []Announcement `json:"announcements"`
}

// Module is a reference embedded module that can live in this repo or a separate community repo.
type Module struct {
	announcements []Announcement
}

func New() *Module {
	return &Module{
		announcements: []Announcement{
			{
				ID:       "announcement_1",
				TenantID: "demo",
				Title:    "Quarterly Town Hall",
				Body:     "Town hall starts Thursday at 10:00 AM.",
			},
			{
				ID:       "announcement_2",
				TenantID: "demo",
				Title:    "CRM Migration Window",
				Body:     "Contacts migration window opens next Monday.",
			},
			{
				ID:       "announcement_3",
				TenantID: "school",
				Title:    "Term Opens",
				Body:     "Student schedules are published on Sunday.",
			},
		},
	}
}

func (moduleValue *Module) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       "com.community.announcements",
		Kind:     "embedded",
		Provides: []modulekit.CapabilityRef{CapabilityBoardReader},
		// StorageOwn, not StorageNone: this module holds per-tenant data, just
		// not in the kernel's store. That makes tenant isolation this module's
		// responsibility, and makes it invisible to the startup audit — which is
		// exactly what the declaration tells an operator.
		Storage: modulekit.StorageOwn,
	}
}

func (moduleValue *Module) Install(reg modulekit.Registrar) error {
	return reg.RegisterCapability(CapabilityBoardReader, moduleValue.handleListAnnouncements)
}

func (moduleValue *Module) Start(_ context.Context) error {
	return nil
}

func (moduleValue *Module) Stop(_ context.Context) error {
	return nil
}

func (moduleValue *Module) handleListAnnouncements(ctx context.Context, request any) (any, error) {
	// The request is still validated for shape — a caller sending the wrong type
	// should learn that — but nothing in it selects data any more. The one field
	// it carried, TenantID, was the vulnerability.
	if _, err := asRequest(request); err != nil {
		return nil, err
	}

	// The tenant comes from the context, never from the request.
	//
	// This module keeps its data outside the kernel's store, so nothing applies
	// isolation underneath it — the check here is the only thing standing between
	// one tenant and another's announcements. That is the cost of StorageOwn, and
	// it is why the declaration exists.
	//
	// A module on the kernel store cannot write this wrong: the adapter scopes
	// every query and fails closed with no tenant. Off it, this is a claim the
	// author has to make good on, and an earlier version of this file did not:
	// it read the tenant from the request, so a caller could name any tenant it
	// liked, and an empty one returned every tenant's data.
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, fmt.Errorf("%w: announcements are tenant-scoped", modulekit.ErrForbidden)
	}

	response := BoardReaderResponse{Announcements: make([]Announcement, 0)}
	for _, announcement := range moduleValue.announcements {
		if announcement.TenantID == tenantID {
			response.Announcements = append(response.Announcements, announcement)
		}
	}

	return response, nil
}

func asRequest(request any) (BoardReaderRequest, error) {
	if typed, ok := request.(BoardReaderRequest); ok {
		return typed, nil
	}

	if typed, ok := request.(*BoardReaderRequest); ok {
		return *typed, nil
	}

	return BoardReaderRequest{}, fmt.Errorf("announcements: unsupported request type %T", request)
}
