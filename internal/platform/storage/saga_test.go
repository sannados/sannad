package storage_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sannados/sannad/internal/platform/tenancy"
	"github.com/sannados/sannad/pkg/modulekit"
	"gorm.io/gorm"
)

// The saga coordinator lives in pkg/modulekit but cannot be tested there: its
// whole point is durable state across a crash, which needs a real Store, real
// transactions, and the real primary keys from migration 00005. These tests
// exercise it through the shipped adapter.

// migrateSagas registers the SDK saga tables as tenant-scoped. The schema comes
// from migration 00005 in newStore.
func migrateSagas(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := tenancy.RegisterScopedModels(db, &modulekit.SagaRecord{}, &modulekit.SagaStepRecord{}); err != nil {
		t.Fatalf("register saga models as scoped: %v", err)
	}
}

// recorder tracks which step bodies and compensations ran, in order. Order is
// what proves compensation unwinds in reverse.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

func (r *recorder) seq() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c == name {
			n++
		}
	}
	return n
}

func sameSeq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// step builds a step that records its calls and optionally fails.
func step(r *recorder, name string, doErr, compErr error) modulekit.SagaStep {
	return modulekit.SagaStep{
		Name: name,
		Do: func(context.Context) error {
			r.record("do:" + name)
			return doErr
		},
		Compensate: func(context.Context) error {
			r.record("undo:" + name)
			return compErr
		},
	}
}

func TestSagaRunsEveryStepInOrder(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
		step(&r, "charge", nil, nil),
		step(&r, "ship", nil, nil),
	}}

	if err := modulekit.RunSaga(ctx, store, saga, "order-1"); err != nil {
		t.Fatalf("RunSaga: %v", err)
	}

	want := []string{"do:reserve", "do:charge", "do:ship"}
	if got := r.seq(); !sameSeq(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}

	head, steps, err := modulekit.SagaState(ctx, store, "order-1")
	if err != nil {
		t.Fatalf("SagaState: %v", err)
	}
	if head.Status != modulekit.SagaCompleted {
		t.Fatalf("saga status %s, want completed", head.Status)
	}
	for _, s := range steps {
		if s.Status != modulekit.StepCompleted {
			t.Errorf("step %s status %s, want completed", s.Name, s.Status)
		}
	}
}

// TestSagaCompensatesInReverseOrder is the core guarantee. Later steps may
// depend on earlier ones, so undoing forward could remove a prerequisite while
// a dependent effect is still live.
func TestSagaCompensatesInReverseOrder(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	boom := errors.New("card declined")
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
		step(&r, "allocate", nil, nil),
		step(&r, "charge", boom, nil),
		step(&r, "ship", nil, nil),
	}}

	err := modulekit.RunSaga(ctx, store, saga, "order-2")
	if !errors.Is(err, modulekit.ErrSagaCompensated) {
		t.Fatalf("expected ErrSagaCompensated, got %v", err)
	}

	// "ship" never runs: the saga stopped at the failure. "charge" is not
	// compensated: it did not commit, so there is nothing to undo.
	want := []string{
		"do:reserve", "do:allocate", "do:charge",
		"undo:allocate", "undo:reserve",
	}
	if got := r.seq(); !sameSeq(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}

	head, steps, err := modulekit.SagaState(ctx, store, "order-2")
	if err != nil {
		t.Fatalf("SagaState: %v", err)
	}
	if head.Status != modulekit.SagaCompensated {
		t.Fatalf("saga status %s, want compensated", head.Status)
	}
	if head.FailureReason == "" {
		t.Error("compensated saga records no reason; an operator cannot tell why")
	}
	wantStatus := []modulekit.StepStatus{
		modulekit.StepCompensated,
		modulekit.StepCompensated,
		modulekit.StepFailed,
		modulekit.StepPending,
	}
	for i, s := range steps {
		if s.Status != wantStatus[i] {
			t.Errorf("step %s status %s, want %s", s.Name, s.Status, wantStatus[i])
		}
	}
}

