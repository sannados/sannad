//go:build wasip1

// Command guest is a WASM module implementing the Sannad ABI.
//
// It is a test fixture and a worked example of the guest side: a real module
// compiled to WebAssembly, exercising the same path a community plugin would.
// The host tests run against this rather than a hand-written byte array, because
// a fixture that never goes through a compiler proves nothing about whether the
// ABI is implementable.
package main

import (
	"encoding/json"
	"unsafe"
)

// buffers keeps allocations alive. A Go WASM guest must stop the collector
// reclaiming memory the host is about to write into: the host holds only a raw
// offset, which is invisible to the collector.
var buffers = map[uint32][]byte{}

//go:wasmexport sannad_alloc
func alloc(size uint32) uint32 {
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(&buf[0])))
	buffers[ptr] = buf
	return ptr
}

type envelope struct {
	Capability string          `json:"capability"`
	TenantID   string          `json:"tenant_id"`
	Payload    json.RawMessage `json:"payload"`
}

type request struct {
	Amount int    `json:"amount"`
	Reject bool   `json:"reject"`
	Spin   bool   `json:"spin"`
	Grow   bool   `json:"grow"`
	Echo   string `json:"echo"`
}

type response struct {
	Doubled  int    `json:"doubled"`
	TenantID string `json:"tenant_id"`
	Echo     string `json:"echo"`
}

//go:wasmexport sannad_handle
func handle(ptr uint32, length uint32) uint64 {
	input := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)

	var env envelope
	if err := json.Unmarshal(input, &env); err != nil {
		return respond(1, []byte("malformed envelope"))
	}

	var req request
	_ = json.Unmarshal(env.Payload, &req)

	// Exceed the time limit, so a test can prove the sandbox interrupts a guest
	// that will not stop on its own — the thing the embedded tier cannot do.
	if req.Spin {
		for {
		}
	}

	// Exceed the memory limit.
	if req.Grow {
		var held [][]byte
		for {
			held = append(held, make([]byte, 1<<20))
		}
	}

	// A deliberate rejection: a business refusal, not a malfunction.
	if req.Reject {
		return respond(1, []byte("rejected by policy"))
	}

	out, err := json.Marshal(response{
		Doubled:  req.Amount * 2,
		TenantID: env.TenantID,
		Echo:     req.Echo,
	})
	if err != nil {
		return respond(1, []byte("encode failed"))
	}
	return respond(0, out)
}

// respond writes a status byte followed by the payload and packs the pointer and
// length into the single value the ABI expects.
func respond(status byte, payload []byte) uint64 {
	out := make([]byte, 0, len(payload)+1)
	out = append(out, status)
	out = append(out, payload...)

	ptr := uint32(uintptr(unsafe.Pointer(&out[0])))
	buffers[ptr] = out
	return uint64(ptr)<<32 | uint64(len(out))
}

// main is never called.
//
// This is built with -buildmode=c-shared, which produces a WebAssembly *reactor*
// rather than a command: the module exports _initialize instead of _start, so
// the Go runtime is set up and control returns to the host, leaving the exported
// functions callable.
//
// A plugin is a library, not a program, and the distinction is load-bearing. A
// command-mode build exports only _start, which initialises the runtime and then
// runs main — so the module exits before the host can call anything, and
// blocking in main to prevent that either deadlocks (Go's detector aborts on
// "all goroutines are asleep") or never returns control to the host at all.
// Both were tried before this.
//
// Build with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o guest.wasm ./guest/
func main() {}
