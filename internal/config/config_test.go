package config

import (
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

func TestDefaultProductionSettings(t *testing.T) {
	cfg := Default()
	if cfg.Address == "" || cfg.DatabasePath == "" {
		t.Fatal("default address and database path must be configured")
	}
	if cfg.MaxRequestBodyBytes <= 0 || cfg.RequestTimeout <= 0 {
		t.Fatal("default request limits must be positive")
	}
	if cfg.ConnectTimeout <= 0 || cfg.TLSHandshakeTimeout <= 0 || cfg.ResponseHeaderTimeout <= 0 || cfg.IdleConnTimeout <= 0 {
		t.Fatal("default HTTP client timeouts must be positive")
	}
	if cfg.ReadHeaderTimeout <= 0 || cfg.ReadTimeout <= 0 || cfg.IdleTimeout <= 0 || cfg.ShutdownTimeout <= 0 {
		t.Fatal("default HTTP server lifecycle timeouts must be positive")
	}
	if cfg.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, want zero for long-lived SSE", cfg.WriteTimeout)
	}
	if cfg.Breaker.Mode != breaker.ModeCooldown || cfg.Breaker.Threshold <= 0 {
		t.Fatalf("unexpected breaker defaults: %#v", cfg.Breaker)
	}
}

func TestLoadOverridesProductionSettings(t *testing.T) {
	t.Setenv("FLUXGATE_ADDRESS", "127.0.0.1:9090")
	t.Setenv("FLUXGATE_DATABASE_PATH", "test.db")
	t.Setenv("FLUXGATE_MAX_BODY_BYTES", "4096")
	t.Setenv("FLUXGATE_REQUEST_TIMEOUT", "45s")
	t.Setenv("FLUXGATE_CONNECT_TIMEOUT", "2s")
	t.Setenv("FLUXGATE_TLS_HANDSHAKE_TIMEOUT", "3s")
	t.Setenv("FLUXGATE_RESPONSE_HEADER_TIMEOUT", "4s")
	t.Setenv("FLUXGATE_IDLE_CONN_TIMEOUT", "5s")
	t.Setenv("FLUXGATE_READ_HEADER_TIMEOUT", "6s")
	t.Setenv("FLUXGATE_READ_TIMEOUT", "7s")
	t.Setenv("FLUXGATE_WRITE_TIMEOUT", "0s")
	t.Setenv("FLUXGATE_IDLE_TIMEOUT", "8s")
	t.Setenv("FLUXGATE_SHUTDOWN_TIMEOUT", "9s")
	t.Setenv("FLUXGATE_MAX_ATTEMPTS", "5")
	t.Setenv("FLUXGATE_MAX_ATTEMPTS_PER_CHANNEL", "2")
	t.Setenv("FLUXGATE_RETRY_BASE_BACKOFF", "10ms")
	t.Setenv("FLUXGATE_RETRY_MAX_BACKOFF", "100ms")
	t.Setenv("FLUXGATE_BREAKER_MODE", "key_model_cooldown")
	t.Setenv("FLUXGATE_BREAKER_THRESHOLD", "4")
	t.Setenv("FLUXGATE_BREAKER_BASE_COOLDOWN", "20s")
	t.Setenv("FLUXGATE_BREAKER_MAX_COOLDOWN", "2m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Address != "127.0.0.1:9090" || cfg.DatabasePath != "test.db" || cfg.MaxRequestBodyBytes != 4096 {
		t.Fatalf("basic overrides not loaded: %#v", cfg)
	}
	if cfg.RequestTimeout != 45*time.Second || cfg.ConnectTimeout != 2*time.Second || cfg.TLSHandshakeTimeout != 3*time.Second || cfg.ResponseHeaderTimeout != 4*time.Second {
		t.Fatalf("client timeout overrides not loaded: %#v", cfg)
	}
	if cfg.ReadHeaderTimeout != 6*time.Second || cfg.ReadTimeout != 7*time.Second || cfg.IdleTimeout != 8*time.Second || cfg.ShutdownTimeout != 9*time.Second {
		t.Fatalf("server timeout overrides not loaded: %#v", cfg)
	}
	if cfg.Retry.MaxAttempts != 5 || cfg.Retry.MaxAttemptsPerChannel != 2 || cfg.Retry.BaseBackoff != 10*time.Millisecond || cfg.Retry.MaxBackoff != 100*time.Millisecond {
		t.Fatalf("retry overrides not loaded: %#v", cfg.Retry)
	}
	if cfg.Breaker.Mode != breaker.ModeKeyModelCooldown || cfg.Breaker.Threshold != 4 || cfg.Breaker.BaseCooldown != 20*time.Second || cfg.Breaker.MaxCooldown != 2*time.Minute {
		t.Fatalf("breaker overrides not loaded: %#v", cfg.Breaker)
	}
}

func TestLoadRejectsInvalidConfigurationWithoutEchoingValue(t *testing.T) {
	secretLikeValue := "credential-should-not-appear"
	t.Setenv("FLUXGATE_REQUEST_TIMEOUT", secretLikeValue)
	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want invalid duration error")
	}
	if strings.Contains(err.Error(), secretLikeValue) {
		t.Fatalf("configuration error leaked input value: %v", err)
	}
}

func TestLoadRejectsInconsistentBounds(t *testing.T) {
	t.Setenv("FLUXGATE_MAX_ATTEMPTS", "2")
	t.Setenv("FLUXGATE_MAX_ATTEMPTS_PER_CHANNEL", "3")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want attempt-bound validation error")
	}

	t.Setenv("FLUXGATE_MAX_ATTEMPTS", "3")
	t.Setenv("FLUXGATE_MAX_ATTEMPTS_PER_CHANNEL", "2")
	t.Setenv("FLUXGATE_BREAKER_BASE_COOLDOWN", "2m")
	t.Setenv("FLUXGATE_BREAKER_MAX_COOLDOWN", "1m")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want cooldown-bound validation error")
	}
}