// TestFailedCompensationIsTerminal: when the undo itself breaks, the system
// holds committed work nothing automatic can remove. That must be reported as
// distinct from a clean compensation, because a caller may safely retry the
// second and must never blindly retry the first.
func TestFailedCompensationIsTerminal(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, errors.New("refund gateway down")),
		step(&r, "charge", errors.New("declined"), nil),
	}}

	err := modulekit.RunSaga(ctx, store, saga, "order-3")
	if !errors.Is(err, modulekit.ErrSagaFailed) {
		t.Fatalf("expected ErrSagaFailed, got %v", err)
	}
	if errors.Is(err, modulekit.ErrSagaCompensated) {
		t.Fatal("a broken compensation reported as a clean one")
	}

	head, steps, err := modulekit.SagaState(ctx, store, "order-3")
	if err != nil {
		t.Fatalf("SagaState: %v", err)
	}
	if head.Status != modulekit.SagaFailed {
		t.Fatalf("saga status %s, want failed", head.Status)
	}
	if steps[0].Status != modulekit.StepCompensationFailed {
		t.Fatalf("step reserve status %s, want compensation_failed", steps[0].Status)
	}
	if steps[0].FailureReason == "" {
		t.Error("no reason recorded on the step a human has to resolve")
	}
}

// TestCompensationContinuesAfterAFailure: stopping at the first broken
// compensation would leave the remaining committed steps both untouched and
// unattempted. Those might have compensated cleanly, and an operator needs to
// know which steps actually still hold effects.
func TestCompensationContinuesAfterAFailure(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
		step(&r, "allocate", nil, errors.New("stuck")),
		step(&r, "charge", errors.New("declined"), nil),
	}}

	err := modulekit.RunSaga(ctx, store, saga, "order-4")
	if !errors.Is(err, modulekit.ErrSagaFailed) {
		t.Fatalf("expected ErrSagaFailed, got %v", err)
	}

	// reserve must still have been attempted despite allocate failing first.
	if r.count("undo:reserve") != 1 {
		t.Fatalf("undo:reserve ran %d times; compensation stopped at the first failure",
			r.count("undo:reserve"))
	}

	_, steps, err := modulekit.SagaState(ctx, store, "order-4")
	if err != nil {
		t.Fatalf("SagaState: %v", err)
	}
	if steps[0].Status != modulekit.StepCompensated {
		t.Errorf("reserve status %s, want compensated", steps[0].Status)
	}
	if steps[1].Status != modulekit.StepCompensationFailed {
		t.Errorf("allocate status %s, want compensation_failed", steps[1].Status)
	}
}

// TestNilCompensationIsRecorded: a step that only reads or only notifies has
// nothing to undo. That is a real case, but it is declared, and the durable
// state must not leave an operator wondering whether it was skipped by accident.
func TestNilCompensationIsRecorded(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		{Name: "notify", Do: func(context.Context) error { r.record("do:notify"); return nil }},
		step(&r, "charge", errors.New("declined"), nil),
	}}

	err := modulekit.RunSaga(ctx, store, saga, "order-5")
	if !errors.Is(err, modulekit.ErrSagaCompensated) {
		t.Fatalf("expected ErrSagaCompensated, got %v", err)
	}

	_, steps, err := modulekit.SagaState(ctx, store, "order-5")
	if err != nil {
		t.Fatalf("SagaState: %v", err)
	}
	if steps[0].Status != modulekit.StepCompensated {
		t.Fatalf("nil-compensation step recorded as %s, want compensated", steps[0].Status)
	}
}

// TestDuplicateSagaIDIsRefused: a saga ID is tied to whatever triggered the
// operation, so a repeat is a redelivery of that trigger. Starting a second
// copy of an in-flight saga would double every effect.
func TestDuplicateSagaIDIsRefused(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
	}}

	if err := modulekit.RunSaga(ctx, store, saga, "order-6"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	err := modulekit.RunSaga(ctx, store, saga, "order-6")
	if !errors.Is(err, modulekit.ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate on redelivery, got %v", err)
	}
	if r.count("do:reserve") != 1 {
		t.Fatalf("step ran %d times; a redelivered trigger started a second saga",
			r.count("do:reserve"))
	}
}

