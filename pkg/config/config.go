// Package config defines the public runtime configuration for Sannad.
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env           string
	KernelAddr    string
	GatewayAddr   string
	DatabaseDSN   string
	SessionSecret string
	// SessionPreviousSecrets are prior session-signing secrets, still
	// accepted to verify a token issued before a rotation of SessionSecret
	// but never used to sign a new one. Keep an entry listed until the
	// longest-lived token that could have been signed with it (a refresh
	// token, issued up to 7 days before rotation) has had time to expire.
	SessionPreviousSecrets []string
	KayanIssuer            string
	CORSOrigins            string
	TenantBaseDomain       string
	TenantSelfProvision    bool
	NATSUrl                string
	GRPCAddr               string
	OIDCProvidersJSON      string
	SAMLEntityID           string
	SAMLACSUrl             string
	SAMLProvidersJSON      string
	OTELEndpoint           string
	RateLimit              int
	RateLimitPerTenant     int
	ReadTimeout            int
	WriteTimeout           int
	IdleTimeout            int
	BodyLimit              int
	LogLevel               string
	DBMaxOpenConns         int
	DBMaxIdleConns         int
	DBConnMaxLifetimeSecs  int
}

func FromEnv() Config {
	return Config{
		Env:                    envOr("SANNAD_ENV", "development"),
		KernelAddr:             envOr("SANNAD_KERNEL_ADDR", ":8081"),
		GatewayAddr:            envOr("SANNAD_GATEWAY_ADDR", ":8080"),
		DatabaseDSN:            envOr("SANNAD_DATABASE_DSN", "./sannad.db"),
		SessionSecret:          envOr("SANNAD_SESSION_SECRET", "change-me-in-dev"),
		SessionPreviousSecrets: envList("SANNAD_SESSION_SECRET_PREVIOUS"),
		KayanIssuer:            envOr("SANNAD_KAYAN_ISSUER", "http://localhost:8080"),
		CORSOrigins:            envOr("SANNAD_CORS_ORIGINS", "*"),
		TenantBaseDomain:       envOr("SANNAD_TENANT_BASE_DOMAIN", ""),
		TenantSelfProvision:    envBool("SANNAD_TENANT_SELF_PROVISION", false),
		NATSUrl:                envOr("SANNAD_NATS_URL", ""),
		GRPCAddr:               envOr("SANNAD_GRPC_ADDR", ":9090"),
		OIDCProvidersJSON:      envOr("SANNAD_OIDC_PROVIDERS_JSON", ""),
		SAMLEntityID:           envOr("SANNAD_SAML_ENTITY_ID", ""),
		SAMLACSUrl:             envOr("SANNAD_SAML_ACS_URL", ""),
		SAMLProvidersJSON:      envOr("SANNAD_SAML_PROVIDERS_JSON", ""),
		OTELEndpoint:           envOr("SANNAD_OTEL_ENDPOINT", ""),
		RateLimit:              envInt("SANNAD_RATE_LIMIT", 60),
		RateLimitPerTenant:     envInt("SANNAD_RATE_LIMIT_PER_TENANT", 300),
		ReadTimeout:            envInt("SANNAD_READ_TIMEOUT", 15),
		WriteTimeout:           envInt("SANNAD_WRITE_TIMEOUT", 30),
		IdleTimeout:            envInt("SANNAD_IDLE_TIMEOUT", 65),
		BodyLimit:              envInt("SANNAD_BODY_LIMIT", 4194304),
		LogLevel:               envOr("SANNAD_LOG_LEVEL", "info"),
		DBMaxOpenConns:         envInt("SANNAD_DB_MAX_OPEN_CONNS", 25),
		DBMaxIdleConns:         envInt("SANNAD_DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLifetimeSecs:  envInt("SANNAD_DB_CONN_MAX_LIFETIME", 300),
	}
}

// Validate rejects configurations that are unsafe or unsupported in production.
func (cfg Config) Validate() error {
	if len(cfg.SessionSecret) < 32 {
		return errors.New("config: SANNAD_SESSION_SECRET must be at least 32 characters")
	}
	if cfg.SessionSecret == "change-me-in-dev" {
		return errors.New("config: SANNAD_SESSION_SECRET must not be the default placeholder value")
	}
	for _, secret := range cfg.SessionPreviousSecrets {
		if len(secret) < 32 {
			return errors.New("config: every entry in SANNAD_SESSION_SECRET_PREVIOUS must be at least 32 characters")
		}
	}
	if cfg.Env == "production" && !isPostgresDSN(cfg.DatabaseDSN) {
		return errors.New("config: SQLite is not supported in production; set SANNAD_DATABASE_DSN to a PostgreSQL DSN")
	}
	if cfg.Env == "production" && cfg.CORSOrigins == "*" {
		return errors.New("config: SANNAD_CORS_ORIGINS must not be wildcard (*) in production; set explicit allowed origins")
	}
	return nil
}

func isPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") ||
		strings.HasPrefix(dsn, "postgresql://") ||
		strings.HasPrefix(dsn, "host=")
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return fallback
}

// envList splits a comma-separated env var into its non-empty, trimmed
// parts. Absent or empty returns nil, so the zero value of
// SessionPreviousSecrets means "nothing to rotate away from" rather than a
// slice holding one empty string.
func envList(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envBool(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
