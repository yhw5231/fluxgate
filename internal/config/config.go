package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/policy"
)

const (
	defaultAddress             = ":8081"
	defaultDatabasePath        = "../data/hub.db"
	defaultMaxRequestBodyBytes = int64(8 << 20)
	// defaultRequestTimeout bounds one upstream attempt end to end for a request
	// that is answered as a single body. It is a limit on how long a model may
	// take to answer, not a limit on how long the caller waits: a reasoning model
	// regularly needs several minutes, and a gateway that gave up at the sixty
	// seconds this used to be turned every long answer into a canceled request at
	// the upstream.
	defaultRequestTimeout = 5 * time.Minute
	// defaultStreamIdleTimeout bounds how long a streamed answer may go without a
	// byte from the upstream. A stream is bounded by silence rather than by total
	// duration, so a long answer is served while a stalled upstream is given up.
	defaultStreamIdleTimeout = 2 * time.Minute
	// defaultStreamKeepAlive is how often a silent event stream is reminded that it
	// is alive, which is what keeps a client or an intermediary proxy from hanging
	// up on a reasoning model that is thinking.
	defaultStreamKeepAlive     = 15 * time.Second
	defaultMediaRequestTimeout = 10 * time.Minute
	defaultConnectTimeout      = 10 * time.Second
	defaultTLSHandshakeTimeout = 10 * time.Second
	// defaultResponseHeaderTimeout is zero on purpose: the per-attempt request
	// timeout already bounds how long the upstream may take to start answering,
	// and a second, shorter bound underneath it cut requests off before that
	// timeout could apply — the upstream then saw a canceled request while the
	// gateway was still waiting as intended.
	defaultResponseHeaderTimeout = 0
	defaultIdleConnTimeout       = 90 * time.Second
	defaultReadHeaderTimeout     = 10 * time.Second
	defaultReadTimeout           = 30 * time.Second
	defaultWriteTimeout          = 0
	defaultIdleTimeout           = 120 * time.Second
	defaultShutdownTimeout       = 15 * time.Second
	defaultIntegrityCheck        = true
)

// Config contains process-level settings for the production gateway runtime.
type Config struct {
	Address             string
	DatabasePath        string
	ManagementToken     string
	MaxRequestBodyBytes int64
	RequestTimeout      time.Duration
	// StreamIdleTimeout bounds how long a streamed answer may be silent before the
	// gateway gives up on the upstream. A stream is not held to the request
	// timeout above, because a long answer would be cut off as if it had failed.
	StreamIdleTimeout time.Duration
	// StreamKeepAlive is the heartbeat written into a silent event stream, so the
	// caller and any proxy in front of the gateway do not decide the request has
	// stalled. Zero switches it off.
	StreamKeepAlive time.Duration
	// MediaRequestTimeout bounds one generation request — an image or a video —
	// whose upstream works longer than a chat completion takes to answer.
	MediaRequestTimeout   time.Duration
	ConnectTimeout        time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	ReadHeaderTimeout     time.Duration
	ReadTimeout           time.Duration
	WriteTimeout          time.Duration
	IdleTimeout           time.Duration
	ShutdownTimeout       time.Duration
	Retry                 domain.RetryPolicy
	Failover              domain.FailoverPolicy
	Breaker               breaker.Policy
	// RequestLog bounds the gateway's record of the requests it served.
	RequestLog policy.RequestLogPolicy
	// IntegrityCheck verifies the shared SQLite database at startup so a
	// damaged file fails loudly instead of serving wrong routing data.
	IntegrityCheck bool
}

// Policy is the runtime traffic policy these settings produce. A console write
// overrides individual values of it; the environment supplies the rest.
func (c Config) Policy() policy.Policy {
	return policy.Policy{Retry: c.Retry, Failover: c.Failover, Breaker: c.Breaker, RequestLog: c.RequestLog}
}