// TestConcurrentStartRunsOnce is the race the primary key exists to win. Two
// consumers handed the same trigger simultaneously must not both start a saga.
func TestConcurrentStartRunsOnce(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
	}}

	const starts = 8
	var wg sync.WaitGroup
	results := make([]error, starts)
	for i := 0; i < starts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = modulekit.RunSaga(ctx, store, saga, "order-race")
		}(i)
	}
	wg.Wait()

	if r.count("do:reserve") != 1 {
		t.Fatalf("step ran %d times under concurrent start, want exactly 1",
			r.count("do:reserve"))
	}
	var succeeded, duplicates int
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, modulekit.ErrDuplicate):
			duplicates++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d starts reported success, want 1", succeeded)
	}
	if succeeded+duplicates != starts {
		t.Errorf("not every start was accounted for")
	}
}

// TestSagasAreTenantIsolated: two tenants legitimately run the same operation
// with the same trigger ID. Neither must see or suppress the other.
func TestSagasAreTenantIsolated(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
	}}

	if err := modulekit.RunSaga(tenantCtx("tenant-a"), store, saga, "order-7"); err != nil {
		t.Fatalf("tenant-a: %v", err)
	}
	if err := modulekit.RunSaga(tenantCtx("tenant-b"), store, saga, "order-7"); err != nil {
		t.Fatalf("tenant-b: %v — one tenant suppressed another", err)
	}
	if r.count("do:reserve") != 2 {
		t.Fatalf("step ran %d times, want 2", r.count("do:reserve"))
	}

	// tenant-b must not be able to read tenant-a's saga state. Both wrote the
	// same saga ID, so a leak here returns a row rather than not-found.
	_, steps, err := modulekit.SagaState(tenantCtx("tenant-b"), store, "order-7")
	if err != nil {
		t.Fatalf("tenant-b SagaState: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("tenant-b sees %d steps, want 1 — saga state leaked across tenants", len(steps))
	}
}

// TestRecoverResumesForwardFromTheCrashPoint simulates the coordinator dying
// mid-flight: the durable state is left as a crash would leave it, and a fresh
// definition is handed to recovery. A coordinator that kept progress only in
// memory has nothing to resume from, which is what this proves it does not do.
func TestRecoverResumesForwardFromTheCrashPoint(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	// Leave the durable state as a crash after "charge" committed would:
	// two steps done, the head still marked running.
	if err := seedCrashedSaga(ctx, store, "order-8", "checkout",
		[]string{"reserve", "charge", "ship"}, 2); err != nil {
		t.Fatalf("seed crashed saga: %v", err)
	}

	// Fresh recorder: the recovering process has no memory of the first.
	var second recorder
	recovered := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&second, "reserve", nil, nil),
		step(&second, "charge", nil, nil),
		step(&second, "ship", nil, nil),
	}}

	if err := modulekit.RecoverSaga(ctx, store, recovered, "order-8"); err != nil {
		t.Fatalf("RecoverSaga: %v", err)
	}

	// Only "ship" should run. Re-running committed steps is the one thing
	// recovery must never do.
	want := []string{"do:ship"}
	if got := second.seq(); !sameSeq(got, want) {
		t.Fatalf("recovery ran %v, want %v", got, want)
	}

	head, _, err := modulekit.SagaState(ctx, store, "order-8")
	if err != nil {
		t.Fatalf("SagaState: %v", err)
	}
	if head.Status != modulekit.SagaCompleted {
		t.Fatalf("saga status %s, want completed", head.Status)
	}
}

// TestRecoverIsANoOpOnTerminalSagas: re-running a finished saga would build a
// second set of effects on top of the first.
func TestRecoverIsANoOpOnTerminalSagas(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
	}}
	if err := modulekit.RunSaga(ctx, store, saga, "order-9"); err != nil {
		t.Fatalf("RunSaga: %v", err)
	}

	if err := modulekit.RecoverSaga(ctx, store, saga, "order-9"); err != nil {
		t.Fatalf("RecoverSaga on a completed saga: %v", err)
	}
	if r.count("do:reserve") != 1 {
		t.Fatalf("recovery re-ran a completed saga (%d runs)", r.count("do:reserve"))
	}
}

