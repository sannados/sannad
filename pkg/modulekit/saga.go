package modulekit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ADR 0006 grants a module a transaction over its own data and forbids one
// spanning two modules. So a business operation touching several modules —
// reserve stock, charge the card, book the shipment — has no single unit of
// work to roll back. Something has to sequence the steps and undo the ones
// that already committed when a later one fails. That is a saga.
//
// The hard part is not the happy path. It is that the coordinator itself can
// crash between any two steps, and on restart must know exactly how far it got.
// State that lives only in memory tells it nothing after a restart, and a saga
// that forgets where it was either repeats committed work or abandons it
// half-done. So every transition is durable before the step it describes runs.
//
// The order is deliberate: mark the step running, run it, mark the outcome. A
// coordinator that ran first and recorded after would, on a crash in between,
// restart a step it had already executed with no record that it had. Recording
// first means the worst case is a step marked running that may or may not have
// completed — which is recoverable, because a step is required to be
// idempotent, and "unknown" is a state the recovery path can act on.
// "Unrecorded" is not.

// SagaStatus is the lifecycle position of a saga or one of its steps.
type SagaStatus string

const (
	// SagaRunning means steps are executing forward.
	SagaRunning SagaStatus = "running"

	// SagaCompleted means every step succeeded. Terminal.
	SagaCompleted SagaStatus = "completed"

	// SagaCompensating means a step failed and the coordinator is undoing the
	// steps that already committed, in reverse order.
	SagaCompensating SagaStatus = "compensating"

	// SagaCompensated means the forward work failed and every committed step
	// was successfully undone. Terminal, and the *expected* failure outcome:
	// the operation did not happen and the system is consistent.
	SagaCompensated SagaStatus = "compensated"

	// SagaFailed means compensation itself failed. Terminal, and the outcome
	// that needs a human: the system is in a state no automatic path could
	// resolve, and pretending otherwise by retrying forever would hide it.
	SagaFailed SagaStatus = "failed"
)

// StepStatus is the lifecycle position of a single step.
type StepStatus string

const (
	// StepPending means the step has not started.
	StepPending StepStatus = "pending"

	// StepRunning means the step was recorded as started. On recovery this is
	// the ambiguous state: the step may or may not have completed before the
	// crash. Because steps must be idempotent, the recovery path may re-run it.
	StepRunning StepStatus = "running"

	// StepCompleted means the step committed and now needs compensating if a
	// later step fails.
	StepCompleted StepStatus = "completed"

	// StepFailed means the step returned an error. It is not compensated: work
	// that did not commit has nothing to undo.
	StepFailed StepStatus = "failed"

	// StepCompensating means the step's compensation was recorded as started.
	StepCompensating StepStatus = "compensating"

	// StepCompensated means the step's effect was successfully undone.
	StepCompensated StepStatus = "compensated"

	// StepCompensationFailed means the compensation returned an error. This is
	// what turns a saga into SagaFailed, because the step's effect is still
	// live and nothing automatic can remove it.
	StepCompensationFailed StepStatus = "compensation_failed"
)

// SagaRecord is the durable head of one saga instance.
//
// Column names are given explicitly rather than left to an adapter's naming
// convention, since the migration and the model have to agree and only one of
// them is in this package.
type SagaRecord struct {
	TenantID string
	SagaID   string
	Name     string
	Status   SagaStatus

	// FailureReason records why the saga left the happy path. It is kept as
	// text rather than a wrapped error because it has to survive a process
	// restart, and an operator reading a stuck saga needs to know what went
	// wrong without correlating logs that may have rotated.
	FailureReason string

	StartedAt time.Time
	UpdatedAt time.Time
}

func (s *SagaRecord) GetTenantID() string   { return s.TenantID }
func (s *SagaRecord) SetTenantID(id string) { s.TenantID = id }

func (SagaRecord) TableName() string { return "modulekit_sagas" }

// SagaStepRecord is the durable state of one step within a saga.
//
// Position is stored so recovery can compensate in reverse order without
// depending on the step definitions still being in the same order in code — a
// deploy between the failure and the recovery would otherwise silently reorder
// the undo.
type SagaStepRecord struct {
	TenantID string
	SagaID   string
	Position int
	Name     string
	Status   StepStatus

	// Attempts counts how many times the step body has been entered. A step
	// found at StepRunning during recovery is re-entered, and this is what
	// makes a step that fails the same way every time visible rather than an
	// invisible loop.
	Attempts int

	FailureReason string
	UpdatedAt     time.Time
}

