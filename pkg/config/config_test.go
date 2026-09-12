package config_test

import (
	"strings"
	"testing"

	"github.com/sannados/sannad/pkg/config"
)

// validConfig returns a Config that passes Validate(), for tests to mutate
// one field at a time. Copied rather than shared, since Config is a value
// type and each test owns its own.
func validConfig() config.Config {
	return config.Config{
		Env:           "development",
		SessionSecret: "a-session-secret-at-least-32-characters-long",
		DatabaseDSN:   "./sannad.db",
		CORSOrigins:   "https://app.example.com",
	}
}

func TestValidateRejectsAShortSessionSecret(t *testing.T) {
	cfg := validConfig()
	cfg.SessionSecret = "too-short"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for a session secret under 32 characters")
	}
}

func TestValidateRejectsTheDefaultPlaceholderSecret(t *testing.T) {
	cfg := validConfig()
	cfg.SessionSecret = "change-me-in-dev"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for the default placeholder secret")
	}
}

func TestValidateAcceptsAGoodSessionSecret(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("expected a valid config to pass, got %v", err)
	}
}

func TestValidateRejectsASQLiteDSNInProduction(t *testing.T) {
	cfg := validConfig()
	cfg.Env = "production"
	cfg.DatabaseDSN = "./sannad.db"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for SQLite in production")
	}
}

func TestValidateAcceptsAPostgresDSNInProduction(t *testing.T) {
	cfg := validConfig()
	cfg.Env = "production"
	cfg.DatabaseDSN = "postgres://user:pass@host/db"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected a postgres DSN to pass in production, got %v", err)
	}
}

func TestValidateRejectsWildcardCORSInProduction(t *testing.T) {
	cfg := validConfig()
	cfg.Env = "production"
	cfg.DatabaseDSN = "postgres://user:pass@host/db"
	cfg.CORSOrigins = "*"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for wildcard CORS in production")
	}
}

func TestValidateAllowsWildcardCORSOutsideProduction(t *testing.T) {
	cfg := validConfig()
	cfg.Env = "development"
	cfg.CORSOrigins = "*"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("wildcard CORS should be fine outside production, got %v", err)
	}
}

// TestValidateRejectsAShortPreviousSecret: a retiring secret is still a
// signing key until its last token expires, and a weak one is exactly as
// dangerous as a weak active one.
func TestValidateRejectsAShortPreviousSecret(t *testing.T) {
	cfg := validConfig()
	cfg.SessionPreviousSecrets = []string{"too-short"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for a previous secret under 32 characters")
	}
}

func TestValidateAcceptsGoodPreviousSecrets(t *testing.T) {
	cfg := validConfig()
	cfg.SessionPreviousSecrets = []string{"a-retired-secret-at-least-32-characters"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid previous secrets to pass, got %v", err)
	}
}

func TestFromEnvAppliesDefaultsWhenUnset(t *testing.T) {
	cfg := config.FromEnv()
	if cfg.Env != "development" {
		t.Errorf("Env = %q, want development", cfg.Env)
	}
	if cfg.SessionSecret != "change-me-in-dev" {
		t.Errorf("SessionSecret = %q, want the dev placeholder", cfg.SessionSecret)
	}
	if cfg.SessionPreviousSecrets != nil {
		t.Errorf("SessionPreviousSecrets = %v, want nil when unset", cfg.SessionPreviousSecrets)
	}
	if cfg.RateLimit != 60 {
		t.Errorf("RateLimit = %d, want 60", cfg.RateLimit)
	}
}

func TestFromEnvParsesPreviousSecretsList(t *testing.T) {
	t.Setenv("SANNAD_SESSION_SECRET_PREVIOUS", "first-secret-value , second-secret-value,,")
	cfg := config.FromEnv()
	want := []string{"first-secret-value", "second-secret-value"}
	if len(cfg.SessionPreviousSecrets) != len(want) {
		t.Fatalf("SessionPreviousSecrets = %v, want %v", cfg.SessionPreviousSecrets, want)
	}
	for i, v := range want {
		if cfg.SessionPreviousSecrets[i] != v {
			t.Errorf("SessionPreviousSecrets[%d] = %q, want %q", i, cfg.SessionPreviousSecrets[i], v)
		}
	}
}

func TestFromEnvOverridesDefaults(t *testing.T) {
	t.Setenv("SANNAD_ENV", "production")
	t.Setenv("SANNAD_RATE_LIMIT", "120")
	cfg := config.FromEnv()
	if cfg.Env != "production" {
		t.Errorf("Env = %q, want production", cfg.Env)
	}
	if cfg.RateLimit != 120 {
		t.Errorf("RateLimit = %d, want 120", cfg.RateLimit)
	}
}

// TestFromEnvIgnoresAnUnparsableInt: a malformed int env var falls back to
// the default rather than zeroing out the setting or panicking.
func TestFromEnvIgnoresAnUnparsableInt(t *testing.T) {
	t.Setenv("SANNAD_RATE_LIMIT", "not-a-number")
	cfg := config.FromEnv()
	if cfg.RateLimit != 60 {
		t.Errorf("RateLimit = %d, want the default 60 when the env var is unparsable", cfg.RateLimit)
	}
}

func TestFromEnvParsesBoolVariants(t *testing.T) {
	cases := map[string]bool{"true": true, "1": true, "yes": true, "on": true,
		"false": false, "0": false, "no": false, "off": false}
	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			t.Setenv("SANNAD_TENANT_SELF_PROVISION", value)
			cfg := config.FromEnv()
			if cfg.TenantSelfProvision != want {
				t.Errorf("TenantSelfProvision with env %q = %v, want %v", value, cfg.TenantSelfProvision, want)
			}
		})
	}
}

func TestFromEnvBoolFallsBackOnGarbageValue(t *testing.T) {
	t.Setenv("SANNAD_TENANT_SELF_PROVISION", "maybe")
	cfg := config.FromEnv()
	if cfg.TenantSelfProvision != false {
		t.Errorf("TenantSelfProvision = %v, want the default false for an unrecognized value", cfg.TenantSelfProvision)
	}
}

// isPostgresDSN is unexported; this exercises it only through Validate's
// production-DSN check, across the shapes it is documented to accept.
func TestValidateAcceptsEveryDocumentedPostgresDSNShape(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user:pass@host/db",
		"postgresql://user:pass@host/db",
		"host=localhost user=sannad dbname=sannad",
	} {
		t.Run(dsn, func(t *testing.T) {
			cfg := validConfig()
			cfg.Env = "production"
			cfg.DatabaseDSN = dsn
			if err := cfg.Validate(); err != nil {
				t.Errorf("DSN %q should be accepted in production, got %v", dsn, err)
			}
		})
	}
}

func TestValidateErrorMessagesNameTheEnvVar(t *testing.T) {
	cfg := validConfig()
	cfg.SessionSecret = "short"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "SANNAD_SESSION_SECRET") {
		t.Fatalf("expected the error to name SANNAD_SESSION_SECRET, got %v", err)
	}
}