// TestRecoverIsANoOpOnCompensatedAndFailedSagas is the case that actually needs
// the terminal guard. A completed saga has every step marked done, so the
// forward scan finds nothing to resume even without the guard. A compensated or
// failed one does not: its steps sit at compensated or failed, and recovery
// without the guard would read those as "not completed" and drive the saga
// forward — rebuilding, on top of effects that were deliberately undone,
// exactly the work the compensation just removed.
func TestRecoverIsANoOpOnCompensatedAndFailedSagas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status modulekit.SagaStatus
	}{
		{"compensated", modulekit.SagaCompensated},
		{"failed", modulekit.SagaFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := newStore(t)
			migrateSagas(t, db)
			ctx := tenantCtx("tenant-a")

			var r recorder
			saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
				step(&r, "reserve", nil, nil),
				step(&r, "charge", errors.New("declined"), nil),
			}}

			// Reach the terminal state through the real code path rather than
			// by seeding it, so the test breaks if that path stops producing it.
			if err := modulekit.RunSaga(ctx, store, saga, "order-"+tc.name); err == nil {
				t.Fatal("expected the saga to fail")
			}
			if err := forceSagaStatus(ctx, store, "order-"+tc.name, tc.status); err != nil {
				t.Fatalf("force status: %v", err)
			}

			before := len(r.seq())
			if err := modulekit.RecoverSaga(ctx, store, saga, "order-"+tc.name); err != nil {
				t.Fatalf("RecoverSaga on a %s saga: %v", tc.status, err)
			}
			if got := r.seq()[before:]; len(got) != 0 {
				t.Fatalf("recovery ran %v against a terminal (%s) saga", got, tc.status)
			}
		})
	}
}

// forceSagaStatus sets a saga head's status directly, to reach a terminal state
// whose exact shape a test needs.
func forceSagaStatus(ctx context.Context, store modulekit.Store, sagaID string, status modulekit.SagaStatus) error {
	return store.Update(ctx, &modulekit.SagaRecord{},
		map[string]any{"status": status},
		modulekit.Where("saga_id", sagaID))
}

// TestRecoverRefusesAMismatchedDefinition: a deploy that reordered or renamed
// steps between the failure and the recovery would otherwise compensate the
// wrong work — running one step's undo against another step's effect.
func TestRecoverRefusesAMismatchedDefinition(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	if err := seedCrashedSaga(ctx, store, "order-10", "checkout",
		[]string{"reserve", "charge", "ship"}, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var r recorder
	// Steps swapped, as a careless refactor would leave them.
	reordered := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
		step(&r, "ship", nil, nil),
		step(&r, "charge", nil, nil),
	}}
	err := modulekit.RecoverSaga(ctx, store, reordered, "order-10")
	if !errors.Is(err, modulekit.ErrInvalidSaga) {
		t.Fatalf("expected ErrInvalidSaga on reordered steps, got %v", err)
	}
	if len(r.seq()) != 0 {
		t.Fatalf("recovery ran %v against a mismatched definition", r.seq())
	}

	// A different step count is the other shape of the same mistake.
	shortened := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
	}}
	if err := modulekit.RecoverSaga(ctx, store, shortened, "order-10"); !errors.Is(err, modulekit.ErrInvalidSaga) {
		t.Fatalf("expected ErrInvalidSaga on a shortened definition, got %v", err)
	}

	// So is a saga of an entirely different type reusing the ID.
	other := modulekit.Saga{Name: "refund", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
	}}
	if err := modulekit.RecoverSaga(ctx, store, other, "order-10"); !errors.Is(err, modulekit.ErrInvalidSaga) {
		t.Fatalf("expected ErrInvalidSaga on a mismatched saga name, got %v", err)
	}
}

