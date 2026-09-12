package bootstrap_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	kernelV1 "github.com/sannados/sannad/contracts/gen/go/sannad/kernel/v1"
	"github.com/sannados/sannad/examples/contracts/directory"
	"github.com/sannados/sannad/internal/app/bootstrap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// gRPC parity. The two transports must agree: a capability reachable over one
// and not the other, or subject to a different check on one of them, is a
// security boundary nobody can reason about.
//
// These run against a real gRPC server over an in-memory listener, so the auth
// interceptor is exercised rather than assumed. That matters most for the
// question these tests exist to answer: the interceptor is registered
// server-wide, and the new service was added after it — if it were ever scoped
// per-service, this surface would become an unauthenticated path to every
// exposed capability in the deployment.

func grpcBridgeClient(t *testing.T, app *bootstrap.App) kernelV1.CapabilitiesClient {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	server := app.TestGRPCServer()
	if server == nil {
		t.Fatal("app exposes no gRPC server")
	}

	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return kernelV1.NewCapabilitiesClient(conn)
}

func authed(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// TestGRPCBridgeCallsAnExposedCapability is the parity claim: the same
// capability, the same result, over a different transport.
func TestGRPCBridgeCallsAnExposedCapability(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)
	client := grpcBridgeClient(t, app)

	payload, err := json.Marshal(directory.ListRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, err := client.Call(authed(t.Context(), token), &kernelV1.CallRequest{
		Capability: directory.Reader.Key(),
		Payload:    payload,
	})
	if err != nil {
		t.Fatalf("grpc call: %v", err)
	}

	var out directory.ListResponse
	if err := json.Unmarshal(resp.GetPayload(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.People) != 2 || out.People[0].Name != "Amina" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

// TestGRPCBridgeRequiresAuthentication. Without the interceptor covering this
// service it is an unauthenticated path to every exposed capability.
func TestGRPCBridgeRequiresAuthentication(t *testing.T) {
	app := bridgeApp(t)
	client := grpcBridgeClient(t, app)

	_, err := client.Call(t.Context(), &kernelV1.CallRequest{
		Capability: directory.Reader.Key(),
		Payload:    []byte("{}"),
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated call returned %v, want Unauthenticated", status.Code(err))
	}
}

// TestGRPCBridgeHidesUnexposedCapabilities: exposure is enforced on both
// transports, not only the one that shipped first.
func TestGRPCBridgeHidesUnexposedCapabilities(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)
	client := grpcBridgeClient(t, app)

	_, hiddenErr := client.Call(authed(t.Context(), token), &kernelV1.CallRequest{
		Capability: hiddenRef.Key(),
		Payload:    []byte("{}"),
	})
	if status.Code(hiddenErr) != codes.NotFound {
		t.Fatalf("unexposed capability returned %v, want NotFound", status.Code(hiddenErr))
	}

	// Indistinguishable from a capability that does not exist, so the service
	// cannot enumerate what a deployment runs internally.
	_, missingErr := client.Call(authed(t.Context(), token), &kernelV1.CallRequest{
		Capability: "nothing.at.all@v1",
		Payload:    []byte("{}"),
	})
	if status.Code(missingErr) != status.Code(hiddenErr) {
		t.Fatalf("unexposed returned %v and unknown returned %v; the difference leaks what exists",
			status.Code(hiddenErr), status.Code(missingErr))
	}
	if status.Convert(missingErr).Message() != status.Convert(hiddenErr).Message() {
		t.Fatalf("messages differ: %q vs %q",
			status.Convert(hiddenErr).Message(), status.Convert(missingErr).Message())
	}
}

// TestGRPCBridgeRefusesAnUnversionedReference, matching the HTTP transport.
func TestGRPCBridgeRefusesAnUnversionedReference(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)
	client := grpcBridgeClient(t, app)

	_, err := client.Call(authed(t.Context(), token), &kernelV1.CallRequest{
		Capability: "directory.people.reader",
		Payload:    []byte("{}"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unversioned reference returned %v, want InvalidArgument", status.Code(err))
	}
}

// TestGRPCBridgeMalformedPayload maps to InvalidArgument rather than Internal:
// the caller sent something the contract does not accept.
func TestGRPCBridgeMalformedPayload(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)
	client := grpcBridgeClient(t, app)

	_, err := client.Call(authed(t.Context(), token), &kernelV1.CallRequest{
		Capability: directory.Reader.Key(),
		Payload:    []byte(`{"limit": "not a number"}`),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("malformed payload returned %v, want InvalidArgument", status.Code(err))
	}
}

// TestGRPCBridgeListMatchesHTTP: both transports must publish the same set, or
// an integrator's view of what exists depends on how they connected.
func TestGRPCBridgeListMatchesHTTP(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)
	client := grpcBridgeClient(t, app)

	resp, err := client.List(authed(t.Context(), token), &kernelV1.ListRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var sawExposed, sawHidden bool
	for _, ref := range resp.GetCapabilities() {
		switch ref.GetName() + "@" + ref.GetVersion() {
		case directory.Reader.Key():
			sawExposed = true
		case hiddenRef.Key():
			sawHidden = true
		}
	}
	if !sawExposed {
		t.Error("an exposed capability is missing from the gRPC listing")
	}
	if sawHidden {
		t.Error("an unexposed capability appears in the gRPC listing")
	}
}

// TestGRPCCapabilityErrorDetailDoesNotReachTheCaller mirrors the HTTP test. Both
// transports are reachable from outside the process, so a leak on either is a
// leak — and a property enforced on only one is one that comes back.
func TestGRPCCapabilityErrorDetailDoesNotReachTheCaller(t *testing.T) {
	app := bridgeApp(t)
	token := bridgeToken(t, app)
	client := grpcBridgeClient(t, app)

	_, err := client.Call(authed(t.Context(), token), &kernelV1.CallRequest{
		Capability: leakyRef.Key(),
		Payload:    []byte("{}"),
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("got %v, want Internal", status.Code(err))
	}

	message := status.Convert(err).Message()
	if strings.Contains(message, "SELECT") || strings.Contains(message, "other-tenant") {
		t.Fatalf("the capability's internal error detail reached the caller: %q", message)
	}
}
