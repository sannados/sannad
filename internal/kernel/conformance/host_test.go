package conformance_test

import (
	"context"
	"fmt"
	"time"

	"github.com/sannados/sannad/internal/kernel/hooks"
	"github.com/sannados/sannad/pkg/modulekit"
)

// This file is the *host* fixture: a module that owns a flow and opens it to
// participation. It is deliberately domain-neutral — a "record" with "entries"
// and a posting step — because the kernel must not presume an industry, and a
// fixture named after one invites the design to bend toward it.
//
// The extension fixture lives in extension_test.go and imports nothing from
// here. That is the property under test: a module extending a flow it does not
// own, through the registry alone, with no shared code and no edit to this file.

const hostModuleID = "com.sannad.conformance.host"

// The four hook points, one per shape the registry claims to support.
var (
	// HookComputeEntries lets subscribers rewrite derived values.
	HookComputeEntries = modulekit.HookRef{Name: "conformance.compute_entries", Version: "v1"}

	// HookBeforePost lets subscribers veto a state transition.
	HookBeforePost = modulekit.HookRef{Name: "conformance.before_post", Version: "v1"}

	// HookDocumentNumber lets a subscriber own a value the host would supply.
	HookDocumentNumber = modulekit.HookRef{Name: "conformance.document_number", Version: "v1"}

	// HookAfterPost notifies subscribers once the transition has committed.
	HookAfterPost = modulekit.HookRef{Name: "conformance.after_post", Version: "v1"}
)

// Entry is a line in a record. Subscribers receive and return these.
type Entry struct {
	Label  string
	Amount int
}

// Record is the aggregate the host flow operates on.
type Record struct {
	ID       string
	TenantID string
	Entries  []Entry
	Number   string
	Posted   bool
}

// hostModule owns the flow. It knows nothing about who extends it.
type hostModule struct {
	dispatch *hooks.Registry

	// posted records what committed, so a test can assert that a vetoed flow
	// left nothing behind.
	posted []Record
}

func newHostModule() *hostModule { return &hostModule{} }

func (m *hostModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:   hostModuleID,
		Kind: "embedded",
		// The fixture holds posted records in memory: the conformance suite
		// tests extension, not storage.
		Storage: modulekit.StorageNone,
		Offers: []modulekit.HookPoint{
			{
				Ref:         HookComputeEntries,
				Kind:        modulekit.KindTransformer,
				Policy:      modulekit.FailClosed,
				Timeout:     time.Second,
				Description: "Rewrite the entries derived for a record before it is posted.",
			},
			{
				Ref:         HookBeforePost,
				Kind:        modulekit.KindValidator,
				Policy:      modulekit.FailClosed,
				Timeout:     time.Second,
				Description: "Veto a record before it is posted. Every veto is collected.",
			},
			{
				Ref:         HookDocumentNumber,
				Kind:        modulekit.KindTransformer,
				Policy:      modulekit.FailClosed,
				Timeout:     time.Second,
				Description: "Supply the document number for a record being posted.",
			},
			{
				Ref:         HookAfterPost,
				Kind:        modulekit.KindObserver,
				Policy:      modulekit.FailOpen,
				Timeout:     time.Second,
				Description: "React to a posted record. Cannot block the operation.",
			},
		},
	}
}

func (m *hostModule) Install(modulekit.Registrar) error { return nil }
func (m *hostModule) Start(context.Context) error       { return nil }
func (m *hostModule) Stop(context.Context) error        { return nil }

// withDispatch gives the host the hook registry to dispatch through. In a real
// module this would arrive through the module's own wiring; the fixture keeps
// it explicit so a test can see exactly what the flow depends on.
func (m *hostModule) withDispatch(d *hooks.Registry) *hostModule {
	m.dispatch = d
	return m
}

// Post is the flow under test. Its shape is the point: four extension points at
// the four places a real posting flow needs them, with the veto before the
// commit and the notification after it.
func (m *hostModule) Post(ctx context.Context, record Record) (Record, error) {
	// 1. Transformer — subscribers may rewrite the derived entries.
	computed, err := m.dispatch.Transform(ctx, HookComputeEntries, record.Entries)
	if err != nil {
		return Record{}, fmt.Errorf("compute entries: %w", err)
	}
	entries, ok := computed.([]Entry)
	if !ok {
		return Record{}, fmt.Errorf("compute entries: subscriber returned %T, want []Entry", computed)
	}
	record.Entries = entries

	// 2. Validator — subscribers may veto, before anything is committed.
	if err := m.dispatch.Validate(ctx, HookBeforePost, record); err != nil {
		return Record{}, fmt.Errorf("before post: %w", err)
	}

	// 3. Transformer — a subscriber may own the document number. The host
	//    supplies a default so the flow works with no subscriber at all.
	numbered, err := m.dispatch.Transform(ctx, HookDocumentNumber, m.defaultNumber(record))
	if err != nil {
		return Record{}, fmt.Errorf("document number: %w", err)
	}
	number, ok := numbered.(string)
	if !ok {
		return Record{}, fmt.Errorf("document number: subscriber returned %T, want string", numbered)
	}
	record.Number = number

	// 4. Commit. Everything above this line can still abort the operation;
	//    nothing below it can.
	record.Posted = true
	m.posted = append(m.posted, record)

	// 5. Observer — after the commit, and unable to undo it. A failure here is
	//    logged and skipped, which is why the hook point is fail_open.
	if err := m.dispatch.Notify(ctx, HookAfterPost, record); err != nil {
		return Record{}, fmt.Errorf("after post: %w", err)
	}

	return record, nil
}

func (m *hostModule) defaultNumber(record Record) string {
	return "HOST-" + record.ID
}

func (m *hostModule) total() int {
	sum := 0
	for _, r := range m.posted {
		for _, e := range r.Entries {
			sum += e.Amount
		}
	}
	return sum
}
