package conformance_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/sannados/sannad/pkg/modulekit"
)

// This file is the *extension* fixture. It attaches to the host flow through
// the hook registry and nothing else.
//
// What it deliberately does NOT do, because doing any of them would mean the
// hook design had failed:
//
//   - call a host function,
//   - hold a reference to the host module,
//   - require an edit to host_test.go,
//   - know the host's dispatch order or internal state.
//
// It shares the host's *contract* — the hook refs and the value types they
// carry — which is the same thing a third-party module would import from a
// published package. Sharing a contract is the mechanism; sharing code is not.

const extensionModuleID = "com.sannad.conformance.extension"

// surchargeLabel marks entries this extension contributed, so a test can prove
// the mutation came from the extension and not from the host.
const surchargeLabel = "extension-surcharge"

// extensionModule is a third party participating in a flow it does not own.
type extensionModule struct {
	// limit vetoes records whose total exceeds it.
	limit int

	// numberPrefix, when set, means this extension takes over document
	// numbering from the host.
	numberPrefix string

	// observed records what the extension saw after the commit, which is how a
	// test proves the observer ran without letting it influence the outcome.
	observed []string
}

func (m *extensionModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{Storage: modulekit.StorageNone, ID: extensionModuleID, Kind: "embedded"}
}

// Install is where the extension attaches. Every subscription names only a hook
// ref the host published.
func (m *extensionModule) Install(reg modulekit.Registrar) error {
	hooks, ok := modulekit.HookRegistrarFrom(reg)
	if !ok {
		return errors.New("host registrar offers no hook surface")
	}

	// Transformer: add a derived entry the host never computes.
	if err := modulekit.TransformTyped(hooks, HookComputeEntries, extensionModuleID, 10,
		func(_ context.Context, entries []Entry) ([]Entry, error) {
			total := 0
			for _, e := range entries {
				total += e.Amount
			}
			return append(entries, Entry{Label: surchargeLabel, Amount: total / 10}), nil
		}); err != nil {
		return err
	}

	// Validator: veto a state transition, with a reason the caller can render.
	if err := modulekit.SubscribeTyped(hooks, HookBeforePost, extensionModuleID, 0,
		func(_ context.Context, record Record) (any, error) {
			total := 0
			for _, e := range record.Entries {
				total += e.Amount
			}
			if m.limit > 0 && total > m.limit {
				return modulekit.Rejection{
					Reason: fmt.Sprintf("total %d exceeds limit %d", total, m.limit),
				}, nil
			}
			return nil, nil
		}); err != nil {
		return err
	}

	// Transformer: take over a value the host would otherwise supply.
	if err := modulekit.TransformTyped(hooks, HookDocumentNumber, extensionModuleID, 0,
		func(_ context.Context, hostNumber string) (string, error) {
			if m.numberPrefix == "" {
				return hostNumber, nil // abstain; the host's value stands
			}
			return m.numberPrefix + "-" + hostNumber, nil
		}); err != nil {
		return err
	}

	// Observer: react after the commit, unable to undo it.
	return modulekit.SubscribeTyped(hooks, HookAfterPost, extensionModuleID, 0,
		func(_ context.Context, record Record) (any, error) {
			m.observed = append(m.observed, record.Number)
			return nil, nil
		})
}

func (m *extensionModule) Start(context.Context) error { return nil }
func (m *extensionModule) Stop(context.Context) error  { return nil }

// secondExtensionModule exists to prove ordering between two independent
// extensions that know nothing about each other.
type secondExtensionModule struct {
	id       string
	priority int
	label    string
}

func (m *secondExtensionModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{Storage: modulekit.StorageNone, ID: m.id, Kind: "embedded"}
}

func (m *secondExtensionModule) Install(reg modulekit.Registrar) error {
	hooks, ok := modulekit.HookRegistrarFrom(reg)
	if !ok {
		return errors.New("host registrar offers no hook surface")
	}
	return modulekit.TransformTyped(hooks, HookComputeEntries, "", m.priority,
		func(_ context.Context, entries []Entry) ([]Entry, error) {
			return append(entries, Entry{Label: m.label, Amount: 1}), nil
		})
}

func (m *secondExtensionModule) Start(context.Context) error { return nil }
func (m *secondExtensionModule) Stop(context.Context) error  { return nil }

// hostileModule is a badly-behaved extension: it panics, hangs, or fails. A
// third party will eventually ship one of these by accident, so the flow owner
// needs to know what happens when they do.
type hostileModule struct {
	id       string
	ref      modulekit.HookRef
	misbehav func(context.Context) (any, error)
}

func (m *hostileModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{Storage: modulekit.StorageNone, ID: m.id, Kind: "embedded"}
}

func (m *hostileModule) Install(reg modulekit.Registrar) error {
	hooks, ok := modulekit.HookRegistrarFrom(reg)
	if !ok {
		return errors.New("host registrar offers no hook surface")
	}
	return hooks.Subscribe(modulekit.Subscription{
		Ref:      m.ref,
		Priority: 100, // runs after the well-behaved extension
		Handler: func(ctx context.Context, _ any) (any, error) {
			return m.misbehav(ctx)
		},
	})
}

func (m *hostileModule) Start(context.Context) error { return nil }
func (m *hostileModule) Stop(context.Context) error  { return nil }