func (s *SagaStepRecord) GetTenantID() string   { return s.TenantID }
func (s *SagaStepRecord) SetTenantID(id string) { s.TenantID = id }

func (SagaStepRecord) TableName() string { return "modulekit_saga_steps" }

// SagaStep is one unit of forward work and the compensation that undoes it.
type SagaStep struct {
	// Name identifies the step in durable state and in operator-facing output.
	// It must be stable across deploys: recovery matches recorded steps to
	// definitions by position, and reports this name when they disagree.
	Name string

	// Do performs the forward work.
	//
	// It must be idempotent. A crash after Do committed but before its outcome
	// was recorded leaves the step at StepRunning, and recovery will call Do
	// again — there is no way for the coordinator to tell that case from a
	// crash before Do ran. Idempotent here means safe to enter twice, which for
	// a step whose work is an event consumer is exactly what Idempotent
	// provides, and for a step that writes its own tables means an upsert or a
	// guard on its own state.
	Do func(ctx context.Context) error

	// Compensate undoes what Do committed. It is called only for steps recorded
	// as StepCompleted, in reverse order, when a later step fails.
	//
	// It must be idempotent for the same reason as Do, and it must tolerate
	// being called for work it cannot find — a compensation that fails because
	// the thing it wanted to undo is already gone would turn a recoverable
	// saga into SagaFailed for no reason.
	//
	// A nil Compensate declares the step has nothing to undo. That is a real
	// case — a step that only reads, or only emits a notification — but it is
	// stated explicitly rather than inferred, because a forgotten compensation
	// and a deliberately absent one look identical otherwise.
	Compensate func(ctx context.Context) error
}

// Saga is a named sequence of steps executed in order.
type Saga struct {
	// Name identifies the saga type, not the instance. It appears in durable
	// state so an operator can find every stuck instance of one flow.
	Name string

	Steps []SagaStep
}

// ErrSagaCompensated is returned when forward work failed and every committed
// step was successfully undone. This is the *expected* failure: the operation
// did not happen and the system is consistent. Callers should report the
// operation as declined, not as a system error.
var ErrSagaCompensated = errors.New("modulekit: saga compensated")

// ErrSagaFailed is returned when compensation itself failed. The system holds
// committed work that could not be undone. This needs a human, and a caller
// must not retry it blindly — the saga is terminal and a retry would build a
// second set of effects on top of the first.
var ErrSagaFailed = errors.New("modulekit: saga compensation failed")

// ErrInvalidSaga is returned when a saga definition is malformed. Refused at
// call time rather than discovered halfway through execution, where the failure
// would already have committed work.
var ErrInvalidSaga = errors.New("modulekit: invalid saga")

// ErrSagaNotFound is returned when a saga ID names no recorded instance.
var ErrSagaNotFound = errors.New("modulekit: saga not found")

// Validate reports whether the definition can be executed.
func (s Saga) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("%w: saga has no name", ErrInvalidSaga)
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("%w: saga %s has no steps", ErrInvalidSaga, s.Name)
	}

	seen := make(map[string]int, len(s.Steps))
	for i, step := range s.Steps {
		if step.Name == "" {
			return fmt.Errorf("%w: saga %s step %d has no name", ErrInvalidSaga, s.Name, i)
		}
		if step.Do == nil {
			return fmt.Errorf("%w: saga %s step %s has no forward work",
				ErrInvalidSaga, s.Name, step.Name)
		}
		// Duplicate names would make durable state ambiguous to an operator
		// reading it, and make a recovery mismatch impossible to describe.
		if prev, dup := seen[step.Name]; dup {
			return fmt.Errorf("%w: saga %s has two steps named %s (positions %d and %d)",
				ErrInvalidSaga, s.Name, step.Name, prev, i)
		}
		seen[step.Name] = i
	}
	return nil
}

