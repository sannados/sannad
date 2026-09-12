package wasmhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/sannados/sannad/internal/kernel/events"
	"github.com/sannados/sannad/pkg/modulekit"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ADR 0007's ABI v2: a guest may call a capability, publish an event, and be
// subscribed to a hook, without receiving ambient access to the module graph.
// Everything here enforces two rules from the ADR: guest-declared intent is
// not authority (only the operator-approved manifest grants anything), and a
// guest cannot name its own tenant, subject, or caller identity — those travel
// in the Go context this package already carries and are never read from
// guest memory.

// v2Deps is the operator-approved, per-plugin surface a guest's host calls
// reach. Built once at Load from V2Config and never mutated, so it is safe to
// share across every invocation of one Plugin without synchronization.
type v2Deps struct {
	moduleID string
	caller   modulekit.Caller
	events   modulekit.EventPublisher
	grants   grantSet
	limits   v2Limits
}

type grantSet struct {
	consume map[string]struct{}
	publish map[string]struct{}
}

type v2Limits struct {
	maxOutboundRequestBytes uint32
	maxResultBytes          uint32
	maxResultHandles        uint32
	maxEventPayloadBytes    uint32
	maxCallDepth            uint32
}

func newV2Deps(moduleID string, cfg V2Config) *v2Deps {
	consume := make(map[string]struct{}, len(cfg.Consumes))
	for _, ref := range cfg.Consumes {
		consume[ref.Key()] = struct{}{}
	}
	publish := make(map[string]struct{}, len(cfg.PublishesEvents))
	for _, name := range cfg.PublishesEvents {
		publish[name] = struct{}{}
	}
	return &v2Deps{
		moduleID: moduleID,
		caller:   cfg.Caller,
		events:   cfg.Events,
		grants:   grantSet{consume: consume, publish: publish},
		limits: v2Limits{
			maxOutboundRequestBytes: orDefault(cfg.MaxOutboundRequestBytes, DefaultMaxOutboundRequestBytes),
			maxResultBytes:          orDefault(cfg.MaxResultBytes, DefaultMaxResultBytes),
			maxResultHandles:        orDefault(cfg.MaxResultHandles, DefaultMaxResultHandles),
			maxEventPayloadBytes:    orDefault(cfg.MaxEventPayloadBytes, DefaultMaxEventPayloadBytes),
			maxCallDepth:            orDefault(cfg.MaxCallDepth, DefaultMaxCallDepth),
		},
	}
}

func orDefault(value, fallback uint32) uint32 {
	if value == 0 {
		return fallback
	}
	return value
}

// callState holds what one top-level guest invocation produced: results a
// capability_call stored for later result_read, and the last diagnostic for
// error_read. It lives only as long as that one Invoke — attached to ctx, not
// to the Plugin — so nothing here can leak between calls or tenants.
type callState struct {
	mu      sync.Mutex
	results map[uint32][]byte
	nextH   uint32
	lastErr string
}

func newCallState() *callState {
	return &callState{results: make(map[uint32][]byte)}
}

func (s *callState) store(data []byte, max uint32) (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if uint32(len(s.results)) >= max {
		return 0, false
	}
	s.nextH++
	s.results[s.nextH] = data
	return s.nextH, true
}

func (s *callState) get(handle uint32) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.results[handle]
	return data, ok
}

func (s *callState) drop(handle uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.results[handle]; !ok {
		return false
	}
	delete(s.results, handle)
	return true
}

func (s *callState) setErr(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = msg
}

func (s *callState) getErr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Context plumbing. wazero threads the same Go context a guest's exported
// call was started with through every host function that call reaches, so
// values attached here at Invoke's top are visible inside capability_call and
// event_publish without any other channel between host and guest.
type v2DepsKey struct{}
type callStateKey struct{}
type callChainKey struct{}

func withV2Deps(ctx context.Context, deps *v2Deps) context.Context {
	return context.WithValue(ctx, v2DepsKey{}, deps)
}

func v2DepsFrom(ctx context.Context) (*v2Deps, bool) {
	deps, ok := ctx.Value(v2DepsKey{}).(*v2Deps)
	return deps, ok
}

func withCallState(ctx context.Context, state *callState) context.Context {
	return context.WithValue(ctx, callStateKey{}, state)
}

func callStateFrom(ctx context.Context) (*callState, bool) {
	state, ok := ctx.Value(callStateKey{}).(*callState)
	return state, ok
}

// withCallChain records the capability keys currently being served, in order.
// It is host context, never a guest-suppliable field (ADR 0007 §6): a guest
// cannot forge or clear it, since the only way to extend it is a Go value on
// the context wazero itself is carrying.
func withCallChain(ctx context.Context, chain []string) context.Context {
	return context.WithValue(ctx, callChainKey{}, chain)
}

