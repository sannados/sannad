// Package conformance_test is the hook conformance suite (roadmap step 4).
//
// Step 3's unit tests prove the dispatcher honours its guarantees. This suite
// proves the thing those tests cannot: that a module can extend a flow it does
// not own, through the registry alone, with no shared code and no edit to the
// module being extended. That is the kernel's central claim, and it is the one
// property that is expensive to discover is false after modules exist.
//
// These fixtures are throwaway. If the hook design changes they change with it;
// they are not a product and must never become one.
package conformance_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sannados/sannad/internal/kernel/hooks"
	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/pkg/modulekit"
)

// wire registers the host and any extensions in order, returning the host and
// the hook registry the flow dispatches through.
func wire(t *testing.T, extensions ...modulekit.Module) (*hostModule, *hooks.Registry) {
	t.Helper()

	reg := registry.New()
	host := newHostModule()
	if err := reg.RegisterModule(host); err != nil {
		t.Fatalf("register host: %v", err)
	}
	for _, ext := range extensions {
		if err := reg.RegisterModule(ext); err != nil {
			t.Fatalf("register %s: %v", ext.Descriptor().ID, err)
		}
	}
	host.withDispatch(reg.Hooks())
	return host, reg.Hooks()
}

func sample() Record {
	return Record{
		ID:       "rec-1",
		TenantID: "tenant-a",
		Entries: []Entry{
			{Label: "base", Amount: 100},
			{Label: "extra", Amount: 100},
		},
	}
}

// TestFlowRunsWithNoExtensions is the baseline. Most hook points have no
// subscribers most of the time, and a flow that only works when extended would
// be a flow nobody could ship.
func TestFlowRunsWithNoExtensions(t *testing.T) {
	host, _ := wire(t)

	posted, err := host.Post(context.Background(), sample())
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !posted.Posted {
		t.Fatal("record was not posted")
	}
	if posted.Number != "HOST-rec-1" {
		t.Fatalf("Number = %q, want the host default", posted.Number)
	}
	if len(posted.Entries) != 2 {
		t.Fatalf("expected the entries untouched, got %+v", posted.Entries)
	}
}

// TestExtensionMutatesDerivedValues is the transformer shape: a subscriber
// rewrites values the host computed, rather than only observing them.
func TestExtensionMutatesDerivedValues(t *testing.T) {
	ext := &extensionModule{}
	host, _ := wire(t, ext)

	posted, err := host.Post(context.Background(), sample())
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if len(posted.Entries) != 3 {
		t.Fatalf("expected the extension to contribute an entry, got %+v", posted.Entries)
	}
	added := posted.Entries[2]
	if added.Label != surchargeLabel || added.Amount != 20 {
		t.Fatalf("got %+v, want the 10%% surcharge on 200", added)
	}
	// The host's own total reflects the extension's contribution, which is what
	// distinguishes a transformer from an observer.
	if host.total() != 220 {
		t.Fatalf("host total = %d, want 220", host.total())
	}
}

// TestExtensionVetoesStateTransition is the validator shape, and the strongest
// form of extension: a module the host has never heard of stops the host's
// operation, with a reason attributed to it.
func TestExtensionVetoesStateTransition(t *testing.T) {
	ext := &extensionModule{limit: 150}
	host, _ := wire(t, ext)

	_, err := host.Post(context.Background(), sample())
	if !errors.Is(err, modulekit.ErrHookRejected) {
		t.Fatalf("expected the extension to veto, got %v", err)
	}

	rejections, ok := hooks.RejectionsFrom(err)
	if !ok || len(rejections) != 1 {
		t.Fatalf("expected one structured rejection, got %+v", rejections)
	}
	if rejections[0].ModuleID != extensionModuleID {
		t.Errorf("rejection attributed to %q, want %q", rejections[0].ModuleID, extensionModuleID)
	}
	if !strings.Contains(rejections[0].Reason, "exceeds limit") {
		t.Errorf("reason not carried through: %q", rejections[0].Reason)
	}

	// Nothing committed. A veto that still leaves state behind is not a veto.
	if len(host.posted) != 0 {
		t.Fatalf("a vetoed record was posted anyway: %+v", host.posted)
	}
}

