package wasmhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/getkayan/kayan/core/tenant"
	"github.com/sannados/sannad/pkg/modulekit"
)

// CurrentABIVersion is the guest ABI understood by this host: v2 adds the
// sannad_v2 host-call import namespace (ADR 0007) on top of the same guest
// exports and v1 envelope.
//
// MinSupportedABIVersion is v1, kept for the guests already shipped against
// it. ABI v1 is not implied by an empty V2Config; a manifest declaring
// ABIVersion 1 is refused outright if it names any Consumes,
// PublishesEvents, or SubscribesHooks — those grants are ABI v2 features a v1
// guest has no import table to exercise, so approving them for a v1 module
// would be policy the guest can never act on.
const (
	CurrentABIVersion      uint32 = 2
	MinSupportedABIVersion uint32 = 1
)

// ApprovedManifest is the operator-approved identity and publication surface
// of a sandboxed module.
//
// This is intentionally host input, not metadata trusted from inside the guest.
// A guest cannot grant itself a capability or publish itself through the
// gateway by embedding a more permissive declaration in its bytes.
type ApprovedManifest struct {
	ID         string
	ABIVersion uint32
	Provides   []modulekit.CapabilityRef
	Exposed    []modulekit.CapabilityRef

	// Consumes is the exact set of capabilities this guest may call through
	// sannad_v2.capability_call. ABI v2 only; refused for v1.
	//
	// Provides is not a license to consume: a module calling its own provided
	// capability still needs that capability listed here, which is what makes
	// self-recursion visible to whoever approved the manifest.
	Consumes []modulekit.CapabilityRef

	// PublishesEvents is the exact set of logical event names this guest may
	// publish through sannad_v2.event_publish. ABI v2 only; refused for v1.
	// Wildcards, broker subjects, and tenant segments are forbidden — a name
	// here is matched exactly, the same rule as Consumes.
	PublishesEvents []string

	// SubscribesHooks is the set of hook points this guest participates in.
	// ABI v2 only; refused for v1. The declared Kind must match how the point
	// is actually offered; a mismatch is refused at installation rather than
	// discovered the first time the hook fires.
	SubscribesHooks []HookGrant
}

// HookGrant is one approved hook subscription: which point, what kind of
// participation the operator approved it for, and its dispatch priority.
type HookGrant struct {
	Ref      modulekit.HookRef
	Kind     modulekit.HookKind
	Priority int
}

// ModuleConfig contains the approved manifest, guest bytes, and operator-owned
// resource limits for one sandboxed module.
type ModuleConfig struct {
	Manifest ApprovedManifest
	Wasm     []byte

	// Caller and Events are the platform's own dispatch surfaces, not guest
	// input: they are what capability_call and event_publish reach through
	// when the manifest grants them. Required only when Consumes or
	// PublishesEvents is non-empty; bootstrap fills both in automatically when
	// left nil.
	Caller modulekit.Caller
	Events modulekit.EventPublisher

	MaxMemoryPages   uint32
	Timeout          time.Duration
	MaxResponseBytes uint32
	Stdout           func([]byte)
	Stderr           func([]byte)

	// v2 host-call resource limits. Zero uses the default for each.
	MaxOutboundRequestBytes uint32
	MaxResultBytes          uint32
	MaxResultHandles        uint32
	MaxEventPayloadBytes    uint32
	MaxCallDepth            uint32
}

// Module adapts a loaded WASM guest to the same lifecycle and capability
// registration surface as an embedded Go module.
type Module struct {
	manifest ApprovedManifest
	plugin   *Plugin
	codec    Codec
	stopOnce sync.Once
	stopErr  error
}

