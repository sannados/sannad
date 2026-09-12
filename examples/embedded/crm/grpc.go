package crm

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	crmV1 "github.com/sannados/sannad/contracts/gen/go/sannad/crm/v1"
	"github.com/sannados/sannad/pkg/modulekit"
)

// ContactReaderServer bridges the gRPC ContactReader service to the in-process
// capability bus. All method calls are delegated to the crm.contacts.reader
// capability registered in the kernel registry.
//
// This lives beside the module rather than in the kernel: a gRPC service is a
// projection of one module's contract, so the kernel must not name it. Wire it
// in from an entrypoint via App.RegisterGRPCService.
type ContactReaderServer struct {
	crmV1.UnimplementedContactReaderServer
	caller modulekit.Caller
}

// NewContactReaderServer returns a gRPC server for the ContactReader service.
func NewContactReaderServer(caller modulekit.Caller) *ContactReaderServer {
	return &ContactReaderServer{caller: caller}
}

func (s *ContactReaderServer) ListContacts(ctx context.Context, req *crmV1.ListContactsRequest) (*crmV1.ListContactsResponse, error) {
	result, err := s.caller.Call(ctx, CapabilityContactReader, ContactReaderRequest{
		TenantID: req.GetTenantId(),
	})
	if err != nil {
		if errors.Is(err, modulekit.ErrForbidden) {
			return nil, status.Error(codes.PermissionDenied, "access to this tenant is not allowed")
		}
		return nil, status.Errorf(codes.Internal, "crm: list contacts: %v", err)
	}

	resp, ok := result.(ContactReaderResponse)
	if !ok {
		return nil, status.Error(codes.Internal, "crm: unexpected response type")
	}

	protoContacts := make([]*crmV1.Contact, len(resp.Contacts))
	for i, c := range resp.Contacts {
		protoContacts[i] = &crmV1.Contact{
			Id:       c.ID,
			TenantId: c.TenantID,
			Name:     c.Name,
			Email:    c.Email,
		}
	}
	return &crmV1.ListContactsResponse{Contacts: protoContacts}, nil
}