// RunSaga executes a saga to completion, compensating committed steps if any
// step fails.
//
// sagaID identifies this instance and must be supplied by the caller, not
// generated here. It is what makes the operation resumable and what ties the
// saga to whatever triggered it — an order ID, an event ID. A generated ID
// would be lost with the process that generated it.
//
// Each step runs outside a transaction. This is the point of a saga: the steps
// touch different modules, and ADR 0006 forbids a transaction spanning them.
// Durable state transitions do use transactions, over the coordinator's own
// tables, which the same ADR permits.
//
// Returns nil on success, ErrSagaCompensated when the operation was cleanly
// undone, and ErrSagaFailed when compensation broke and the state needs a
// human.
//
// Requires the kernel store. Durable state is what makes a saga survive the
// coordinator crashing, and a module declaring StorageOwn has nowhere for the
// kernel to put it.
func RunSaga(ctx context.Context, store Store, saga Saga, sagaID string) error {
	if err := saga.Validate(); err != nil {
		return err
	}
	if sagaID == "" {
		return fmt.Errorf("%w: saga %s needs an instance ID", ErrInvalidSaga, saga.Name)
	}

	if err := beginSaga(ctx, store, saga, sagaID); err != nil {
		return err
	}
	return driveSaga(ctx, store, saga, sagaID, 0)
}

// beginSaga records the saga head and every step as pending, in one
// transaction, before any step runs.
//
// Writing all the steps up front rather than as they start is what lets
// recovery see the shape of the flow without the definition — it can report
// that a recorded saga has steps the current code no longer defines, instead of
// silently skipping them.
func beginSaga(ctx context.Context, store Store, saga Saga, sagaID string) error {
	return store.Transact(ctx, func(tx Store) error {
		head := SagaRecord{
			SagaID:    sagaID,
			Name:      saga.Name,
			Status:    SagaRunning,
			StartedAt: now(),
			UpdatedAt: now(),
		}
		if err := tx.Create(ctx, &head); err != nil {
			if isDuplicateKey(err) {
				// A saga ID is meant to be tied to the thing that triggered it,
				// so a repeat is a redelivery of that trigger, not a new
				// operation. Refusing it here is what stops a redelivered
				// message from starting a second copy of an in-flight saga.
				return fmt.Errorf("%w: saga %s already exists", ErrDuplicate, sagaID)
			}
			return fmt.Errorf("start saga %s: %w", sagaID, err)
		}

		for i, step := range saga.Steps {
			rec := SagaStepRecord{
				SagaID:    sagaID,
				Position:  i,
				Name:      step.Name,
				Status:    StepPending,
				UpdatedAt: now(),
			}
			if err := tx.Create(ctx, &rec); err != nil {
				return fmt.Errorf("record step %s of saga %s: %w", step.Name, sagaID, err)
			}
		}
		return nil
	})
}

// driveSaga runs forward from a position, then compensates if anything fails.
func driveSaga(ctx context.Context, store Store, saga Saga, sagaID string, from int) error {
	for i := from; i < len(saga.Steps); i++ {
		step := saga.Steps[i]

		// Recorded before the work runs. A crash between this write and the
		// step's own commit leaves the step at StepRunning, which recovery
		// treats as "may or may not have happened" and re-enters. Recording
		// after the work would leave that same crash indistinguishable from
		// "never started", and the step would be re-run with no record that it
		// had ever been attempted.
		if err := markStepRunning(ctx, store, sagaID, i); err != nil {
			return err
		}

		if err := step.Do(ctx); err != nil {
			reason := fmt.Sprintf("step %s failed: %v", step.Name, err)
			if markErr := markStep(ctx, store, sagaID, i, StepFailed, reason); markErr != nil {
				return markErr
			}
			// Compensate from the previous step: this one did not commit, so
			// there is nothing of it to undo.
			return compensate(ctx, store, saga, sagaID, i-1, reason)
		}

		if err := markStep(ctx, store, sagaID, i, StepCompleted, ""); err != nil {
			return err
		}
	}

	return markSaga(ctx, store, sagaID, SagaCompleted, "")
}