// TestVetoSeesTransformedValue is the ordering property that makes the two
// shapes composable: the validator must judge the record as the transformers
// left it, not as it arrived. Here the surcharge is what pushes the total over
// the limit — the record would pass if validation ran first.
func TestVetoSeesTransformedValue(t *testing.T) {
	ext := &extensionModule{limit: 210} // 200 passes; 220 after the surcharge does not
	host, _ := wire(t, ext)

	_, err := host.Post(context.Background(), sample())
	if !errors.Is(err, modulekit.ErrHookRejected) {
		t.Fatalf("validator did not see the transformed entries: %v", err)
	}
}

// TestExtensionOwnsAGeneratedValue is the shape where a subscriber takes over
// something the host would otherwise supply — the host keeps a working default
// and cedes it when someone else wants it.
func TestExtensionOwnsAGeneratedValue(t *testing.T) {
	ext := &extensionModule{numberPrefix: "EXT"}
	host, _ := wire(t, ext)

	posted, err := host.Post(context.Background(), sample())
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if posted.Number != "EXT-HOST-rec-1" {
		t.Fatalf("Number = %q, want the extension's value", posted.Number)
	}
}

// TestObserverRunsAfterCommitAndCannotBlock is the observer shape. The
// extension sees the committed record, and its failure does not undo it.
func TestObserverRunsAfterCommitAndCannotBlock(t *testing.T) {
	ext := &extensionModule{}
	host, _ := wire(t, ext)

	posted, err := host.Post(context.Background(), sample())
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(ext.observed) != 1 || ext.observed[0] != posted.Number {
		t.Fatalf("observer saw %v, want [%s]", ext.observed, posted.Number)
	}
}

