package registry_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sannados/sannad/internal/kernel/registry"
	"github.com/sannados/sannad/pkg/modulekit"
)

// A module that never declared where it keeps its data is exactly the one whose
// author did not consider tenant isolation. Defaulting to the safe answer would
// hide that; refusing puts the question in front of them at registration.
type undeclaredModule struct{}

func (undeclaredModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{ID: "undeclared", Kind: "embedded"}
}
func (undeclaredModule) Install(modulekit.Registrar) error { return nil }
func (undeclaredModule) Start(context.Context) error       { return nil }
func (undeclaredModule) Stop(context.Context) error        { return nil }

func TestUndeclaredStorageIsRefused(t *testing.T) {
	err := registry.New().RegisterModule(undeclaredModule{})
	if err == nil {
		t.Fatal("a module that declared no Storage registered silently")
	}
	// The message must name the options, so the fix is in the error rather than
	// in documentation the author has to go find.
	for _, want := range []string{"kernel", "own", "none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q as an option: %v", want, err)
		}
	}
}
