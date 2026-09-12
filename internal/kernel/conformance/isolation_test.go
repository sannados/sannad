package conformance_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The suite's central claim is that the extension attaches to the host through
// the registry alone. A comment saying so is worth nothing: the fixtures live in
// one package, so nothing stops a future edit from calling a host method
// directly and leaving every behavioural test passing while the claim quietly
// becomes false.
//
// These tests read the fixture source and enforce the claim structurally.

const (
	hostFile      = "host_test.go"
	extensionFile = "extension_test.go"
)

// hostOwnedTypes are the host's implementation types and constructors. Naming
// one means holding a reference to the module being extended.
//
// The extension may name the shared contract — hook refs and the value types
// they carry — because that is exactly what a third-party module would import
// from a published package. Sharing a contract is the mechanism; holding a
// reference to the implementation is not.
var hostOwnedTypes = []string{
	"hostModule",
	"newHostModule",
	"hostModuleID",
}

// hostOwnedMembers are fields and methods on the host. These are checked only as
// selector expressions (x.total()), never as bare identifiers: the extension has
// its own local named total, and a name-only check would flag it. What matters
// is reaching into something, not the spelling of a local variable.
var hostOwnedMembers = []string{
	"withDispatch",
	"defaultNumber",
	"total",
	"posted",
	"Post",
}

// TestExtensionNamesNoHostInternals fails if the extension fixture reaches into
// the host rather than attaching through the registry.
func TestExtensionNamesNoHostInternals(t *testing.T) {
	src, err := os.ReadFile(extensionFile)
	if err != nil {
		t.Fatalf("read %s: %v", extensionFile, err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, extensionFile, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", extensionFile, err)
	}

	forbiddenTypes := make(map[string]bool, len(hostOwnedTypes))
	for _, id := range hostOwnedTypes {
		forbiddenTypes[id] = true
	}
	forbiddenMembers := make(map[string]bool, len(hostOwnedMembers))
	for _, id := range hostOwnedMembers {
		forbiddenMembers[id] = true
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			// x.member — the shape of reaching into the host.
			if forbiddenMembers[node.Sel.Name] {
				t.Errorf("%s: extension selects host member %q — it must attach through "+
					"the hook registry, not by reaching into the module it extends",
					fset.Position(node.Sel.Pos()), node.Sel.Name)
			}
		case *ast.Ident:
			// A host type or constructor named anywhere means holding a
			// reference to the module being extended.
			if forbiddenTypes[node.Name] {
				t.Errorf("%s: extension names host type %q — it must not hold a reference "+
					"to the module it extends",
					fset.Position(node.Pos()), node.Name)
			}
		}
		return true
	})
}

// TestHostNamesNoExtension is the other direction, and the more important one:
// a host that knows its extensions is not extensible, it is just a program with
// its plugins hard-coded. This is what would fail if someone "fixed" a
// conformance test by teaching the host about the extension.
func TestHostNamesNoExtension(t *testing.T) {
	src, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatalf("read %s: %v", hostFile, err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, hostFile, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", hostFile, err)
	}

	extensionOwned := map[string]bool{
		"extensionModule":       true,
		"extensionModuleID":     true,
		"secondExtensionModule": true,
		"hostileModule":         true,
		"surchargeLabel":        true,
	}

	ast.Inspect(file, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if extensionOwned[ident.Name] {
			t.Errorf("%s: host names extension identifier %q — a host that knows its "+
				"extensions is not extensible",
				fset.Position(ident.Pos()), ident.Name)
		}
		return true
	})
}

// TestExtensionSubscribesOnlyToPublishedHooks confirms every hook the extension
// attaches to is one the host actually offers. A subscription to a hook point
// the host does not declare would be reaching past the contract even if it
// happened to work.
func TestExtensionSubscribesOnlyToPublishedHooks(t *testing.T) {
	_, dispatch := wire(t)

	offered := make(map[string]bool)
	for _, p := range dispatch.OfferedPoints() {
		offered[p.Ref.Key()] = true
	}

	src, err := os.ReadFile(extensionFile)
	if err != nil {
		t.Fatalf("read %s: %v", extensionFile, err)
	}
	// The extension names hook refs by their Go identifier; each must resolve to
	// a hook point the host declared.
	for _, name := range []string{"HookComputeEntries", "HookBeforePost", "HookDocumentNumber", "HookAfterPost"} {
		if !strings.Contains(string(src), name) {
			continue // this fixture does not use that hook point
		}
		ref := hookRefByName(t, name)
		if !offered[ref.Key()] {
			t.Errorf("extension subscribes to %s, which the host does not offer", ref.Key())
		}
	}
}

func hookRefByName(t *testing.T, name string) interface{ Key() string } {
	t.Helper()
	switch name {
	case "HookComputeEntries":
		return HookComputeEntries
	case "HookBeforePost":
		return HookBeforePost
	case "HookDocumentNumber":
		return HookDocumentNumber
	case "HookAfterPost":
		return HookAfterPost
	}
	t.Fatalf("unknown hook ref %q", name)
	return nil
}
