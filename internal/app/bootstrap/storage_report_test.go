package bootstrap_test

import (
	"path/filepath"
	"testing"

	"github.com/sannados/sannad/examples/embedded/announcements"
	"github.com/sannados/sannad/internal/app/bootstrap"
	"github.com/sannados/sannad/pkg/config"
	"github.com/sannados/sannad/pkg/modulekit"
)

// The operator-visible outcome: a module keeping tenant data outside the kernel
// store must be named at startup, because the audit cannot see it and an
// operator reading a clean audit would otherwise assume it had been checked.
func TestSelfManagedModulesAreReported(t *testing.T) {
	cfg := config.Config{
		Env: "development", KernelAddr: ":0", GatewayAddr: ":0",
		DatabaseDSN:   filepath.Join(t.TempDir(), "e2e.db"),
		SessionSecret: "e2e-test-session-secret-long-enough",
		KayanIssuer:   "http://localhost:8080", CORSOrigins: "*",
	}
	app, err := bootstrap.New(cfg)
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	if err := app.RegisterModule(announcements.New()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(t.Context()) })

	reported := app.Registry.SelfManagedStorage()
	if len(reported) != 1 || reported[0] != "com.community.announcements" {
		t.Fatalf("self-managed modules = %v, want [com.community.announcements]", reported)
	}

	// And a kernel-storage module must not appear, or the warning becomes noise
	// and an operator learns to skip it.
	if announcements.New().Descriptor().Storage != modulekit.StorageOwn {
		t.Fatal("fixture no longer declares StorageOwn")
	}
}
