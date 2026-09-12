package modulekit_test

import (
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/sannados/sannad/pkg/modulekit"
)

// These cover the ADR 0001 cost line — "contracts must be serializable and
// tier-agnostic" — which until now was prose with nothing behind it.

type portableRequest struct {
	Kind   string
	Limit  int
	Tags   []string
	Labels map[string]string
}

type nested struct {
	Inner portableRequest
	More  []portableRequest
}

func TestPortableContractsPass(t *testing.T) {
	for _, v := range []any{
		portableRequest{},
		&portableRequest{},
		nested{},
		[]portableRequest{},
		map[string]portableRequest{},
		"", 0, false,
	} {
		if err := modulekit.AssertPortable(v); err != nil {
			t.Errorf("%T should be portable: %v", v, err)
		}
	}
}

// A func field is the case that looks harmless in tier 1: the value is handed
// over by pointer and the func is simply called. There is no encoding of
// behaviour, so the same contract cannot exist at any other tier.
func TestFuncFieldIsRefused(t *testing.T) {
	type withFunc struct {
		Kind     string
		Validate func(string) error
	}
	err := modulekit.AssertPortable(withFunc{})
	if !errors.Is(err, modulekit.ErrNotPortable) {
		t.Fatalf("expected ErrNotPortable, got %v", err)
	}
	// The message must name the member, not only the type: an author reading
	// "withFunc is not portable" goes looking, one reading ".Validate" is done.
	if !strings.Contains(err.Error(), "Validate") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}

func TestChannelFieldIsRefused(t *testing.T) {
	type withChan struct {
		Done chan struct{}
	}
	if err := modulekit.AssertPortable(withChan{}); !errors.Is(err, modulekit.ErrNotPortable) {
		t.Fatalf("expected ErrNotPortable, got %v", err)
	}
}

func TestPointerKindsAreRefused(t *testing.T) {
	type withUnsafe struct{ P unsafe.Pointer }
	type withUintptr struct{ P uintptr }

	for _, v := range []any{withUnsafe{}, withUintptr{}} {
		if err := modulekit.AssertPortable(v); !errors.Is(err, modulekit.ErrNotPortable) {
			t.Errorf("%T: expected ErrNotPortable, got %v", v, err)
		}
	}
}

// The worst case, because it is the quietest. An unexported field is invisible
// to every encoder, so the value does not fail to cross — it crosses with the
// field missing.
func TestUnexportedStateIsRefused(t *testing.T) {
	type withHiddenState struct {
		Kind   string
		cached []string
	}
	err := modulekit.AssertPortable(withHiddenState{})
	if !errors.Is(err, modulekit.ErrNotPortable) {
		t.Fatalf("expected ErrNotPortable, got %v", err)
	}
	if !strings.Contains(err.Error(), "cached") {
		t.Errorf("error does not name the hidden field: %v", err)
	}
}

// A zero-width unexported field carries no state, so nothing is lost crossing a
// boundary. Refusing it would reject the ordinary no-compare and marker idioms
// for no benefit.
func TestZeroWidthUnexportedFieldIsAllowed(t *testing.T) {
	type withMarker struct {
		_    [0]func() // the standard "do not compare me" marker
		Kind string
	}
	if err := modulekit.AssertPortable(withMarker{}); err != nil {
		t.Errorf("zero-width marker should be allowed: %v", err)
	}
}

// Nesting is where this earns its place. A payload is rarely unportable at the
// top level; it holds something that holds something that holds a func.
func TestNestedViolationIsFound(t *testing.T) {
	type deep struct{ Run func() }
	type middle struct{ Items []deep }
	type outer struct {
		Kind string
		Mid  middle
	}

	err := modulekit.AssertPortable(outer{})
	if !errors.Is(err, modulekit.ErrNotPortable) {
		t.Fatalf("nested func field not found: %v", err)
	}
	if !strings.Contains(err.Error(), "Run") {
		t.Errorf("error does not name the nested field: %v", err)
	}
}

func TestMapValueViolationIsFound(t *testing.T) {
	type withFuncMap struct {
		Handlers map[string]func()
	}
	if err := modulekit.AssertPortable(withFuncMap{}); !errors.Is(err, modulekit.ErrNotPortable) {
		t.Fatalf("expected ErrNotPortable, got %v", err)
	}
}

// A self-referencing type must not hang the walk.
func TestRecursiveTypeTerminates(t *testing.T) {
	type node struct {
		Label    string
		Children []*node
	}
	if err := modulekit.AssertPortable(node{}); err != nil {
		t.Errorf("recursive type should be portable: %v", err)
	}
}

func TestNilIsRefused(t *testing.T) {
	if err := modulekit.AssertPortable(nil); !errors.Is(err, modulekit.ErrNotPortable) {
		t.Fatalf("expected ErrNotPortable for nil, got %v", err)
	}
}
