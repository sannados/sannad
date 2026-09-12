package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	sannad "github.com/sannados/sannad"
	crmV1 "github.com/sannados/sannad/contracts/gen/go/sannad/crm/v1"
	"github.com/sannados/sannad/examples/embedded/crm"
	"google.golang.org/grpc"
)

func main() {
	cfg := sannad.ConfigFromEnv()

	var logLevel slog.LevelVar
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	default:
		logLevel.Set(slog.LevelInfo)
	}
	opts := &slog.HandlerOptions{Level: &logLevel}
	if cfg.Env == "production" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, opts)))
	} else {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))
	}

	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	app, err := sannad.New(cfg)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}

	// Domain wiring lives here, not in bootstrap: the kernel names no module,
	// no model, and no gRPC service. Registering the scoped model is what
	// subjects Contact to tenant isolation.
	if err := app.RegisterScopedModels(&crm.Contact{}); err != nil {
		slog.Error("failed to register scoped models", "module", "crm", "error", err)
		os.Exit(1)
	}
	if err := app.RegisterModule(crm.New(app.Store(), cfg.Env).WithPublisher(app.Events())); err != nil {
		slog.Error("failed to register module", "module", "crm", "error", err)
		os.Exit(1)
	}
	app.RegisterAuthenticatedRoutes(crm.MountRoutes(app))
	app.RegisterGRPCService(func(srv *grpc.Server) {
		crmV1.RegisterContactReaderServer(srv, crm.NewContactReaderServer(app))
	})

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		slog.Error("failed to start modules", "error", err)
		os.Exit(1)
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.RunGateway(ctx)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-runErr:
		if err != nil {
			slog.Error("gateway server error", "error", err)
			os.Exit(1)
		}
	case sig := <-quit:
		slog.Info("received signal, shutting down", "signal", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown error", "error", err)
		}
	}
}
