package modulekit

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ADR 0001 defines three tiers — embedded Go, sandboxed WASM, external app —
// and stakes the design on one claim: "The same capability lives at any tier...
// and callers cannot tell the difference." It names the cost plainly:
// "contracts must be serializable and tier-agnostic".
//
// Nothing enforced that. A payload carrying a func, a channel, or state in
// unexported fields compiles, dispatches in-process, and passes every test,
// because tier 1 hands the value over by pointer and never encodes it. The
// failure arrives the day the module moves to WASM or behind the gateway —
// which is exactly when the design promised nothing would change.
//
// This is the same shape as a rule written only in prose. The check below turns
// the ADR's cost line into something that fails.

// ErrNotPortable is returned when a value cannot cross a tier boundary. It is a
// contract error, not a runtime one: the type is wrong for the job, and no
// input will make it right.
var ErrNotPortable = errors.New("modulekit: contract is not portable across tiers")

// AssertPortable reports whether a capability or hook payload can cross a
// process boundary.
//
// Call it on the request and response types of every capability, and on the
// value type of every hook point. A module that only ever runs in-process will
// pass its own tests either way; this is what tells its author, at the time
// they write it, that the contract would not survive the move to WASM or an
// external app.
//
// What it rejects, and why each one actually breaks:
//
//   - func and chan — no encoding exists for behaviour or for a reference to
//     another goroutine's scheduler. These do not fail at the boundary, they
//     fail to have any meaning on the far side of it.
//   - unsafe.Pointer and uintptr — an address is meaningful only inside one
//     address space, and both WASM and a remote process have their own.
//   - unexported fields carrying state — an encoder cannot read them, so the
//     value silently arrives incomplete rather than failing loudly. This is the
//     one that bites hardest: in-process it works perfectly.
//
// What it deliberately allows: any exported field of a serializable kind,
// including nested structs, slices, maps, and pointers. Interfaces are allowed
// at declaration time because the concrete type is what matters and cannot be
// known here — that gap is real and stated rather than hidden.
func AssertPortable(v any) error {
	if v == nil {
		return fmt.Errorf("%w: nil carries no type", ErrNotPortable)
	}
	root := reflect.TypeOf(v)
	return checkPortable(root, root, nil, map[reflect.Type]bool{})
}

// checkPortable walks a type, recording the field path so a failure names the
// exact member rather than only the top-level type. A contract error that says
// "Record is not portable" sends an author looking; one that says
// "Record.Entries[].validate is a func" is already the fix.
func checkPortable(root, t reflect.Type, path []string, seen map[reflect.Type]bool) error {
	// A type may reference itself — a tree node, a linked list. Without this the
	// walk would not terminate.
	if seen[t] {
		return nil
	}
	seen[t] = true

	switch t.Kind() {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Uintptr:
		return fmt.Errorf("%w: %s is a %s, which has no representation outside this process",
			ErrNotPortable, describe(root, path), t.Kind())

	case reflect.Ptr, reflect.Slice, reflect.Array:
		return checkPortable(root, t.Elem(), path, seen)

	case reflect.Map:
		if err := checkPortable(root, t.Key(), append(path, "key"), seen); err != nil {
			return err
		}
		return checkPortable(root, t.Elem(), append(path, "value"), seen)

	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)

			// An unexported field is invisible to every encoder. If it carries
			// state, the value arrives on the far side incomplete — and
			// silently, which is worse than a refusal.
			//
			// Zero-width fields are exempt: they carry nothing, so nothing is
			// lost. That covers the common marker and alignment idioms.
			if field.PkgPath != "" {
				if field.Type.Size() == 0 {
					continue
				}
				return fmt.Errorf(
					"%w: %s is unexported, so an encoder cannot read it and the value would arrive incomplete",
					ErrNotPortable, describe(root, append(path, field.Name)))
			}

			if err := checkPortable(root, field.Type, append(path, field.Name), seen); err != nil {
				return err
			}
		}
		return nil

	default:
		// Numbers, strings, bools, interfaces. Interfaces pass because the
		// concrete type is what has to be portable and is not knowable here.
		return nil
	}
}

// describe renders the path from the root contract type down to the offending
// member. The root is what the author declared, so naming the leaf type instead
// would print the problem twice and the location not at all.
func describe(root reflect.Type, path []string) string {
	name := root.String()
	if len(path) == 0 {
		return name
	}
	return name + "." + strings.Join(path, ".")
}
