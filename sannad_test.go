package sannad_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkayan/kayan/core/tenant"
	sannad "github.com/sannados/sannad"
	"github.com/sannados/sannad/examples/embedded/notes"
	"github.com/sannados/sannad/pkg/modulekit"
)

func testConfig(t *testing.T) sannad.Config {
	t.Helper()
	return sannad.Config{
		Env:                 "development",
		KernelAddr:          ":0",
		GatewayAddr:         ":0",
		GRPCAddr:            "",
		DatabaseDSN:         filepath.Join(t.TempDir(), "sannad-public-test.db"),
		SessionSecret:       "public-api-test-session-secret-long-enough",
		KayanIssuer:         "http://localhost:8080",
		CORSOrigins:         "*",
		TenantSelfProvision: true,
	}
}

func TestPublicAppRegistersWASM(t *testing.T) {
	app, err := sannad.New(testConfig(t))
	if err != nil {
		t.Fatalf("sannad.New: %v", err)
	}
	defer func() { _ = app.Shutdown(context.Background()) }()

	wasm, err := os.ReadFile(filepath.Join("internal", "kernel", "wasmhost", "testdata", "guest.wasm"))
	if err != nil {
		t.Fatalf("read guest: %v", err)
	}
	ref := modulekit.CapabilityRef{Name: "public.sandbox.echo", Version: "v1"}
	if err := app.RegisterWASM(t.Context(), sannad.WASMConfig{
		Manifest: sannad.WASMManifest{
			ID:         "com.example.public-sandbox",
			ABIVersion: sannad.CurrentWASMABIVersion,
			Provides:   []modulekit.CapabilityRef{ref},
		},
		Wasm: wasm,
	}); err != nil {
		t.Fatalf("RegisterWASM: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	result, err := app.Call(tenant.WithTenantID(t.Context(), "acme"), ref, map[string]any{"amount": 6})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var response struct {
		Doubled  int    `json:"doubled"`
		TenantID string `json:"tenant_id"`
	}
	if err := json.Unmarshal(result.(modulekit.RawPayload), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Doubled != 12 || response.TenantID != "acme" {
		t.Fatalf("unexpected response: %+v", response)
	}
}

// downstreamCheckModule provides a trivial capability for
// TestPublicAppRegistersWASMWithHostCallGrants to grant a v2 guest as
// Consumes — an ordinary embedded module, registered the same way any
// consumer of the public API would.
type downstreamCheckModule struct{}

var downstreamCheckRef = modulekit.CapabilityRef{Name: "public.inventory.check", Version: "v1"}

func (downstreamCheckModule) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{
		ID:       "com.example.public-downstream",
		Kind:     "embedded",
		Storage:  modulekit.StorageNone,
		Provides: []modulekit.CapabilityRef{downstreamCheckRef},
	}
}

func (downstreamCheckModule) Install(reg modulekit.Registrar) error {
	return reg.RegisterCapability(downstreamCheckRef, func(context.Context, any) (any, error) {
		return modulekit.RawPayload(`{"in_stock":true}`), nil
	})
}

func (downstreamCheckModule) Start(context.Context) error { return nil }
func (downstreamCheckModule) Stop(context.Context) error  { return nil }

// TestPublicAppRegistersWASMWithHostCallGrants proves the public sannad
// package can actually grant a sandboxed module the ABI v2 host-call surface
// (ADR 0007) — Consumes, here — not only provide/expose. Before this test
// (and the WASMManifest/WASMConfig fields it exercises), a third party
// embedding this framework through the sannad package alone had no way to
// grant a guest an outbound capability call, an event publish, or a hook
// subscription at all: those fields existed only on the internal
// wasmhost.ApprovedManifest this package never surfaced.
func TestPublicAppRegistersWASMWithHostCallGrants(t *testing.T) {
	app, err := sannad.New(testConfig(t))
	if err != nil {
		t.Fatalf("sannad.New: %v", err)
	}
	defer func() { _ = app.Shutdown(context.Background()) }()

	if err := app.RegisterModule(downstreamCheckModule{}); err != nil {
		t.Fatalf("RegisterModule: %v", err)
	}

	wasm, err := os.ReadFile(filepath.Join("internal", "kernel", "wasmhost", "testdata", "guestv2.wasm"))
	if err != nil {
		t.Fatalf("read guestv2: %v", err)
	}
	v2Ref := modulekit.CapabilityRef{Name: "public.sandbox.v2", Version: "v1"}
	if err := app.RegisterWASM(t.Context(), sannad.WASMConfig{
		Manifest: sannad.WASMManifest{
			ID:         "com.example.public-sandbox-v2",
			ABIVersion: sannad.CurrentWASMABIVersion,
			Provides:   []modulekit.CapabilityRef{v2Ref},
			Consumes:   []modulekit.CapabilityRef{downstreamCheckRef},
		},
		Wasm: wasm,
	}); err != nil {
		t.Fatalf("RegisterWASM: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	req := map[string]any{"call_capability": downstreamCheckRef.Key()}
	result, err := app.Call(tenant.WithTenantID(t.Context(), "acme"), v2Ref, req)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(result.(modulekit.RawPayload)) != `{"in_stock":true}` {
		t.Fatalf("got %s, want the downstream capability's raw response forwarded through the guest", result.(modulekit.RawPayload))
	}
}

func TestPublicAppRegistersScopedModuleWithoutInternalImports(t *testing.T) {
	app, err := sannad.New(testConfig(t))
	if err != nil {
		t.Fatalf("sannad.New: %v", err)
	}
	defer func() { _ = app.Shutdown(context.Background()) }()

	module := notes.New(app.Store())
	// No RegisterScopedModels call: the module owns that declaration.
	if err := app.RegisterModule(module); err != nil {
		t.Fatalf("RegisterModule: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx := tenant.WithTenantID(t.Context(), "acme")
	if _, err := module.Add(ctx, "public API"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := app.Call(ctx, notes.CapabilityNoteReader, notes.ListRequest{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	response, ok := result.(notes.ListResponse)
	if !ok || len(response.Notes) != 1 || response.Notes[0].Body != "public API" {
		t.Fatalf("unexpected response: %#v", result)
	}
}

// TestExternalConsumerCompiles proves Go's internal-package rule from outside
// this module. An in-repository test cannot catch the broken import the old
// documentation recommended because internal imports are legal from here.
func TestExternalConsumerCompiles(t *testing.T) {
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	dir := t.TempDir()
	goMod := "module example.com/sannad-consumer\n\ngo 1.25.7\n\n" +
		"require github.com/sannados/sannad v0.0.0\n\n" +
		"replace github.com/sannados/sannad => " + filepath.ToSlash(repo) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "consumer_test.go"), []byte(externalConsumerSource), 0o600); err != nil {
		t.Fatalf("write consumer: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "go", "test", "-mod=mod", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("external consumer does not compile: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "use of internal package") {
		t.Fatalf("external consumer reached an internal package:\n%s", output)
	}
}

const externalConsumerSource = `package consumer_test

import (
	"context"
	"path/filepath"
	"testing"

	sannad "github.com/sannados/sannad"
	"github.com/sannados/sannad/pkg/modulekit"
)

var ref = modulekit.CapabilityRef{Name: "community.echo", Version: "v1"}

type module struct{}

func (module) Descriptor() modulekit.Descriptor {
	return modulekit.Descriptor{ID: "com.community.echo", Kind: "embedded", Storage: modulekit.StorageNone, Provides: []modulekit.CapabilityRef{ref}}
}
func (module) Install(reg modulekit.Registrar) error {
	return modulekit.HandleTyped(reg, ref, func(_ context.Context, value string) (string, error) { return "hello " + value, nil })
}
func (module) Start(context.Context) error { return nil }
func (module) Stop(context.Context) error { return nil }

func TestFramework(t *testing.T) {
	app, err := sannad.New(sannad.Config{
		Env: "development", KernelAddr: ":0", GatewayAddr: ":0", GRPCAddr: "",
		DatabaseDSN: filepath.Join(t.TempDir(), "consumer.db"),
		SessionSecret: "external-consumer-session-secret-long-enough",
		KayanIssuer: "http://localhost:8080", CORSOrigins: "*",
	})
	if err != nil { t.Fatal(err) }
	defer app.Shutdown(context.Background())
	if err := app.RegisterModule(module{}); err != nil { t.Fatal(err) }
	if err := app.Start(t.Context()); err != nil { t.Fatal(err) }
	got, err := modulekit.CallTyped[string, string](t.Context(), app, ref, "world")
	if err != nil { t.Fatal(err) }
	if got != "hello world" { t.Fatalf("got %q", got) }
}
`
