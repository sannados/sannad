package bootstrap_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sannados/sannad/internal/app/bootstrap"
	"github.com/sannados/sannad/pkg/config"
)

func TestNew_StartShutdown(t *testing.T) {
	cfg := config.Config{
		Env:           "development",
		KernelAddr:    ":0",
		GatewayAddr:   ":0",
		DatabaseDSN:   ":memory:",
		SessionSecret: "bootstrap-test-session-secret-long-enough",
		KayanIssuer:   "http://localhost:8080",
		CORSOrigins:   "*",
	}

	app, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}

	// Start servers so Shutdown can cleanly stop them.
	go func() { _ = app.RunKernel(ctx) }()
	go func() { _ = app.RunGateway(ctx) }()

	// Give servers a moment to bind before shutting down.
	time.Sleep(100 * time.Millisecond)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("app.Shutdown: %v", err)
	}
}

// TestShutdownWithoutStart covers cleanup after a wiring failure between New
// and Start. Servers are built in Start, so Shutdown must tolerate their
// absence — and must still release the database pool, which is the whole
// reason to call it on that path.
func TestShutdownWithoutStart(t *testing.T) {
	cfg := config.Config{
		Env:           "test",
		DatabaseDSN:   filepath.Join(t.TempDir(), "shutdown-test.db"),
		SessionSecret: "shutdown-test-session-secret-long-enough",
		KayanIssuer:   "http://localhost:8080",
	}
	app, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown without Start: %v", err)
	}
}