// Load reads gateway settings from environment variables and applies bounded,
// production-safe defaults. Configuration errors never include credential values.
func Load() (Config, error) {
	cfg := Default()

	if value := strings.TrimSpace(os.Getenv("FLUXGATE_ADDRESS")); value != "" {
		cfg.Address = value
	}
	if value := strings.TrimSpace(os.Getenv("FLUXGATE_DATABASE_PATH")); value != "" {
		cfg.DatabasePath = value
	}
	cfg.ManagementToken = strings.TrimSpace(os.Getenv("FLUXGATE_MANAGEMENT_TOKEN"))

	var err error
	if cfg.MaxRequestBodyBytes, err = int64Env("FLUXGATE_MAX_BODY_BYTES", cfg.MaxRequestBodyBytes, true); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = durationEnv("FLUXGATE_REQUEST_TIMEOUT", cfg.RequestTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.StreamIdleTimeout, err = durationEnv("FLUXGATE_STREAM_IDLE_TIMEOUT", cfg.StreamIdleTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.StreamKeepAlive, err = durationEnv("FLUXGATE_STREAM_KEEPALIVE", cfg.StreamKeepAlive, false); err != nil {
		return Config{}, err
	}
	if cfg.MediaRequestTimeout, err = durationEnv("FLUXGATE_MEDIA_REQUEST_TIMEOUT", cfg.MediaRequestTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.ConnectTimeout, err = durationEnv("FLUXGATE_CONNECT_TIMEOUT", cfg.ConnectTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.TLSHandshakeTimeout, err = durationEnv("FLUXGATE_TLS_HANDSHAKE_TIMEOUT", cfg.TLSHandshakeTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.ResponseHeaderTimeout, err = durationEnv("FLUXGATE_RESPONSE_HEADER_TIMEOUT", cfg.ResponseHeaderTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.IdleConnTimeout, err = durationEnv("FLUXGATE_IDLE_CONN_TIMEOUT", cfg.IdleConnTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.ReadHeaderTimeout, err = durationEnv("FLUXGATE_READ_HEADER_TIMEOUT", cfg.ReadHeaderTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.ReadTimeout, err = durationEnv("FLUXGATE_READ_TIMEOUT", cfg.ReadTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.WriteTimeout, err = durationEnv("FLUXGATE_WRITE_TIMEOUT", cfg.WriteTimeout, false); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = durationEnv("FLUXGATE_IDLE_TIMEOUT", cfg.IdleTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = durationEnv("FLUXGATE_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout, true); err != nil {
		return Config{}, err
	}
	if cfg.Retry.BaseBackoff, err = durationEnv("FLUXGATE_RETRY_BASE_BACKOFF", cfg.Retry.BaseBackoff, false); err != nil {
		return Config{}, err
	}
	if cfg.Retry.MaxBackoff, err = durationEnv("FLUXGATE_RETRY_MAX_BACKOFF", cfg.Retry.MaxBackoff, false); err != nil {
		return Config{}, err
	}
	if cfg.Breaker.BaseCooldown, err = durationEnv("FLUXGATE_BREAKER_BASE_COOLDOWN", cfg.Breaker.BaseCooldown, true); err != nil {
		return Config{}, err
	}
	if cfg.Breaker.MaxCooldown, err = durationEnv("FLUXGATE_BREAKER_MAX_COOLDOWN", cfg.Breaker.MaxCooldown, true); err != nil {
		return Config{}, err
	}

	if cfg.Retry.MaxAttempts, err = intEnv("FLUXGATE_MAX_ATTEMPTS", cfg.Retry.MaxAttempts, true); err != nil {
		return Config{}, err
	}
	if cfg.Retry.MaxAttemptsPerChannel, err = intEnv("FLUXGATE_MAX_ATTEMPTS_PER_CHANNEL", cfg.Retry.MaxAttemptsPerChannel, true); err != nil {
		return Config{}, err
	}
	if cfg.Breaker.Threshold, err = intEnv("FLUXGATE_BREAKER_THRESHOLD", cfg.Breaker.Threshold, true); err != nil {
		return Config{}, err
	}
	if cfg.Breaker.Multiplier, err = floatEnv("FLUXGATE_BREAKER_COOLDOWN_MULTIPLIER", cfg.Breaker.Multiplier, 1); err != nil {
		return Config{}, err
	}
	if cfg.Failover.Enabled, err = boolEnv("FLUXGATE_FAILOVER_ENABLED", cfg.Failover.Enabled); err != nil {
		return Config{}, err
	}
	if cfg.Failover.CrossUpstream, err = boolEnv("FLUXGATE_FAILOVER_CROSS_UPSTREAM", cfg.Failover.CrossUpstream); err != nil {
		return Config{}, err
	}
	if cfg.IntegrityCheck, err = boolEnv("FLUXGATE_INTEGRITY_CHECK", cfg.IntegrityCheck); err != nil {
		return Config{}, err
	}
	if cfg.RequestLog.Enabled, err = boolEnv("FLUXGATE_REQUEST_LOG_ENABLED", cfg.RequestLog.Enabled); err != nil {
		return Config{}, err
	}
	if cfg.RequestLog.Keep, err = intEnv("FLUXGATE_REQUEST_LOG_KEEP", cfg.RequestLog.Keep, true); err != nil {
		return Config{}, err
	}

	if value := strings.TrimSpace(os.Getenv("FLUXGATE_BREAKER_MODE")); value != "" {
		cfg.Breaker.Mode = breaker.Mode(value)
	}
	if !validBreakerMode(cfg.Breaker.Mode) {
		return Config{}, fmt.Errorf("FLUXGATE_BREAKER_MODE must be one of cooldown, disable, key_cooldown, or key_model_cooldown")
	}
	if cfg.Retry.MaxAttemptsPerChannel > cfg.Retry.MaxAttempts {
		return Config{}, errors.New("FLUXGATE_MAX_ATTEMPTS_PER_CHANNEL must not exceed FLUXGATE_MAX_ATTEMPTS")
	}
	// A generation outlives an answer: an upstream that produces an artifact
	// answers far later than one that answers a chat completion, so a media
	// timeout that is not longer than the request timeout would fail every image
	// and video before the request timeout could matter.
	if cfg.MediaRequestTimeout <= cfg.RequestTimeout {
		return Config{}, errors.New("FLUXGATE_MEDIA_REQUEST_TIMEOUT must be greater than FLUXGATE_REQUEST_TIMEOUT")
	}
	// A heartbeat that arrives after the stream has already been given up is of no
	// use to the caller, so it has to be shorter than the silence it is covering.
	if cfg.StreamKeepAlive > 0 && cfg.StreamKeepAlive >= cfg.StreamIdleTimeout {
		return Config{}, errors.New("FLUXGATE_STREAM_KEEPALIVE must be less than FLUXGATE_STREAM_IDLE_TIMEOUT")
	}
	if cfg.Retry.MaxBackoff < cfg.Retry.BaseBackoff {
		return Config{}, errors.New("FLUXGATE_RETRY_MAX_BACKOFF must not be less than FLUXGATE_RETRY_BASE_BACKOFF")
	}
	if cfg.Breaker.MaxCooldown < cfg.Breaker.BaseCooldown {
		return Config{}, errors.New("FLUXGATE_BREAKER_MAX_COOLDOWN must not be less than FLUXGATE_BREAKER_BASE_COOLDOWN")
	}
	if strings.TrimSpace(cfg.DatabasePath) == "" {
		return Config{}, errors.New("FLUXGATE_DATABASE_PATH must not be empty")
	}

	return cfg, nil
}

// Default returns bounded settings suitable for production startup. WriteTimeout
// is intentionally zero so long-lived server-sent event responses are not cut off.
func Default() Config {
	traffic := policy.Default()
	return Config{
		Address:               defaultAddress,
		DatabasePath:          defaultDatabasePath,
		MaxRequestBodyBytes:   defaultMaxRequestBodyBytes,
		RequestTimeout:        defaultRequestTimeout,
		StreamIdleTimeout:     defaultStreamIdleTimeout,
		StreamKeepAlive:       defaultStreamKeepAlive,
		MediaRequestTimeout:   defaultMediaRequestTimeout,
		ConnectTimeout:        defaultConnectTimeout,
		TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
		ResponseHeaderTimeout: defaultResponseHeaderTimeout,
		IdleConnTimeout:       defaultIdleConnTimeout,
		ReadHeaderTimeout:     defaultReadHeaderTimeout,
		ReadTimeout:           defaultReadTimeout,
		WriteTimeout:          defaultWriteTimeout,
		IdleTimeout:           defaultIdleTimeout,
		ShutdownTimeout:       defaultShutdownTimeout,
		Retry:                 traffic.Retry,
		Failover:              traffic.Failover,
		Breaker:               traffic.Breaker,
		RequestLog:            traffic.RequestLog,
		IntegrityCheck:        defaultIntegrityCheck,
	}
}

func boolEnv(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return parsed, nil
}

func durationEnv(name string, fallback time.Duration, positive bool) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || (positive && parsed <= 0) || (!positive && parsed < 0) {
		qualifier := "non-negative"
		if positive {
			qualifier = "positive"
		}
		return 0, fmt.Errorf("%s must be a %s duration", name, qualifier)
	}
	return parsed, nil
}

func intEnv(name string, fallback int, positive bool) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || (positive && parsed <= 0) || (!positive && parsed < 0) {
		qualifier := "non-negative"
		if positive {
			qualifier = "positive"
		}
		return 0, fmt.Errorf("%s must be a %s integer", name, qualifier)
	}
	return parsed, nil
}

func int64Env(name string, fallback int64, positive bool) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || (positive && parsed <= 0) || (!positive && parsed < 0) {
		qualifier := "non-negative"
		if positive {
			qualifier = "positive"
		}
		return 0, fmt.Errorf("%s must be a %s integer", name, qualifier)
	}
	return parsed, nil
}

func floatEnv(name string, fallback, minimum float64) (float64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < minimum {
		return 0, fmt.Errorf("%s must be a number of at least %g", name, minimum)
	}
	return parsed, nil
}

func validBreakerMode(mode breaker.Mode) bool {
	switch mode {
	case breaker.ModeCooldown, breaker.ModeDisable, breaker.ModeKeyCooldown, breaker.ModeKeyModelCooldown:
		return true
	default:
		return false
	}
}
