package scaffold_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sannados/sannad/cmd/sannad-gen/internal/scaffold"
)

func plan(t *testing.T) scaffold.Plan {
	t.Helper()
	p, err := scaffold.NewPlan(scaffold.Spec{
		ModuleID:   "com.acme.parties",
		Capability: "parties.reader",
		Version:    "v1",
		Dir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return p
}

// TestGeneratedFilesParse is the cheapest guard against the failure this tool is
// most prone to: a template that renders text which is not Go. Templates are
// strings, so nothing else checks them.
//
// Compiling the result is the real test and lives outside this package, because
// it needs a module and a network-free build. This catches the syntax errors
// before that.
func TestGeneratedFilesParse(t *testing.T) {
	files, err := scaffold.Render(plan(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	fset := token.NewFileSet()
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".go") {
			continue
		}
		if _, err := parser.ParseFile(fset, f.Name, f.Content, parser.AllErrors); err != nil {
			t.Errorf("%s is not valid Go: %v", f.Name, err)
		}
	}
}

// TestGeneratedModuleNamesNoInternalPackage: the generator's output is the first
// code a module author sees, so it must not demonstrate the one import that
// makes a module break on a kernel upgrade.
func TestGeneratedModuleNamesNoInternalPackage(t *testing.T) {
	files, err := scaffold.Render(plan(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, f := range files {
		if strings.Contains(f.Content, "sannad-os/internal/") {
			t.Errorf("%s references an internal package", f.Name)
		}
	}
}

// TestGeneratedContractCarriesNoTenantField and no engine struct tags. Both are
// rules the generated tests enforce for the author; the generator must not ship
// output that fails its own tests.
func TestGeneratedContractIsCorrectByConstruction(t *testing.T) {
	files, err := scaffold.Render(plan(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var contract string
	for _, f := range files {
		if f.Name == "contract.go" {
			contract = f.Content
		}
	}
	if contract == "" {
		t.Fatal("no contract.go generated")
	}

	// A request carrying a tenant is a tenant the caller controls.
	requestBlock := contract[strings.Index(contract, "Request struct"):]
	requestBlock = requestBlock[:strings.Index(requestBlock, "}")]
	if strings.Contains(requestBlock, "TenantID") {
		t.Error("generated request carries a tenant field")
	}

	// An engine struct tag couples the module to a database without importing
	// one, which is the coupling the storage boundary exists to prevent.
	if strings.Contains(contract, `gorm:"`) {
		t.Error("generated contract carries engine struct tags")
	}

	// The model must be tenant-aware or every query against it runs unscoped.
	if !strings.Contains(contract, "GetTenantID") || !strings.Contains(contract, "SetTenantID") {
		t.Error("generated model is not tenant-aware")
	}
}

func TestWriteRefusesToOverwrite(t *testing.T) {
	p := plan(t)
	if _, err := scaffold.Write(p); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// A generator that clobbers is one nobody runs twice, and the second run is
	// exactly when an author wants to compare.
	if _, err := scaffold.Write(p); err == nil {
		t.Fatal("second write overwrote an existing module")
	}
}

// TestWriteIsAtomicOnCollision: a partial module left behind is worse than none,
// because it compiles-ish and hides which files are the author's.
func TestWriteIsAtomicOnCollision(t *testing.T) {
	p := plan(t)
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Collide on a file the generator writes late.
	blocker := filepath.Join(p.Dir, "README.md")
	if err := os.WriteFile(blocker, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffold.Write(p); err == nil {
		t.Fatal("expected refusal")
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "module.go")); !os.IsNotExist(err) {
		t.Error("a file was written despite the collision; the module is half-generated")
	}
	content, err := os.ReadFile(blocker)
	if err != nil || string(content) != "mine" {
		t.Error("the existing file was modified")
	}
}

func TestInvalidSpecsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec scaffold.Spec
	}{
		{"not reverse DNS", scaffold.Spec{ModuleID: "parties", Capability: "parties.reader", Version: "v1", Dir: "x"}},
		{"uppercase ID", scaffold.Spec{ModuleID: "com.Acme.parties", Capability: "parties.reader", Version: "v1", Dir: "x"}},
		{"bad capability", scaffold.Spec{ModuleID: "com.acme.parties", Capability: "Parties Reader", Version: "v1", Dir: "x"}},
		{"bad version", scaffold.Spec{ModuleID: "com.acme.parties", Capability: "parties.reader", Version: "1.0", Dir: "x"}},
		{"no directory", scaffold.Spec{ModuleID: "com.acme.parties", Capability: "parties.reader", Version: "v1"}},
		{"keyword package", scaffold.Spec{ModuleID: "com.acme.range", Capability: "parties.reader", Version: "v1", Dir: "x"}},
	} {
		if _, err := scaffold.NewPlan(tc.spec); err == nil {
			t.Errorf("%s: expected refusal", tc.name)
		}
	}
}

// TestDerivedNamesAreStable pins the naming rules. Two files disagreeing about a
// derived name is the classic generator bug, and it produces code that does not
// compile for a reason the author did not cause.
func TestDerivedNamesAreStable(t *testing.T) {
	p := plan(t)
	if p.Package != "parties" {
		t.Errorf("package = %q, want parties", p.Package)
	}
	if p.TypePrefix != "Parties" {
		t.Errorf("type prefix = %q, want Parties", p.TypePrefix)
	}
	if p.CapabilityConst != "CapabilityPartiesReader" {
		t.Errorf("capability const = %q, want CapabilityPartiesReader", p.CapabilityConst)
	}
	if p.Table != "parties_parties" {
		t.Errorf("table = %q, want parties_parties", p.Table)
	}
}