// TestRecoverResumesAnUnwind: a saga that crashed while compensating must keep
// compensating. The forward path was abandoned for a reason that is still true.
func TestRecoverResumesAnUnwind(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	if err := seedCompensatingSaga(ctx, store, "order-11", "checkout",
		[]string{"reserve", "charge"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
		step(&r, "charge", nil, nil),
	}}

	err := modulekit.RecoverSaga(ctx, store, saga, "order-11")
	if !errors.Is(err, modulekit.ErrSagaCompensated) {
		t.Fatalf("expected ErrSagaCompensated, got %v", err)
	}

	// Neither step's forward work may run: this saga is going backwards.
	for _, call := range r.seq() {
		if len(call) > 3 && call[:3] == "do:" {
			t.Fatalf("recovery ran forward work %q while compensating", call)
		}
	}
	want := []string{"undo:charge", "undo:reserve"}
	if got := r.seq(); !sameSeq(got, want) {
		t.Fatalf("unwind ran %v, want %v", got, want)
	}
}

// TestRunningStepIsReEntered covers the ambiguous crash: the coordinator died
// between recording a step as started and the step's own commit. It may or may
// not have happened, and nothing can tell which — so it is re-entered, and the
// contract requires steps to be idempotent for exactly this reason.
func TestRunningStepIsReEntered(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	// Step 1 left at "running": started, outcome unknown.
	if err := seedSagaWithRunningStep(ctx, store, "order-12", "checkout",
		[]string{"reserve", "charge"}, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
		step(&r, "charge", nil, nil),
	}}

	if err := modulekit.RecoverSaga(ctx, store, saga, "order-12"); err != nil {
		t.Fatalf("RecoverSaga: %v", err)
	}

	// "charge" re-runs; "reserve" is completed and must not.
	want := []string{"do:charge"}
	if got := r.seq(); !sameSeq(got, want) {
		t.Fatalf("recovery ran %v, want %v", got, want)
	}

	_, steps, err := modulekit.SagaState(ctx, store, "order-12")
	if err != nil {
		t.Fatalf("SagaState: %v", err)
	}
	// Attempts must show the re-entry, or a step failing the same way forever
	// is an invisible loop.
	if steps[1].Attempts < 2 {
		t.Fatalf("charge records %d attempts after a re-entry, want at least 2",
			steps[1].Attempts)
	}
}

// TestInFlightSagasFindsWhatRecoveryMustConsider. Startup has to ask this
// question; a saga nobody lists is a saga nobody recovers.
func TestInFlightSagasFindsWhatRecoveryMustConsider(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	done := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
	}}
	if err := modulekit.RunSaga(ctx, store, done, "order-done"); err != nil {
		t.Fatalf("completed saga: %v", err)
	}
	if err := seedCrashedSaga(ctx, store, "order-stuck", "checkout",
		[]string{"reserve", "charge"}, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}

	inFlight, err := modulekit.InFlightSagas(ctx, store)
	if err != nil {
		t.Fatalf("InFlightSagas: %v", err)
	}
	if len(inFlight) != 1 {
		t.Fatalf("found %d in-flight sagas, want 1", len(inFlight))
	}
	if inFlight[0].SagaID != "order-stuck" {
		t.Fatalf("found %s, want order-stuck", inFlight[0].SagaID)
	}
}

func TestInvalidSagaDefinitionsAreRefused(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	ok := func(context.Context) error { return nil }
	for _, tc := range []struct {
		name   string
		saga   modulekit.Saga
		sagaID string
	}{
		{"no name", modulekit.Saga{Steps: []modulekit.SagaStep{{Name: "a", Do: ok}}}, "s1"},
		{"no steps", modulekit.Saga{Name: "x"}, "s1"},
		{"step without name", modulekit.Saga{Name: "x", Steps: []modulekit.SagaStep{{Do: ok}}}, "s1"},
		{"step without work", modulekit.Saga{Name: "x", Steps: []modulekit.SagaStep{{Name: "a"}}}, "s1"},
		{"duplicate step names", modulekit.Saga{Name: "x", Steps: []modulekit.SagaStep{
			{Name: "a", Do: ok}, {Name: "a", Do: ok},
		}}, "s1"},
		{"no saga ID", modulekit.Saga{Name: "x", Steps: []modulekit.SagaStep{{Name: "a", Do: ok}}}, ""},
	} {
		err := modulekit.RunSaga(ctx, store, tc.saga, tc.sagaID)
		if !errors.Is(err, modulekit.ErrInvalidSaga) {
			t.Errorf("%s: expected ErrInvalidSaga, got %v", tc.name, err)
		}
	}
}