// NewModule validates operator policy and loads the guest before it can enter
// the kernel registry.
func NewModule(ctx context.Context, config ModuleConfig) (*Module, error) {
	manifest, err := validateManifest(config.Manifest)
	if err != nil {
		return nil, err
	}

	var v2 *V2Config
	if len(manifest.Consumes) > 0 || len(manifest.PublishesEvents) > 0 {
		v2 = &V2Config{
			Caller:                  config.Caller,
			Events:                  config.Events,
			Consumes:                manifest.Consumes,
			PublishesEvents:         manifest.PublishesEvents,
			MaxOutboundRequestBytes: config.MaxOutboundRequestBytes,
			MaxResultBytes:          config.MaxResultBytes,
			MaxResultHandles:        config.MaxResultHandles,
			MaxEventPayloadBytes:    config.MaxEventPayloadBytes,
			MaxCallDepth:            config.MaxCallDepth,
		}
		if len(manifest.Consumes) > 0 && config.Caller == nil {
			return nil, fmt.Errorf("wasmhost: module %s consumes capabilities but no caller was provided", manifest.ID)
		}
		if len(manifest.PublishesEvents) > 0 && config.Events == nil {
			return nil, fmt.Errorf("wasmhost: module %s publishes events but no event publisher was provided", manifest.ID)
		}
	}

	plugin, err := Load(ctx, Config{
		ModuleID:         manifest.ID,
		Wasm:             config.Wasm,
		MaxMemoryPages:   config.MaxMemoryPages,
		Timeout:          config.Timeout,
		MaxResponseBytes: config.MaxResponseBytes,
		Stdout:           config.Stdout,
		Stderr:           config.Stderr,
		V2:               v2,
	})
	if err != nil {
		return nil, err
	}

	return &Module{manifest: manifest, plugin: plugin, codec: JSONCodec{}}, nil
}

func validateManifest(input ApprovedManifest) (ApprovedManifest, error) {
	if input.ABIVersion < MinSupportedABIVersion || input.ABIVersion > CurrentABIVersion {
		return ApprovedManifest{}, fmt.Errorf("wasmhost: module %q requests ABI version %d; host supports %d through %d",
			input.ID, input.ABIVersion, MinSupportedABIVersion, CurrentABIVersion)
	}
	if input.ABIVersion == 1 && (len(input.Consumes) > 0 || len(input.PublishesEvents) > 0 || len(input.SubscribesHooks) > 0) {
		return ApprovedManifest{}, fmt.Errorf(
			"wasmhost: module %s declares ABI version 1 but requests v2 grants (consumes, published events, or hook subscriptions); "+
				"a v1 guest has no import table to exercise them", input.ID)
	}
	// Reuse the SDK's existing reverse-DNS validator. WASM modules do not ship
	// kernel migrations today, but accepting a different ID grammar for this tier
	// would make an ID valid or invalid depending on where its implementation ran.
	if _, err := modulekit.MigrationTableFor(input.ID); err != nil {
		return ApprovedManifest{}, fmt.Errorf("wasmhost: invalid approved manifest: %w", err)
	}
	if len(input.Provides) == 0 {
		return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s provides no capabilities", input.ID)
	}

	manifest := input
	manifest.Provides = append([]modulekit.CapabilityRef(nil), input.Provides...)
	manifest.Exposed = append([]modulekit.CapabilityRef(nil), input.Exposed...)
	manifest.Consumes = append([]modulekit.CapabilityRef(nil), input.Consumes...)
	manifest.PublishesEvents = append([]string(nil), input.PublishesEvents...)
	manifest.SubscribesHooks = append([]HookGrant(nil), input.SubscribesHooks...)

	provided := make(map[string]struct{}, len(manifest.Provides))
	for _, ref := range manifest.Provides {
		if err := validateCapabilityRef(ref); err != nil {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s: %w", input.ID, err)
		}
		if _, duplicate := provided[ref.Key()]; duplicate {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s provides %s more than once", input.ID, ref.Key())
		}
		provided[ref.Key()] = struct{}{}
	}

	exposed := make(map[string]struct{}, len(manifest.Exposed))
	for _, ref := range manifest.Exposed {
		if err := validateCapabilityRef(ref); err != nil {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s: %w", input.ID, err)
		}
		if _, ok := provided[ref.Key()]; !ok {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s exposes %s but does not provide it", input.ID, ref.Key())
		}
		if _, duplicate := exposed[ref.Key()]; duplicate {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s exposes %s more than once", input.ID, ref.Key())
		}
		exposed[ref.Key()] = struct{}{}
	}

	consumed := make(map[string]struct{}, len(manifest.Consumes))
	for _, ref := range manifest.Consumes {
		if err := validateCapabilityRef(ref); err != nil {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s: %w", input.ID, err)
		}
		if _, duplicate := consumed[ref.Key()]; duplicate {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s consumes %s more than once", input.ID, ref.Key())
		}
		consumed[ref.Key()] = struct{}{}
	}

	published := make(map[string]struct{}, len(manifest.PublishesEvents))
	for _, name := range manifest.PublishesEvents {
		if name == "" {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s declares an empty published event name", input.ID)
		}
		if strings.ContainsAny(name, "*>") {
			return ApprovedManifest{}, fmt.Errorf(
				"wasmhost: module %s publishes %q: wildcards are forbidden, event grants are exact names", input.ID, name)
		}
		if _, duplicate := published[name]; duplicate {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s publishes %s more than once", input.ID, name)
		}
		published[name] = struct{}{}
	}

	subscribed := make(map[string]struct{}, len(manifest.SubscribesHooks))
	for _, grant := range manifest.SubscribesHooks {
		if grant.Ref.Name == "" || grant.Ref.Version == "" {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s: hook reference needs both a name and a version", input.ID)
		}
		if !grant.Kind.Valid() {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s subscribes to %s with unknown hook kind %q",
				input.ID, grant.Ref.Key(), grant.Kind)
		}
		if _, duplicate := subscribed[grant.Ref.Key()]; duplicate {
			return ApprovedManifest{}, fmt.Errorf("wasmhost: module %s subscribes to %s more than once", input.ID, grant.Ref.Key())
		}
		subscribed[grant.Ref.Key()] = struct{}{}
	}

	return manifest, nil
}