// compensate undoes committed steps in reverse order, starting at from.
//
// Reverse order is not cosmetic. Later steps may depend on earlier ones, so
// undoing forward could remove a prerequisite while a dependent effect is still
// live.
func compensate(ctx context.Context, store Store, saga Saga, sagaID string, from int, reason string) error {
	if err := markSaga(ctx, store, sagaID, SagaCompensating, reason); err != nil {
		return err
	}

	// Collected rather than returned on the first failure. Stopping at the
	// first broken compensation would leave the remaining committed steps
	// untouched *and* unattempted, which is strictly worse: those steps might
	// have compensated cleanly, and an operator resolving the saga by hand
	// needs to know which ones actually still hold effects.
	var failures []error

	for i := from; i >= 0; i-- {
		step := saga.Steps[i]

		status, err := stepStatus(ctx, store, sagaID, i)
		if err != nil {
			return err
		}
		// Only committed work has anything to undo. A step at StepRunning is
		// ambiguous — it may have committed — so it is compensated too, which
		// is safe because Compensate is required to tolerate absent work.
		if status != StepCompleted && status != StepRunning {
			continue
		}

		if step.Compensate == nil {
			// Declared as having nothing to undo. Recorded as compensated so
			// the durable state does not leave an operator wondering whether
			// the step was skipped by accident.
			if err := markStep(ctx, store, sagaID, i, StepCompensated, ""); err != nil {
				return err
			}
			continue
		}

		if err := markStep(ctx, store, sagaID, i, StepCompensating, ""); err != nil {
			return err
		}

		if err := step.Compensate(ctx); err != nil {
			failure := fmt.Errorf("compensating %s: %w", step.Name, err)
			failures = append(failures, failure)
			if markErr := markStep(ctx, store, sagaID, i,
				StepCompensationFailed, failure.Error()); markErr != nil {
				return markErr
			}
			continue
		}

		if err := markStep(ctx, store, sagaID, i, StepCompensated, ""); err != nil {
			return err
		}
	}

	if len(failures) > 0 {
		joined := errors.Join(failures...)
		detail := fmt.Sprintf("%s; compensation failed: %v", reason, joined)
		if err := markSaga(ctx, store, sagaID, SagaFailed, detail); err != nil {
			return err
		}
		return fmt.Errorf("%w: saga %s: %s", ErrSagaFailed, sagaID, detail)
	}

	if err := markSaga(ctx, store, sagaID, SagaCompensated, reason); err != nil {
		return err
	}
	return fmt.Errorf("%w: saga %s: %s", ErrSagaCompensated, sagaID, reason)
}

// RecoverSaga resumes a saga left in flight by a crash.
//
// A coordinator that only ran in memory would have nothing to resume: this
// exists because the process that started a saga is not guaranteed to be the
// one that finishes it. Call it at startup for every saga still recorded as
// running or compensating.
//
// The definition passed here must be the same saga type that started the
// instance. Recovery matches recorded steps to definitions by position and
// refuses to proceed if the names disagree, because a deploy that reordered or
// renamed steps would otherwise compensate the wrong work — running one step's
// undo against another step's effect.
func RecoverSaga(ctx context.Context, store Store, saga Saga, sagaID string) error {
	if err := saga.Validate(); err != nil {
		return err
	}

	var head SagaRecord
	err := store.First(ctx, &head, Where("saga_id", sagaID))
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrSagaNotFound, sagaID)
	}
	if err != nil {
		return fmt.Errorf("read saga %s: %w", sagaID, err)
	}

	if head.Name != saga.Name {
		return fmt.Errorf("%w: saga %s was recorded as %q, definition is %q",
			ErrInvalidSaga, sagaID, head.Name, saga.Name)
	}

	switch head.Status {
	case SagaCompleted, SagaCompensated, SagaFailed:
		// Terminal. Re-running would build a second set of effects on top of
		// the first, which is the one thing recovery must never do.
		return nil
	}

	steps, err := recordedSteps(ctx, store, sagaID)
	if err != nil {
		return err
	}
	if len(steps) != len(saga.Steps) {
		return fmt.Errorf("%w: saga %s recorded %d steps, definition has %d",
			ErrInvalidSaga, sagaID, len(steps), len(saga.Steps))
	}
	for i, rec := range steps {
		if rec.Name != saga.Steps[i].Name {
			return fmt.Errorf("%w: saga %s step %d was recorded as %q, definition has %q",
				ErrInvalidSaga, sagaID, i, rec.Name, saga.Steps[i].Name)
		}
	}

	if head.Status == SagaCompensating {
		// Already unwinding. Resume the unwind rather than going forward — the
		// forward path was abandoned for a reason that is still true.
		return compensate(ctx, store, saga, sagaID, len(saga.Steps)-1, head.FailureReason)
	}

	// Forward. Resume at the first step not yet completed. A step at
	// StepRunning is re-entered: it may or may not have committed, and
	// idempotency is what makes that safe.
	resume := len(saga.Steps)
	for i, rec := range steps {
		if rec.Status != StepCompleted {
			resume = i
			break
		}
	}
	return driveSaga(ctx, store, saga, sagaID, resume)
}

// InFlightSagas lists sagas that are neither completed nor terminally failed,
// which is the set recovery has to consider at startup.
func InFlightSagas(ctx context.Context, store Store) ([]SagaRecord, error) {
	var sagas []SagaRecord
	err := store.Find(ctx, &sagas,
		In("status", []SagaStatus{SagaRunning, SagaCompensating}))
	if err != nil {
		return nil, fmt.Errorf("list in-flight sagas: %w", err)
	}
	return sagas, nil
}