func callChainFrom(ctx context.Context) []string {
	chain, _ := ctx.Value(callChainKey{}).([]string)
	return chain
}

// v2 host-call status codes, packed into the high 32 bits of capability_call's
// result alongside a result handle in the low 32 bits.
const (
	v2StatusOK       uint32 = 0
	v2StatusDenied   uint32 = 1
	v2StatusInvalid  uint32 = 2
	v2StatusFailed   uint32 = 3
	v2StatusInternal uint32 = 4
)

// instantiateV2Host wires the sannad_v2 import module every guest built
// against ABI v2 links against. It is wired unconditionally (see Load): a
// guest with no V2Config still gets a working import table, just one that
// denies everything through the ordinary status path rather than failing to
// link.
func instantiateV2Host(ctx context.Context, runtime wazero.Runtime) error {
	builder := runtime.NewHostModuleBuilder("sannad_v2")

	builder.NewFunctionBuilder().
		WithFunc(v2CapabilityCall).
		Export("capability_call")
	builder.NewFunctionBuilder().
		WithFunc(v2ResultLen).
		Export("result_len")
	builder.NewFunctionBuilder().
		WithFunc(v2ResultRead).
		Export("result_read")
	builder.NewFunctionBuilder().
		WithFunc(v2ResultDrop).
		Export("result_drop")
	builder.NewFunctionBuilder().
		WithFunc(v2EventPublish).
		Export("event_publish")
	builder.NewFunctionBuilder().
		WithFunc(v2ErrorLen).
		Export("error_len")
	builder.NewFunctionBuilder().
		WithFunc(v2ErrorRead).
		Export("error_read")

	_, err := builder.Instantiate(ctx)
	return err
}

type v2CallEnvelope struct {
	Capability string          `json:"capability"`
	Payload    json.RawMessage `json:"payload"`
}

// v2CapabilityCall resolves a capability through the same kernel caller any
// module uses — WASM-to-embedded, WASM-to-WASM, and WASM-to-external all take
// this one route, so there is no sandbox-specific service locator to keep in
// sync with the real one.
//
// It executes the call at most once. The result is stored in invocation-scoped
// state and returned as a handle rather than copied straight into guest
// memory, because a caller-sized output buffer that turns out too small would
// otherwise have to be discovered by retrying the call — and retrying a
// side-effecting capability is not safe.
func v2CapabilityCall(ctx context.Context, mod api.Module, reqPtr, reqLen uint32) uint64 {
	deps, depsOK := v2DepsFrom(ctx)
	state, stateOK := callStateFrom(ctx)
	if !depsOK || !stateOK {
		return Pack(v2StatusInternal, 0)
	}

	if reqLen > deps.limits.maxOutboundRequestBytes {
		state.setErr("capability_call: request exceeds the outbound size limit")
		return Pack(v2StatusInvalid, 0)
	}
	mem := mod.Memory()
	if mem == nil {
		state.setErr("capability_call: guest has no memory")
		return Pack(v2StatusInternal, 0)
	}
	raw, ok := mem.Read(reqPtr, reqLen)
	if !ok {
		state.setErr("capability_call: request pointer out of range")
		return Pack(v2StatusInvalid, 0)
	}

	var envelope v2CallEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		state.setErr("capability_call: " + err.Error())
		return Pack(v2StatusInvalid, 0)
	}

	if deps.caller == nil {
		state.setErr("capability_call: this module has no approved caller")
		return Pack(v2StatusDenied, 0)
	}
	if _, granted := deps.grants.consume[envelope.Capability]; !granted {
		state.setErr(fmt.Sprintf("%s: %s", ErrHostCallDenied, envelope.Capability))
		return Pack(v2StatusDenied, 0)
	}

	name, version, err := splitCapabilityKey(envelope.Capability)
	if err != nil {
		state.setErr("capability_call: " + err.Error())
		return Pack(v2StatusInvalid, 0)
	}
	ref := modulekit.CapabilityRef{Name: name, Version: version}

	// The chain already active in ctx propagates unchanged: the callee, if it
	// is itself WASM-served, checks it at its own Handler entry point. This
	// call does not need to duplicate that check to catch a cycle that would
	// pass back through an embedded module before returning here.
	result, err := deps.caller.Call(ctx, ref, modulekit.RawPayload(envelope.Payload))
	if err != nil {
		state.setErr("capability_call: " + err.Error())
		return Pack(v2StatusFailed, 0)
	}

	encoded, err := encodeCallResult(result)
	if err != nil {
		state.setErr("capability_call: encode response: " + err.Error())
		return Pack(v2StatusInternal, 0)
	}
	if uint32(len(encoded)) > deps.limits.maxResultBytes {
		state.setErr("capability_call: response exceeds the result size limit")
		return Pack(v2StatusInvalid, 0)
	}

	handle, ok := state.store(encoded, deps.limits.maxResultHandles)
	if !ok {
		state.setErr("capability_call: too many open result handles; drop one before calling again")
		return Pack(v2StatusInternal, 0)
	}
	return Pack(v2StatusOK, handle)
}