func validateCapabilityRef(ref modulekit.CapabilityRef) error {
	if ref.Name == "" || ref.Version == "" {
		return fmt.Errorf("capability reference %q must have a name and version", ref.Key())
	}
	if containsAt(ref.Name) || containsAt(ref.Version) {
		return fmt.Errorf("capability reference %q contains an invalid @ separator", ref.Key())
	}
	return nil
}

func containsAt(value string) bool {
	for _, r := range value {
		if r == '@' {
			return true
		}
	}
	return false
}

func (m *Module) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       m.manifest.ID,
		Kind:     "sandboxed",
		Provides: append([]modulekit.CapabilityRef(nil), m.manifest.Provides...),
		Consumes: append([]modulekit.CapabilityRef(nil), m.manifest.Consumes...),
		Exposed:  append([]modulekit.CapabilityRef(nil), m.manifest.Exposed...),
		Storage:  modulekit.StorageNone,
	}
}

// offeredPointsLister is the narrow surface Install uses to check a hook
// grant's declared kind against how the point is actually offered, before
// asking to subscribe. Not part of modulekit.Registrar: most registrars never
// need it, and the real registry satisfies it incidentally.
type offeredPointsLister interface {
	OfferedPoints() []modulekit.HookPoint
}

func (m *Module) Install(reg modulekit.Registrar) error {
	for _, ref := range m.manifest.Provides {
		if err := reg.RegisterCapability(ref, m.plugin.Handler(ref, m.codec)); err != nil {
			return fmt.Errorf("wasmhost: register %s for %s: %w", ref.Key(), m.manifest.ID, err)
		}
	}

	if len(m.manifest.SubscribesHooks) == 0 {
		return nil
	}

	hookReg, ok := modulekit.HookRegistrarFrom(reg)
	if !ok {
		return fmt.Errorf("wasmhost: %s declares hook subscriptions but the registrar does not support hooks", m.manifest.ID)
	}
	lister, _ := reg.(offeredPointsLister)

	for _, grant := range m.manifest.SubscribesHooks {
		if lister != nil {
			if offered, found := offeredKind(lister.OfferedPoints(), grant.Ref); found && offered != grant.Kind {
				return fmt.Errorf("wasmhost: %s expects hook %s to be %q but it is offered as %q",
					m.manifest.ID, grant.Ref.Key(), grant.Kind, offered)
			}
		}
		sub := modulekit.Subscription{
			Ref:      grant.Ref,
			ModuleID: m.manifest.ID,
			Priority: grant.Priority,
			Handler:  m.hookHandler(grant),
		}
		if err := hookReg.Subscribe(sub); err != nil {
			return fmt.Errorf("wasmhost: subscribe %s for %s: %w", grant.Ref.Key(), m.manifest.ID, err)
		}
	}
	return nil
}

