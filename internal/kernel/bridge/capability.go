// Package bridge exposes registered capabilities to callers outside the
// process.
//
// ADR 0001 stakes the design on one claim: "the same capability lives at any
// tier… and callers cannot tell the difference." Until this package existed that
// was false for tier 3. A module got an external API only if someone hand-wrote
// a .proto and a gRPC service for it, which is why the only external surface in
// the tree belongs to the example module. Tier 3 is where the marketplace lives,
// so the gap was in the tier that matters most.
//
// The bridge dispatches by reference: one endpoint serves every exposed
// capability, and a module gets an external API by declaring one. No proto, no
// handler, no codegen per module.
package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/pkg/modulekit"
)

// MaxRequestBytes caps a bridge request body.
//
// Unbounded reads on a public endpoint are a denial-of-service primitive: a
// caller streams gigabytes and the process dies before any handler is reached.
// The limit is deliberately generous for a JSON contract payload and far below
// anything that threatens memory.
const MaxRequestBytes = 1 << 20 // 1 MiB

// Registry is the subset of the kernel registry the bridge depends on.
//
// Narrow on purpose: the bridge must be able to ask whether a capability is
// published, and nothing else. A wider dependency would let a future edit reach
// registry internals from an HTTP handler.
type Registry interface {
	IsExposed(ref modulekit.CapabilityRef) bool
	ListExposed() []modulekit.CapabilityRef
}

// ParseRef turns "name@version" into a reference.
//
// The version is required. A caller that omits it is asking for "whatever you
// have", which is how an integration silently starts talking to v2 after an
// upgrade — the exact failure versioned references exist to prevent.
func ParseRef(s string) (modulekit.CapabilityRef, error) {
	name, version, found := strings.Cut(s, "@")
	if !found || name == "" || version == "" {
		return modulekit.CapabilityRef{}, fmt.Errorf("capability reference %q must be name@version", s)
	}
	return modulekit.CapabilityRef{Name: name, Version: version}, nil
}

// StatusFor maps a capability error to an HTTP status.
//
// Handlers return the SDK's sentinels, and this is the one place they become
// status codes, so two transports cannot disagree about what ErrForbidden means.
func StatusFor(err error) int {
	switch {
	case errors.Is(err, modulekit.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, modulekit.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, modulekit.ErrTypeMismatch):
		// The caller sent a body the contract does not accept. That is a
		// malformed request, not a server fault, and returning 500 would send
		// integrators hunting a bug on the wrong side.
		return http.StatusBadRequest
	case errors.Is(err, tenancy.ErrNoTenantContext):
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

// Dispatch calls an exposed capability with a JSON body and writes a JSON
// response.
//
// It is written against net/http rather than the gateway's framework so the
// dispatch logic can be tested without a server, and so a future transport
// reuses it unchanged.
func Dispatch(reg Registry, call func(request *http.Request, ref modulekit.CapabilityRef, payload modulekit.RawPayload) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		refText := strings.TrimPrefix(r.URL.Path, "/")
		if idx := strings.LastIndex(refText, "/"); idx >= 0 {
			refText = refText[idx+1:]
		}

		ref, err := ParseRef(refText)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		// Exposure is checked before resolution, and the answer is the same 404
		// either way. An unexposed capability must not be distinguishable from a
		// nonexistent one, or the endpoint becomes an oracle for enumerating
		// what a deployment runs internally.
		if !reg.IsExposed(ref) {
			writeError(w, http.StatusNotFound, "unknown capability")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "cannot read request body")
			return
		}
		if len(body) > MaxRequestBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		// An empty body is a request with no fields, not an error: a capability
		// taking only a limit should be callable with no body at all.
		if len(body) == 0 {
			body = []byte("{}")
		}

		result, err := call(r, ref, modulekit.RawPayload(body))
		if err != nil {
			status := StatusFor(err)
			// The detail is logged, never returned. A capability error can carry
			// internal state — a query, a path, another tenant's identifier —
			// and this endpoint is public.
			slog.WarnContext(r.Context(), "bridge dispatch failed",
				"capability", ref.Key(), "status", status, "err", err)
			writeError(w, status, publicMessage(status))
			return
		}

		encoded, err := encodeResult(result, json.Marshal)
		if err != nil {
			slog.ErrorContext(r.Context(), "bridge response encoding failed",
				"capability", ref.Key(), "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}
}

// encodeResult preserves an already encoded transport payload instead of
// encoding its bytes as a JSON string. WASM and remote handlers return
// RawPayload because the kernel cannot know their concrete response type.
func encodeResult(result any, encode func(any) ([]byte, error)) ([]byte, error) {
	if raw, ok := result.(modulekit.RawPayload); ok {
		if !json.Valid(raw) {
			return nil, errors.New("capability returned invalid JSON")
		}
		return append([]byte(nil), raw...), nil
	}
	return encode(result)
}

// ListHandler serves the capabilities callable from outside.
//
// Deliberately ListExposed, not ListCapabilities: the latter includes every
// internal capability in the deployment and publishing it would tell a stranger
// exactly what modules are installed.
func ListHandler(reg Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reg.ListExposed())
	}
}

func publicMessage(status int) string {
	switch status {
	case http.StatusNotFound:
		return "not found"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusBadRequest:
		return "invalid request for this capability"
	case http.StatusUnauthorized:
		return "unauthorized"
	default:
		return "internal error"
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// assert the concrete registry satisfies the narrow interface, so a signature
// drift is a compile error rather than a wiring failure at boot.
var _ Registry = (*registry.Registry)(nil)