func encodeCallResult(result any) ([]byte, error) {
	if raw, ok := result.(modulekit.RawPayload); ok {
		return append([]byte(nil), raw...), nil
	}
	return json.Marshal(result)
}

func splitCapabilityKey(key string) (name, version string, err error) {
	idx := strings.LastIndexByte(key, '@')
	if idx <= 0 || idx == len(key)-1 {
		return "", "", fmt.Errorf("invalid capability reference %q", key)
	}
	return key[:idx], key[idx+1:], nil
}

// sentinel is returned by result_len and result_read for an unknown handle. Not
// zero, since zero is a legitimate length for an empty result.
const v2Sentinel uint32 = 0xFFFFFFFF

func v2ResultLen(ctx context.Context, _ api.Module, handle uint32) uint32 {
	state, ok := callStateFrom(ctx)
	if !ok {
		return v2Sentinel
	}
	data, ok := state.get(handle)
	if !ok {
		return v2Sentinel
	}
	return uint32(len(data))
}

// v2ResultRead copies a stored result into guest memory. If the guest's
// buffer is smaller than the result, nothing is written and the actual length
// is returned so the guest can allocate again and retry the read — retrying a
// copy is safe in a way retrying the call itself is not.
func v2ResultRead(ctx context.Context, mod api.Module, handle, outPtr, outCap uint32) uint32 {
	state, ok := callStateFrom(ctx)
	if !ok {
		return v2Sentinel
	}
	data, ok := state.get(handle)
	if !ok {
		return v2Sentinel
	}
	if uint32(len(data)) > outCap {
		return uint32(len(data))
	}
	mem := mod.Memory()
	if mem == nil || !mem.Write(outPtr, data) {
		return v2Sentinel
	}
	return uint32(len(data))
}

func v2ResultDrop(ctx context.Context, _ api.Module, handle uint32) uint32 {
	state, ok := callStateFrom(ctx)
	if !ok {
		return 1
	}
	if state.drop(handle) {
		return 0
	}
	return 1
}

type v2EventEnvelope struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

// v2EventPublish builds the tenant-scoped broker subject from the invocation
// context: a guest supplies only a logical event name and payload, and can
// neither choose a tenant nor construct a subject directly.
func v2EventPublish(ctx context.Context, mod api.Module, reqPtr, reqLen uint32) uint32 {
	deps, depsOK := v2DepsFrom(ctx)
	state, stateOK := callStateFrom(ctx)
	if !depsOK || !stateOK {
		return 1
	}
	if reqLen > deps.limits.maxEventPayloadBytes {
		state.setErr("event_publish: payload exceeds the event size limit")
		return 1
	}
	mem := mod.Memory()
	if mem == nil {
		state.setErr("event_publish: guest has no memory")
		return 1
	}
	raw, ok := mem.Read(reqPtr, reqLen)
	if !ok {
		state.setErr("event_publish: request pointer out of range")
		return 1
	}

	var envelope v2EventEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		state.setErr("event_publish: " + err.Error())
		return 1
	}

	if deps.events == nil {
		state.setErr("event_publish: this module has no approved publisher")
		return 1
	}
	if _, granted := deps.grants.publish[envelope.Event]; !granted {
		state.setErr(fmt.Sprintf("%s: %s", ErrHostCallDenied, envelope.Event))
		return 1
	}

	subject, err := events.Subject(ctx, envelope.Event)
	if err != nil {
		state.setErr("event_publish: " + err.Error())
		return 1
	}
	// envelope.Payload is passed as json.RawMessage, not a []byte alias:
	// json.Marshal respects its own MarshalJSON and re-embeds the bytes
	// verbatim. Passing a plain []byte here would encode it a second time as a
	// base64 string, the same defect the v1 RawPayload path had to avoid.
	if err := deps.events.Publish(ctx, subject, envelope.Payload); err != nil {
		state.setErr("event_publish: " + err.Error())
		return 1
	}
	return 0
}

func v2ErrorLen(ctx context.Context, _ api.Module) uint32 {
	state, ok := callStateFrom(ctx)
	if !ok {
		return 0
	}
	return uint32(len(state.getErr()))
}

func v2ErrorRead(ctx context.Context, mod api.Module, outPtr, outCap uint32) uint32 {
	state, ok := callStateFrom(ctx)
	if !ok {
		return v2Sentinel
	}
	msg := []byte(state.getErr())
	if uint32(len(msg)) > outCap {
		return uint32(len(msg))
	}
	mem := mod.Memory()
	if mem == nil || !mem.Write(outPtr, msg) {
		return v2Sentinel
	}
	return uint32(len(msg))
}
