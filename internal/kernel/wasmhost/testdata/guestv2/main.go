//go:build wasip1

// Command guestv2 is a WASM module implementing ABI v2 (ADR 0007): it imports
// sannad_v2 and exercises capability_call, event_publish, and inbound hook
// dispatch, in addition to the same sannad_alloc/sannad_handle exports v1
// guests implement.
//
// A separate fixture from testdata/guest rather than one guest testing both:
// v1's whole point is that it imports nothing beyond WASI, and a guest that
// imports sannad_v2 is a different artifact than one that does not — the same
// reason the ADR distinguishes the two ABI versions instead of unioning them
// into one guest contract.
package main

import (
	"encoding/json"
	"unsafe"
)

var buffers = map[uint32][]byte{}

//go:wasmexport sannad_alloc
func alloc(size uint32) uint32 {
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(&buf[0])))
	buffers[ptr] = buf
	return ptr
}

//go:wasmimport sannad_v2 capability_call
func hostCapabilityCall(ptr, length uint32) uint64

//go:wasmimport sannad_v2 result_len
func hostResultLen(handle uint32) uint32

//go:wasmimport sannad_v2 result_read
func hostResultRead(handle, outPtr, outCap uint32) uint32

//go:wasmimport sannad_v2 result_drop
func hostResultDrop(handle uint32) uint32

//go:wasmimport sannad_v2 event_publish
func hostEventPublish(ptr, length uint32) uint32

//go:wasmimport sannad_v2 error_len
func hostErrorLen() uint32

//go:wasmimport sannad_v2 error_read
func hostErrorRead(outPtr, outCap uint32) uint32

// envelope covers both shapes sannad_handle receives: the v1/v2 capability
// envelope (capability, tenant_id, payload) and the v2 inbound hook envelope
// (operation, name, hook_kind, tenant_id, payload). Operation is empty for a
// capability invocation, "hook" for a hook invocation.
type envelope struct {
	Operation  string          `json:"operation"`
	Capability string          `json:"capability"`
	Name       string          `json:"name"`
	HookKind   string          `json:"hook_kind"`
	TenantID   string          `json:"tenant_id"`
	Payload    json.RawMessage `json:"payload"`
}

// request is the business payload for a capability invocation of this guest.
type request struct {
	Amount int `json:"amount"`

	// CallCapability, if set, makes this handler call out through
	// sannad_v2.capability_call before responding, so a test can drive an
	// outbound host call from either the top-level entry or from inside a
	// hook.
	CallCapability string          `json:"call_capability"`
	CallPayload    json.RawMessage `json:"call_payload"`

	// PublishEvent, if set, makes this handler publish an event before
	// responding with a simple acknowledgement.
	PublishEvent   string          `json:"publish_event"`
	PublishPayload json.RawMessage `json:"publish_payload"`
}

// hookPayload is what a hook invocation carries in Payload.
type hookPayload struct {
	Amount int  `json:"amount"`
	Reject bool `json:"reject"`
}

//go:wasmexport sannad_handle
func handle(ptr uint32, length uint32) uint64 {
	input := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)

	var env envelope
	if err := json.Unmarshal(input, &env); err != nil {
		return respond(1, []byte("malformed envelope"))
	}

	if env.Operation == "hook" {
		return handleHook(env)
	}
	return handleCapability(env)
}

func handleHook(env envelope) uint64 {
	var payload hookPayload
	_ = json.Unmarshal(env.Payload, &payload)

	switch env.HookKind {
	case "validator":
		if payload.Reject {
			return respond(1, []byte("rejected by guest"))
		}
		return respond(0, []byte("null"))
	case "transformer":
		out, err := json.Marshal(map[string]int{"amount": payload.Amount * 2})
		if err != nil {
			return respond(1, []byte("encode failed"))
		}
		return respond(0, out)
	default: // observer, or anything else: acknowledged, return value ignored
		return respond(0, []byte("null"))
	}
}

func handleCapability(env envelope) uint64 {
	var req request
	_ = json.Unmarshal(env.Payload, &req)

	if req.CallCapability != "" {
		return doCapabilityCall(req.CallCapability, req.CallPayload)
	}
	if req.PublishEvent != "" {
		return doEventPublish(req.PublishEvent, req.PublishPayload)
	}

	out, err := json.Marshal(map[string]any{
		"doubled":   req.Amount * 2,
		"tenant_id": env.TenantID,
	})
	if err != nil {
		return respond(1, []byte("encode failed"))
	}
	return respond(0, out)
}

func doCapabilityCall(capability string, payload json.RawMessage) uint64 {
	callReq, err := json.Marshal(map[string]any{
		"capability": capability,
		"payload":    payload,
	})
	if err != nil {
		return respond(1, []byte("encode call request failed"))
	}

	callPtr := writeBuffer(callReq)
	packed := hostCapabilityCall(callPtr, uint32(len(callReq)))
	status, resultHandle := uint32(packed>>32), uint32(packed)
	if status != 0 {
		return respond(1, readHostError())
	}

	resultLen := hostResultLen(resultHandle)
	if resultLen == 0xFFFFFFFF {
		return respond(1, []byte("unknown result handle"))
	}
	resultBuf := make([]byte, resultLen)
	var readPtr uint32
	if resultLen > 0 {
		readPtr = uint32(uintptr(unsafe.Pointer(&resultBuf[0])))
		buffers[readPtr] = resultBuf
	}
	n := hostResultRead(resultHandle, readPtr, resultLen)
	if n == 0xFFFFFFFF {
		return respond(1, []byte("result_read failed"))
	}
	hostResultDrop(resultHandle)

	return respond(0, resultBuf[:n])
}

func doEventPublish(event string, payload json.RawMessage) uint64 {
	eventReq, err := json.Marshal(map[string]any{
		"event":   event,
		"payload": payload,
	})
	if err != nil {
		return respond(1, []byte("encode event request failed"))
	}
	ptr := writeBuffer(eventReq)
	if hostEventPublish(ptr, uint32(len(eventReq))) != 0 {
		return respond(1, readHostError())
	}
	return respond(0, []byte("null"))
}

func readHostError() []byte {
	n := hostErrorLen()
	if n == 0 || n == 0xFFFFFFFF {
		return []byte("host call denied")
	}
	buf := make([]byte, n)
	ptr := uint32(uintptr(unsafe.Pointer(&buf[0])))
	buffers[ptr] = buf
	hostErrorRead(ptr, n)
	return buf
}

func writeBuffer(data []byte) uint32 {
	buf := make([]byte, len(data))
	copy(buf, data)
	var ptr uint32
	if len(buf) > 0 {
		ptr = uint32(uintptr(unsafe.Pointer(&buf[0])))
	}
	buffers[ptr] = buf
	return ptr
}

func respond(status byte, payload []byte) uint64 {
	out := make([]byte, 0, len(payload)+1)
	out = append(out, status)
	out = append(out, payload...)

	ptr := uint32(uintptr(unsafe.Pointer(&out[0])))
	buffers[ptr] = out
	return uint64(ptr)<<32 | uint64(len(out))
}

// main is never called; this is built as a reactor. See testdata/guest for
// why. Build with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o guestv2.wasm ./guestv2/
func main() {}