func TestSagaRequiresTenantContext(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
	}}
	err := modulekit.RunSaga(context.Background(), store, saga, "order-13")
	if !errors.Is(err, tenancy.ErrNoTenantContext) {
		t.Fatalf("expected ErrNoTenantContext, got %v", err)
	}
	if len(r.seq()) != 0 {
		t.Fatal("step ran with no tenant in context")
	}
}

func TestRecoverUnknownSaga(t *testing.T) {
	store, db := newStore(t)
	migrateSagas(t, db)
	ctx := tenantCtx("tenant-a")

	var r recorder
	saga := modulekit.Saga{Name: "checkout", Steps: []modulekit.SagaStep{
		step(&r, "reserve", nil, nil),
	}}
	if err := modulekit.RecoverSaga(ctx, store, saga, "nope"); !errors.Is(err, modulekit.ErrSagaNotFound) {
		t.Fatalf("expected ErrSagaNotFound, got %v", err)
	}
}

// --- seeding helpers -------------------------------------------------------
//
// These write the durable state a crashed coordinator would leave behind.
// Going through the Store rather than raw SQL keeps them honest: if the shape
// of that state changes, these break with the code.

func seedSteps(ctx context.Context, store modulekit.Store, sagaID string, names []string, statuses []modulekit.StepStatus) error {
	for i, name := range names {
		rec := modulekit.SagaStepRecord{
			SagaID:   sagaID,
			Position: i,
			Name:     name,
			Status:   statuses[i],
			Attempts: 1,
		}
		if statuses[i] == modulekit.StepPending {
			rec.Attempts = 0
		}
		if err := store.Create(ctx, &rec); err != nil {
			return fmt.Errorf("seed step %s: %w", name, err)
		}
	}
	return nil
}

// seedCrashedSaga leaves a saga recorded as running with the first `completed`
// steps done and the rest pending — the state a crash between steps produces.
func seedCrashedSaga(ctx context.Context, store modulekit.Store, sagaID, name string, steps []string, completed int) error {
	if err := store.Create(ctx, &modulekit.SagaRecord{
		SagaID: sagaID, Name: name, Status: modulekit.SagaRunning,
	}); err != nil {
		return err
	}
	statuses := make([]modulekit.StepStatus, len(steps))
	for i := range steps {
		if i < completed {
			statuses[i] = modulekit.StepCompleted
		} else {
			statuses[i] = modulekit.StepPending
		}
	}
	return seedSteps(ctx, store, sagaID, steps, statuses)
}

// seedSagaWithRunningStep leaves one step at "running": recorded as started,
// outcome unknown. Steps before it are completed.
func seedSagaWithRunningStep(ctx context.Context, store modulekit.Store, sagaID, name string, steps []string, running int) error {
	if err := store.Create(ctx, &modulekit.SagaRecord{
		SagaID: sagaID, Name: name, Status: modulekit.SagaRunning,
	}); err != nil {
		return err
	}
	statuses := make([]modulekit.StepStatus, len(steps))
	for i := range steps {
		switch {
		case i < running:
			statuses[i] = modulekit.StepCompleted
		case i == running:
			statuses[i] = modulekit.StepRunning
		default:
			statuses[i] = modulekit.StepPending
		}
	}
	return seedSteps(ctx, store, sagaID, steps, statuses)
}

// seedCompensatingSaga leaves a saga mid-unwind with every step committed.
func seedCompensatingSaga(ctx context.Context, store modulekit.Store, sagaID, name string, steps []string) error {
	if err := store.Create(ctx, &modulekit.SagaRecord{
		SagaID: sagaID, Name: name, Status: modulekit.SagaCompensating,
		FailureReason: "downstream refused",
	}); err != nil {
		return err
	}
	statuses := make([]modulekit.StepStatus, len(steps))
	for i := range steps {
		statuses[i] = modulekit.StepCompleted
	}
	return seedSteps(ctx, store, sagaID, steps, statuses)
}
