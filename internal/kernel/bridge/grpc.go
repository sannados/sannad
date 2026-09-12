package bridge

import (
	"context"
	"errors"
	"log/slog"

	kernelV1 "github.com/sannados/sannad/contracts/gen/go/sannad/kernel/v1"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/pkg/modulekit"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCService serves the same dispatch as the HTTP bridge.
//
// Both transports call the same handler through the same registry with the same
// exposure check, because a capability reachable over one and not the other
// would be a security boundary nobody could reason about. The only difference is
// how the payload arrives.
type GRPCService struct {
	kernelV1.UnimplementedCapabilitiesServer

	registry Registry
	call     func(ctx context.Context, ref modulekit.CapabilityRef, payload modulekit.RawPayload) (any, error)
	encode   func(value any) ([]byte, error)
}

// NewGRPCService builds the service. call dispatches into the kernel; encode
// serialises a capability's result.
func NewGRPCService(
	reg Registry,
	call func(ctx context.Context, ref modulekit.CapabilityRef, payload modulekit.RawPayload) (any, error),
	encode func(value any) ([]byte, error),
) *GRPCService {
	return &GRPCService{registry: reg, call: call, encode: encode}
}

// Call invokes one exposed capability.
func (s *GRPCService) Call(ctx context.Context, req *kernelV1.CallRequest) (*kernelV1.CallResponse, error) {
	ref, err := ParseRef(req.GetCapability())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Same rule as HTTP, and the same indistinguishable answer: an unexposed
	// capability and a nonexistent one both come back NotFound, so the service
	// cannot be used to enumerate what a deployment runs internally.
	if !s.registry.IsExposed(ref) {
		return nil, status.Error(codes.NotFound, "unknown capability")
	}

	payload := req.GetPayload()
	if len(payload) == 0 {
		// A capability whose request has only optional fields should be callable
		// with no payload at all.
		payload = []byte("{}")
	}
	if len(payload) > MaxRequestBytes {
		return nil, status.Error(codes.InvalidArgument, "request payload too large")
	}

	result, err := s.call(ctx, ref, modulekit.RawPayload(payload))
	if err != nil {
		code := grpcCodeFor(err)
		// Logged, never returned. A capability error can carry internal state —
		// a query, a path, another tenant's identifier — and this surface is
		// reachable from outside the process.
		slog.WarnContext(ctx, "grpc bridge dispatch failed",
			"capability", ref.Key(), "code", code, "err", err)
		return nil, status.Error(code, publicGRPCMessage(code))
	}

	encoded, err := encodeResult(result, s.encode)
	if err != nil {
		slog.ErrorContext(ctx, "grpc bridge response encoding failed",
			"capability", ref.Key(), "err", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &kernelV1.CallResponse{Payload: encoded}, nil
}

// List returns the capabilities callable from outside the process.
func (s *GRPCService) List(_ context.Context, _ *kernelV1.ListRequest) (*kernelV1.ListResponse, error) {
	refs := s.registry.ListExposed()
	out := make([]*kernelV1.CapabilityRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, &kernelV1.CapabilityRef{Name: ref.Name, Version: ref.Version})
	}
	return &kernelV1.ListResponse{Capabilities: out}, nil
}

// grpcCodeFor maps a capability error to a gRPC code, mirroring StatusFor so the
// two transports cannot disagree about what a sentinel means.
func grpcCodeFor(err error) codes.Code {
	switch {
	case errors.Is(err, modulekit.ErrNotFound):
		return codes.NotFound
	case errors.Is(err, modulekit.ErrForbidden):
		return codes.PermissionDenied
	case errors.Is(err, modulekit.ErrTypeMismatch):
		// The caller sent a payload the contract does not accept: a malformed
		// request, not a server fault.
		return codes.InvalidArgument
	case errors.Is(err, tenancy.ErrNoTenantContext):
		return codes.Unauthenticated
	default:
		return codes.Internal
	}
}

func publicGRPCMessage(code codes.Code) string {
	switch code {
	case codes.NotFound:
		return "not found"
	case codes.PermissionDenied:
		return "forbidden"
	case codes.InvalidArgument:
		return "invalid request for this capability"
	case codes.Unauthenticated:
		return "unauthorized"
	default:
		return "internal error"
	}
}
