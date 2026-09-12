package bootstrap_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	kernelV1 "github.com/sannados/sannad/contracts/gen/go/sannad/kernel/v1"
	"github.com/sannados/sannad/internal/app/bootstrap"
	"github.com/sannados/sannad/internal/kernel/wasmhost"
	"github.com/sannados/sannad/pkg/config"
	"github.com/sannados/sannad/pkg/modulekit"
)

var (
	wasmPublicRef = modulekit.CapabilityRef{Name: "sandbox.echo", Version: "v1"}
	wasmHiddenRef = modulekit.CapabilityRef{Name: "sandbox.internal", Version: "v1"}
)

func wasmFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "kernel", "wasmhost", "testdata", "guest.wasm")
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read WASM fixture: %v", err)
	}
	return bytes
}

func wasmApp(t *testing.T) *bootstrap.App {
	t.Helper()
	app, err := bootstrap.New(config.Config{
		Env:                 "development",
		KernelAddr:          ":0",
		GatewayAddr:         ":0",
		DatabaseDSN:         filepath.Join(t.TempDir(), "wasm-bridge-test.db"),
		SessionSecret:       "wasm-bridge-test-session-secret-long-enough",
		KayanIssuer:         "http://localhost:8080",
		CORSOrigins:         "*",
		TenantSelfProvision: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	if err := app.RegisterWASM(t.Context(), wasmhost.ModuleConfig{
		Manifest: wasmhost.ApprovedManifest{
			ID:         "com.example.gateway-sandbox",
			ABIVersion: wasmhost.CurrentABIVersion,
			Provides:   []modulekit.CapabilityRef{wasmPublicRef, wasmHiddenRef},
			Exposed:    []modulekit.CapabilityRef{wasmPublicRef},
		},
		Wasm:    wasmFixture(t),
		Timeout: 5 * time.Second,
	}); err != nil {
		_ = app.Shutdown(context.Background())
		t.Fatalf("RegisterWASM: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		_ = app.Shutdown(context.Background())
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	return app
}

func TestWASMModuleEndToEnd(t *testing.T) {
	app := wasmApp(t)
	token := bridgeToken(t, app)

	t.Run("HTTP preserves contract and tenant", func(t *testing.T) {
		response := bridgeCall(t, app, token, wasmPublicRef.Key(), map[string]any{"amount": 9})
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("gateway returned %d, want 200", response.StatusCode)
		}
		var body struct {
			Doubled  int    `json:"doubled"`
			TenantID string `json:"tenant_id"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.Doubled != 18 || body.TenantID != "bridge-tenant" {
			t.Fatalf("unexpected response: %+v", body)
		}
	})

	t.Run("unapproved exposure stays hidden", func(t *testing.T) {
		response := bridgeCall(t, app, token, wasmHiddenRef.Key(), map[string]any{"amount": 9})
		defer response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("unapproved capability returned %d, want 404", response.StatusCode)
		}
	})

	t.Run("gRPC preserves contract and tenant", func(t *testing.T) {
		client := grpcBridgeClient(t, app)
		payload, err := json.Marshal(map[string]any{"amount": 7})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		response, err := client.Call(authed(t.Context(), token), &kernelV1.CallRequest{
			Capability: wasmPublicRef.Key(),
			Payload:    payload,
		})
		if err != nil {
			t.Fatalf("gRPC call: %v", err)
		}
		var body struct {
			Doubled  int    `json:"doubled"`
			TenantID string `json:"tenant_id"`
		}
		if err := json.Unmarshal(response.GetPayload(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.Doubled != 14 || body.TenantID != "bridge-tenant" {
			t.Fatalf("unexpected response: %+v", body)
		}
	})
}
