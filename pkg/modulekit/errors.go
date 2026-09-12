package modulekit

import "errors"

// ErrForbidden is returned by a capability handler when the caller does not
// have permission to access the requested resource (e.g. tenant mismatch).
// Gateway handlers should map this to HTTP 403.
var ErrForbidden = errors.New("modulekit: forbidden")

// ErrNotFound is returned by a capability handler when the requested resource
// does not exist. Gateway handlers should map this to HTTP 404.
var ErrNotFound = errors.New("modulekit: not found")

// ErrUnconditional is returned when a Delete or Update is issued with no
// conditions. Rewriting or emptying a whole table is far more often a forgotten
// predicate than an intended operation, so the unconditional form is refused.
// Deliberate bulk work belongs in a migration.
var ErrUnconditional = errors.New("modulekit: write without conditions")

// ErrUnsupportedOperator is returned when a Condition names an operator the
// adapter does not implement.
var ErrUnsupportedOperator = errors.New("modulekit: unsupported operator")

// ErrInvalidHookPoint is returned when a hook point declaration is malformed —
// missing a name or version, an unknown kind, no failure policy, or a
// non-positive timeout. Refused at registration rather than discovered when a
// subscriber misbehaves.
var ErrInvalidHookPoint = errors.New("modulekit: invalid hook point")

// ErrHookNotOffered is returned when a module subscribes to a hook point no
// module offers. Subscribing into silence is almost always a typo or a missing
// dependency, and failing loudly at wiring time is cheaper than a hook that
// never fires.
var ErrHookNotOffered = errors.New("modulekit: hook point not offered")

// ErrHookAlreadyOffered is returned when two modules offer the same hook ref.
// A hook point has exactly one owner; two owners means two different contracts
// behind one name.
var ErrHookAlreadyOffered = errors.New("modulekit: hook point already offered")

// ErrHookRejected is returned by a validator dispatch when at least one
// subscriber vetoed. The error carries every rejection, not only the first.
var ErrHookRejected = errors.New("modulekit: operation rejected by hook subscriber")

// ErrHookSubscriberFailed is returned when a subscriber on a fail_closed hook
// point returns an error, times out, or panics.
var ErrHookSubscriberFailed = errors.New("modulekit: hook subscriber failed")

// ErrTypeMismatch is returned when a typed handler or subscriber receives a
// value of the wrong type. This is a wiring error — two modules disagreeing
// about a contract — and is reported rather than silently skipped.
var ErrTypeMismatch = errors.New("modulekit: type mismatch")

// ErrDuplicate is returned when a write violates a primary-key or unique
// constraint. Adapters translate their engine's constraint violation into this,
// the same way they translate a missing row into ErrNotFound, so a module can
// recognise the condition without naming a driver's error type.
var ErrDuplicate = errors.New("modulekit: duplicate key")
