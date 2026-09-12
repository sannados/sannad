// Package wasmhost runs sandboxed WASM modules in-process.
//
// ADR 0001 defines this as tier 2: partial trust, in-process, isolated. It exists
// because the other two tiers each give up something. An embedded Go module runs
// with full process privileges — it can call os.Exit, read session secrets, and
// spawn goroutines nothing can stop — so a marketplace cannot run untrusted code
// there. An external app is safe but pays a network hop, which rules it out for
// anything in a request path.
//
// WASM is the tier where untrusted code runs fast: memory-limited, time-limited,
// no ambient filesystem or network, and host functions as the only capability
// surface. wazero is used because it is pure Go — a CGO dependency would end
// cross-compilation and static binaries, which a self-hosted framework cannot
// give up.
//
// It is also the only real fix for a flaw the embedded tier cannot address. A
// tier-1 subscriber that ignores its context leaks a goroutine the kernel can
// only abandon; a guest that ignores its deadline is interrupted, because the
// runtime owns its execution.
package wasmhost

import (
	"errors"
	"fmt"
)

// The ABI is deliberately small: two exported functions and a linear-memory
// convention. A larger surface would be more convenient and would also be more
// to keep compatible across every guest language, forever.
//
// Guests export:
//
//	sannad_alloc(size uint32) uint32
//	sannad_handle(ptr uint32, len uint32) uint64
//
// The host writes a request into memory the guest allocated, calls the handler,
// and reads the response from the packed pointer and length the handler returns.
// The guest never receives a Go pointer and the host never writes to memory the
// guest did not hand it.
const (
	// ExportAlloc is the guest function that reserves memory for a request.
	//
	// The guest allocates rather than the host choosing an address, because only
	// the guest knows what its own allocator considers free. A host that picked
	// an offset would corrupt the guest's heap.
	ExportAlloc = "sannad_alloc"

	// ExportHandle is the guest's entry point.
	ExportHandle = "sannad_handle"

	// ExportMemory is the linear memory both sides read and write.
	ExportMemory = "memory"
)

var (
	// ErrGuestContract is returned when a module does not implement the ABI.
	// Refused at load rather than on the first call, so a broken guest fails
	// where an operator is looking.
	ErrGuestContract = errors.New("wasmhost: guest does not implement the Sannad ABI")

	// ErrGuestFailed is returned when a guest traps, exceeds its limits, or
	// returns a malformed result. Deliberately distinct from a capability error:
	// the guest misbehaved, rather than the operation legitimately failing.
	ErrGuestFailed = errors.New("wasmhost: guest execution failed")

	// ErrGuestTimeout is returned when a guest exceeds its time limit. Separate
	// from ErrGuestFailed because it is the one failure an operator responds to
	// by changing configuration rather than by reporting a bug.
	ErrGuestTimeout = errors.New("wasmhost: guest exceeded its time limit")

	// ErrResponseTooLarge is returned when a guest's response exceeds the
	// configured cap. A guest could otherwise return a length that makes the
	// host allocate arbitrarily — the sandbox bounds the guest's memory, not the
	// host's.
	ErrResponseTooLarge = errors.New("wasmhost: guest response exceeds the limit")

	// ErrHostCallDenied is returned when a v2 guest asks for a capability call
	// or event publish its approved manifest does not grant. Guest-declared
	// intent is not authority (ADR 0007); only the operator-approved manifest
	// decides what a guest may reach.
	ErrHostCallDenied = errors.New("wasmhost: host call not approved for this module")

	// ErrCallCycle is returned when serving a capability would re-enter a
	// capability already active in the same synchronous call chain. Detected at
	// every WASM capability's entry point regardless of how the chain reached
	// it, so a cycle through an embedded module in the middle is still caught.
	ErrCallCycle = errors.New("wasmhost: capability call cycle detected")

	// ErrCallDepthExceeded is returned when the synchronous call chain through
	// WASM-served capabilities exceeds the configured limit.
	ErrCallDepthExceeded = errors.New("wasmhost: max capability call depth exceeded")
)

// packResult splits the uint64 a guest handler returns into pointer and length.
//
// One value rather than two because WebAssembly's core spec allows a single
// return, and multi-value is not universally available across guest toolchains.
// High 32 bits are the pointer, low 32 the length.
func packResult(value uint64) (ptr uint32, length uint32) {
	return uint32(value >> 32), uint32(value)
}

// Pack builds the return value a guest handler produces. Exported so a guest
// written in Go, and the tests, agree with the host by construction rather than
// by both implementing the same paragraph of documentation.
func Pack(ptr, length uint32) uint64 {
	return uint64(ptr)<<32 | uint64(length)
}

// statusOf reads the one-byte status prefix a guest writes before its payload.
//
// The prefix distinguishes "the capability returned an error" from "the guest
// itself broke". Without it a guest reporting a business rejection would be
// indistinguishable from one that trapped, and a fail-closed hook could not tell
// a legitimate veto from a crash.
func statusOf(response []byte) (status byte, payload []byte, err error) {
	if len(response) == 0 {
		return 0, nil, fmt.Errorf("%w: empty response", ErrGuestFailed)
	}
	return response[0], response[1:], nil
}

// Guest response statuses.
const (
	// StatusOK means the payload is a successful result.
	StatusOK byte = 0

	// StatusError means the payload is an error message the guest produced
	// deliberately — a validation failure, a rejected operation.
	StatusError byte = 1
)

// EncodeResponse builds a guest response. Exported for the same reason as Pack.
func EncodeResponse(status byte, payload []byte) []byte {
	out := make([]byte, 0, len(payload)+1)
	out = append(out, status)
	return append(out, payload...)
}
