package modulekit_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// forbiddenImports are engine and driver packages that must never appear in the
// public module SDK. A module compiling against modulekit must not be able to
// name a storage engine, because that is what makes the engine a bootstrap
// decision rather than a property of every module ever written. See ADR 0006.
var forbiddenImports = []string{
	"gorm.io/",
	"database/sql",
	"github.com/glebarez/sqlite",
	"github.com/lib/pq",
	"github.com/jackc/pgx",
	"go.mongodb.org/",
}

// TestSDKNamesNoEngineType enforces the layering rule as a test rather than a
// convention. ADR 0006's whole guarantee rests on this package staying free of
// engine types, and a rule that is only written down is one an ordinary import
// added in a hurry can quietly break.
func TestSDKNamesNoEngineType(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	for name, pkg := range pkgs {
		// The boundary applies to the SDK itself. This test file is in the
		// external test package and imports nothing engine-related, but skip
		// any _test package on principle: test scaffolding is not the SDK.
		if strings.HasSuffix(name, "_test") {
			continue
		}

		for path, file := range pkg.Files {
			for _, spec := range file.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatalf("%s: unquote import %s: %v", path, spec.Path.Value, err)
				}
				for _, forbidden := range forbiddenImports {
					if strings.HasPrefix(imported, forbidden) {
						t.Errorf("%s imports %q: the module SDK must name no engine type (ADR 0006). "+
							"Define the capability on modulekit.Store and implement it in an adapter under internal/platform/storage.",
							positionOf(fset, spec), imported)
					}
				}
			}
		}
	}
}

func positionOf(fset *token.FileSet, node ast.Node) string {
	return fset.Position(node.Pos()).String()
}

// TestSDKCarriesNoEngineStructTags closes a blind spot in the import check
// above: a struct tag names an engine without importing one, so `gorm:"..."` on
// an SDK model passes an import-only test while coupling every module to GORM
// exactly as an import would. This was a real violation, caught here rather
// than in review.
//
// Column and constraint definitions belong in the migration, which is engine
// code by definition, not in the SDK's public models.
func TestSDKCarriesNoEngineStructTags(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	forbiddenTags := []string{"gorm:", "db:", "bson:", "sql:"}

	for name, pkg := range pkgs {
		if strings.HasSuffix(name, "_test") {
			continue
		}
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				field, ok := n.(*ast.Field)
				if !ok || field.Tag == nil {
					return true
				}
				for _, forbidden := range forbiddenTags {
					if strings.Contains(field.Tag.Value, forbidden) {
						t.Errorf("%s: struct tag names a storage engine (%s): %s. "+
							"Engine mapping belongs in the migration and the adapter, not in the SDK.",
							positionOf(fset, field.Tag), forbidden, field.Tag.Value)
					}
				}
				return true
			})
		}
	}
}