func offeredKind(points []modulekit.HookPoint, ref modulekit.HookRef) (modulekit.HookKind, bool) {
	for _, point := range points {
		if point.Ref.Key() == ref.Key() {
			return point.Kind, true
		}
	}
	return "", false
}

// hookEnvelope is the v2 inbound shape a guest's sannad_handle receives for a
// hook invocation, distinct from the v1 capability envelope by its
// "operation" field. Capability invocations use "capability" (see codec.go);
// this is "hook".
type hookEnvelope struct {
	Operation string          `json:"operation"`
	Name      string          `json:"name"`
	HookKind  string          `json:"hook_kind"`
	TenantID  string          `json:"tenant_id"`
	Payload   json.RawMessage `json:"payload"`
}

// hookHandler adapts one approved hook grant to modulekit.HookHandler. The
// guest never sees which capabilities exist or gets a service locator: it
// receives exactly one call, shaped by the hook point's kind, and its
// response is translated back into that kind's contract — the guest cannot
// change what a validator's rejection or a transformer's output means by
// returning something unexpected. Hook ordering, timeout, and failure policy
// stay owned by the hook registry and the offering module, never the guest.
func (m *Module) hookHandler(grant HookGrant) modulekit.HookHandler {
	return func(ctx context.Context, value any) (any, error) {
		tenantID := tenant.IDFromContext(ctx)
		if tenantID == "" {
			return nil, fmt.Errorf("wasmhost: no tenant in context for hook %s", grant.Ref.Key())
		}

		var payload []byte
		switch v := value.(type) {
		case nil:
			payload = []byte("null")
		case modulekit.RawPayload:
			if !json.Valid(v) {
				return nil, fmt.Errorf("wasmhost: hook %s payload is not valid JSON", grant.Ref.Key())
			}
			payload = append([]byte(nil), v...)
		default:
			if err := modulekit.AssertPortable(value); err != nil {
				return nil, fmt.Errorf("hook %s: %w", grant.Ref.Key(), err)
			}
			var err error
			payload, err = json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("wasmhost: encode hook %s payload: %w", grant.Ref.Key(), err)
			}
		}

		envelope := hookEnvelope{
			Operation: "hook",
			Name:      grant.Ref.Key(),
			HookKind:  string(grant.Kind),
			TenantID:  tenantID,
			Payload:   payload,
		}
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return nil, fmt.Errorf("wasmhost: encode hook envelope for %s: %w", grant.Ref.Key(), err)
		}

		raw, err := m.plugin.Invoke(ctx, encoded)
		if err != nil {
			return nil, err
		}
		status, body, err := statusOf(raw)
		if err != nil {
			return nil, err
		}

		switch grant.Kind {
		case modulekit.KindObserver:
			// Ignored by contract: an observer cannot block, and a subscriber
			// that traps already returned as an error above, before there was
			// any status byte to read.
			return nil, nil
		case modulekit.KindValidator:
			if status == StatusOK {
				return nil, nil
			}
			// ModuleID is filled in by the dispatcher, not the guest, so a
			// rejection is attributable even though the guest never states
			// its own identity.
			return modulekit.Rejection{Reason: string(body)}, nil
		case modulekit.KindTransformer:
			if status == StatusError {
				return nil, fmt.Errorf("hook %s: %s", grant.Ref.Key(), string(body))
			}
			if len(body) == 0 || string(body) == "null" {
				return nil, nil
			}
			return modulekit.RawPayload(body), nil
		default:
			return nil, fmt.Errorf("wasmhost: unknown hook kind %q for %s", grant.Kind, grant.Ref.Key())
		}
	}
}

func (m *Module) Start(context.Context) error { return nil }

// Stop releases the compiled guest and runtime. It is idempotent because
// startup rollback and application shutdown may both try to clean up a module.
func (m *Module) Stop(ctx context.Context) error {
	m.stopOnce.Do(func() { m.stopErr = m.plugin.Close(ctx) })
	return m.stopErr
}

// Close is an alias for Stop used by bootstrap when registration itself fails
// and the module therefore never enters the registry lifecycle.
func (m *Module) Close(ctx context.Context) error {
	return m.Stop(ctx)
}

var _ modulekit.Module = (*Module)(nil)