// TestTwoIndependentExtensionsOrderDeterministically: two extensions that know
// nothing about each other must compose predictably. Registration order must
// not decide the outcome, so the same pair is wired both ways round.
func TestTwoIndependentExtensionsOrderDeterministically(t *testing.T) {
	run := func(reversed bool) []string {
		early := &secondExtensionModule{id: "com.sannad.conformance.early", priority: 1, label: "early"}
		late := &secondExtensionModule{id: "com.sannad.conformance.late", priority: 50, label: "late"}

		var mods []modulekit.Module
		if reversed {
			mods = []modulekit.Module{late, early}
		} else {
			mods = []modulekit.Module{early, late}
		}
		host, _ := wire(t, mods...)

		posted, err := host.Post(context.Background(), sample())
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		labels := make([]string, len(posted.Entries))
		for i, e := range posted.Entries {
			labels[i] = e.Label
		}
		return labels
	}

	forward, reversed := run(false), run(true)
	want := []string{"base", "extra", "early", "late"}
	for _, got := range [][]string{forward, reversed} {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestHostileExtensionCannotCrashTheFlow: a third party will eventually ship a
// panicking subscriber. On a fail_closed point the operation aborts with an
// attributed error; the host process survives either way.
func TestHostileExtensionCannotCrashTheFlow(t *testing.T) {
	hostile := &hostileModule{
		id:  "com.sannad.conformance.panics",
		ref: HookComputeEntries,
		misbehav: func(context.Context) (any, error) {
			panic("third-party bug")
		},
	}
	host, _ := wire(t, hostile)

	_, err := host.Post(context.Background(), sample())
	if !errors.Is(err, modulekit.ErrHookSubscriberFailed) {
		t.Fatalf("expected an attributed subscriber failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "conformance.panics") {
		t.Errorf("error does not name the culprit: %v", err)
	}
	if len(host.posted) != 0 {
		t.Fatal("a record was posted despite a fail_closed subscriber failure")
	}
}

// TestHostileObserverCannotUndoACommit is the fail_open half. The observer hook
// point is declared fail_open precisely because it runs after the commit, and a
// failure there must not turn a committed record into an error.
func TestHostileObserverCannotUndoACommit(t *testing.T) {
	hostile := &hostileModule{
		id:  "com.sannad.conformance.badobserver",
		ref: HookAfterPost,
		misbehav: func(context.Context) (any, error) {
			return nil, errors.New("notification failed")
		},
	}
	host, _ := wire(t, hostile)

	posted, err := host.Post(context.Background(), sample())
	if err != nil {
		t.Fatalf("a post-commit observer failure surfaced as an error: %v", err)
	}
	if !posted.Posted || len(host.posted) != 1 {
		t.Fatal("the commit did not stand")
	}
}

// TestHangingExtensionCannotStallTheFlow: without the timeout this test hangs
// rather than fails, which is the honest way to test a liveness property.
func TestHangingExtensionCannotStallTheFlow(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	hostile := &hostileModule{
		id:  "com.sannad.conformance.hangs",
		ref: HookBeforePost,
		misbehav: func(context.Context) (any, error) {
			<-release
			return nil, nil
		},
	}
	host, _ := wire(t, hostile)

	start := time.Now()
	_, err := host.Post(context.Background(), sample())
	elapsed := time.Since(start)

	if !errors.Is(err, modulekit.ErrHookSubscriberFailed) {
		t.Fatalf("expected a timeout failure, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the flow waited %s on a hung subscriber", elapsed)
	}
}

// TestEveryOfferedHookPointIsDocumented guards the ADR 0003 requirement that
// offering a hook point is a public API commitment. An undocumented extension
// point is one nobody outside the authoring team can use.
func TestEveryOfferedHookPointIsDocumented(t *testing.T) {
	_, dispatch := wire(t)

	points := dispatch.OfferedPoints()
	if len(points) != 4 {
		t.Fatalf("expected 4 hook points, got %d", len(points))
	}
	for _, p := range points {
		if p.Description == "" {
			t.Errorf("hook point %s has no description", p.Ref.Key())
		}
		if err := p.Validate(); err != nil {
			t.Errorf("hook point %s is malformed: %v", p.Ref.Key(), err)
		}
	}
}

// TestExtensionAttachesWithoutHostKnowledge is the claim stated directly. The
// host was registered before the extension existed, offers the same hook points
// either way, and its own code path is identical — everything that differs is
// contributed from outside.
func TestExtensionAttachesWithoutHostKnowledge(t *testing.T) {
	bare, bareDispatch := wire(t)
	extended, extDispatch := wire(t, &extensionModule{numberPrefix: "EXT"})

	// The host offers the same contract in both worlds.
	if len(bareDispatch.OfferedPoints()) != len(extDispatch.OfferedPoints()) {
		t.Fatal("the host's offered hook points changed when an extension was added")
	}
	// And has no subscribers of its own.
	if ids := bareDispatch.SubscriberIDs(HookComputeEntries); len(ids) != 0 {
		t.Fatalf("the bare host has subscribers: %v", ids)
	}

	bareResult, err := bare.Post(context.Background(), sample())
	if err != nil {
		t.Fatalf("bare Post: %v", err)
	}
	extResult, err := extended.Post(context.Background(), sample())
	if err != nil {
		t.Fatalf("extended Post: %v", err)
	}

	if bareResult.Number == extResult.Number {
		t.Fatal("the extension changed nothing")
	}
	if len(extResult.Entries) <= len(bareResult.Entries) {
		t.Fatal("the extension contributed no entries")
	}
}

// TestHookPayloadsSurviveATierChange enforces the claim ADR 0001 rests on: the
// same capability lives at any tier, and callers cannot tell the difference.
//
// The host fixture's payloads travel by pointer in tier 1 and are never encoded,
// so a payload holding a func, a channel, or unexported state would pass every
// other test in this suite and then fail the day the module moved to WASM or
// behind the gateway — the one moment the design promised nothing would change.
//
// Checking the fixtures rather than a synthetic type is the point: these are the
// shapes the conformance suite claims a real module can have.
func TestHookPayloadsSurviveATierChange(t *testing.T) {
	// Every value that crosses one of the host's four hook points.
	for _, payload := range []any{
		Record{},  // before_post, after_post
		[]Entry{}, // compute_entries
		"",        // document_number
	} {
		if err := modulekit.AssertPortable(payload); err != nil {
			t.Errorf("hook payload %T cannot cross a tier boundary: %v", payload, err)
		}
	}
}