// SagaState reports a saga's head record and its steps, for operator tooling
// and for the SagaFailed case where a human has to decide what to do.
func SagaState(ctx context.Context, store Store, sagaID string) (SagaRecord, []SagaStepRecord, error) {
	var head SagaRecord
	err := store.First(ctx, &head, Where("saga_id", sagaID))
	if errors.Is(err, ErrNotFound) {
		return SagaRecord{}, nil, fmt.Errorf("%w: %s", ErrSagaNotFound, sagaID)
	}
	if err != nil {
		return SagaRecord{}, nil, fmt.Errorf("read saga %s: %w", sagaID, err)
	}

	steps, err := recordedSteps(ctx, store, sagaID)
	if err != nil {
		return SagaRecord{}, nil, err
	}
	return head, steps, nil
}

// recordedSteps loads a saga's steps ordered by position.
//
// The Store contract has no ORDER BY, so ordering is applied here from the
// recorded position rather than trusted from the engine's return order — which
// is not guaranteed and would silently reverse a compensation.
func recordedSteps(ctx context.Context, store Store, sagaID string) ([]SagaStepRecord, error) {
	var rows []SagaStepRecord
	if err := store.Find(ctx, &rows, Where("saga_id", sagaID)); err != nil {
		return nil, fmt.Errorf("read steps of saga %s: %w", sagaID, err)
	}

	ordered := make([]SagaStepRecord, len(rows))
	for _, row := range rows {
		if row.Position < 0 || row.Position >= len(rows) {
			return nil, fmt.Errorf("%w: saga %s has a step at position %d of %d",
				ErrInvalidSaga, sagaID, row.Position, len(rows))
		}
		ordered[row.Position] = row
	}
	return ordered, nil
}

// stepStatus reads one step's recorded status.
func stepStatus(ctx context.Context, store Store, sagaID string, position int) (StepStatus, error) {
	var rec SagaStepRecord
	err := store.First(ctx, &rec,
		Where("saga_id", sagaID),
		Where("position", position))
	if err != nil {
		return "", fmt.Errorf("read step %d of saga %s: %w", position, sagaID, err)
	}
	return rec.Status, nil
}

// markStepRunning records a step as started and increments its attempt count.
//
// Attempts is read and rewritten rather than incremented in place because the
// Store contract has no expression-valued update. That is safe without a
// transaction around the pair: one saga instance is driven by one coordinator
// at a time, which the saga ID's primary key enforces at start. Wrapping it in
// a transaction anyway would widen the write-lock window across a read for no
// added guarantee, and on a single-writer engine that is enough to make a
// legitimate saga fail on contention with sagas that are merely being refused.
func markStepRunning(ctx context.Context, store Store, sagaID string, position int) error {
	var rec SagaStepRecord
	if err := store.First(ctx, &rec,
		Where("saga_id", sagaID), Where("position", position)); err != nil {
		return fmt.Errorf("read step %d of saga %s: %w", position, sagaID, err)
	}

	err := store.Update(ctx, &SagaStepRecord{}, map[string]any{
		"status":     StepRunning,
		"attempts":   rec.Attempts + 1,
		"updated_at": now(),
	}, Where("saga_id", sagaID), Where("position", position))
	if err != nil {
		return fmt.Errorf("mark step %d of saga %s as running: %w", position, sagaID, err)
	}
	return nil
}

// markStep records a step's outcome.
func markStep(ctx context.Context, store Store, sagaID string, position int, status StepStatus, reason string) error {
	err := store.Update(ctx, &SagaStepRecord{}, map[string]any{
		"status":         status,
		"failure_reason": reason,
		"updated_at":     now(),
	}, Where("saga_id", sagaID), Where("position", position))
	if err != nil {
		return fmt.Errorf("mark step %d of saga %s as %s: %w", position, sagaID, status, err)
	}
	return nil
}

// markSaga records the saga head's status.
func markSaga(ctx context.Context, store Store, sagaID string, status SagaStatus, reason string) error {
	err := store.Update(ctx, &SagaRecord{}, map[string]any{
		"status":         status,
		"failure_reason": reason,
		"updated_at":     now(),
	}, Where("saga_id", sagaID))
	if err != nil {
		return fmt.Errorf("mark saga %s as %s: %w", sagaID, status, err)
	}
	return nil
}
