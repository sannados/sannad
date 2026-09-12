package wasmhost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sannados/sannad/pkg/modulekit"
)

// A Go context does not cross into a guest, so everything the guest needs to
// know has to be written into the request explicitly. That is the whole
// difference between this tier and the embedded one, and it is where the
// portability contract stops being theoretical: a payload holding a func, a
// channel, or unexported state works perfectly in-process and cannot be encoded
// here at all.

// Envelope is what a guest receives.
//
// The tenant is a field rather than something the guest asserts. The host writes
// it from the request context, so a guest cannot name a tenant it was not
// invoked for — the same rule as the HTTP bridge, for the same reason.
type Envelope struct {
	// Capability is the reference being invoked, as name@version. A guest serving
	// more than one capability dispatches on it.
	Capability string `json:"capability"`

	// TenantID is the tenant this invocation acts for.
	TenantID string `json:"tenant_id"`

	// Payload is the request, encoded.
	Payload json.RawMessage `json:"payload"`
}

// Codec encodes requests for a guest and decodes what it returns.
//
// An interface so the wire format is a deployment decision rather than a
// property of every guest ever written. JSON is the default because it is the
// one format every guest language can produce without a toolchain.
type Codec interface {
	EncodeRequest(ctx context.Context, ref modulekit.CapabilityRef, tenantID string, request any) ([]byte, error)
	DecodeResponse(ref modulekit.CapabilityRef, payload []byte) (any, error)
}

// JSONCodec encodes envelopes as JSON.
type JSONCodec struct{}

// EncodeRequest builds the envelope a guest receives.
func (JSONCodec) EncodeRequest(_ context.Context, ref modulekit.CapabilityRef, tenantID string, request any) ([]byte, error) {
	// AssertPortable before encoding, so a contract that cannot cross a tier
	// boundary is reported as the contract error it is rather than as a confusing
	// marshalling failure. This is the check pkg/modulekit added for exactly this
	// moment — until now nothing exercised it against a real boundary.
	if request != nil {
		if err := modulekit.AssertPortable(request); err != nil {
			return nil, fmt.Errorf("capability %s: %w", ref.Key(), err)
		}
	}

	var payload []byte
	if raw, ok := request.(modulekit.RawPayload); ok {
		// A transport has already encoded the contract. Marshaling []byte again
		// would turn JSON into a base64 string and the guest would see every field
		// at its zero value. Validate and preserve it instead.
		if !json.Valid(raw) {
			return nil, fmt.Errorf("wasmhost: request for %s is not valid JSON", ref.Key())
		}
		payload = append([]byte(nil), raw...)
	} else {
		var err error
		payload, err = json.Marshal(request)
		if err != nil {
			return nil, fmt.Errorf("wasmhost: encode request for %s: %w", ref.Key(), err)
		}
	}

	envelope := Envelope{
		Capability: ref.Key(),
		TenantID:   tenantID,
		Payload:    payload,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("wasmhost: encode envelope for %s: %w", ref.Key(), err)
	}
	return encoded, nil
}

// DecodeResponse returns the guest's payload as raw bytes.
//
// modulekit.RawPayload rather than a decoded value: the host does not know the
// response type — that belongs to the contract the caller holds — and coerce
// decodes it into the caller's concrete type on the way out. This is the same
// path the HTTP bridge uses, which is what keeps one capability behaving
// identically at both tiers.
func (JSONCodec) DecodeResponse(_ modulekit.CapabilityRef, payload []byte) (any, error) {
	return modulekit.RawPayload(payload), nil
}
