package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yhw5231/fluxgate/internal/admin"
	"github.com/yhw5231/fluxgate/internal/api"
	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/config"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/policy"
	"github.com/yhw5231/fluxgate/internal/probe"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/proxypool"
	"github.com/yhw5231/fluxgate/internal/router"
	"github.com/yhw5231/fluxgate/internal/store"
	"github.com/yhw5231/fluxgate/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// The admin subcommand manages the console credential and exits without
	// starting the gateway.
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := runAdmin(context.Background(), os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(context.Background(), logger); err != nil {
		logger.Error("gateway_stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(parent context.Context, logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	persistentStore, err := store.OpenSQLite(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer persistentStore.Close()

	if cfg.IntegrityCheck {
		if err := persistentStore.VerifyIntegrity(parent); err != nil {
			return err
		}
	}

	if err := persistentStore.EnsureBreakerSchema(parent); err != nil {
		return err
	}

	if err := persistentStore.EnsureAdminSchema(parent); err != nil {
		return err
	}

	// The record of served requests is what an operator reads after a failure, so
	// it is created at startup rather than on the first write: the console's view
	// of it works from the first request on.
	if err := persistentStore.EnsureRequestLogSchema(parent); err != nil {
		return err
	}

	// Where each generation job was created is what lets a request naming only a
	// job identifier reach the upstream holding it, so the record is created at
	// startup too.
	if err := persistentStore.EnsureMediaJobSchema(parent); err != nil {
		return err
	}

	// The token accounting is written on every served request, so its table and
	// indexes are created before the first one arrives rather than on the first
	// write.
	if err := persistentStore.EnsureUsageSchema(parent); err != nil {
		return err
	}

	// The record of what the account health probes found, created before the
	// startup sweep writes its first result.
	if err := persistentStore.EnsureProbeSchema(parent); err != nil {
		return err
	}

	// A fresh deployment gets a built-in administrator so the console is
	// reachable without a CLI step. The account is flagged as needing a password
	// change, and the data endpoints refuse it until that happens, so the
	// well-known password only grants the ability to set a real one.
	if created, err := persistentStore.EnsureDefaultAdminAccount(parent); err != nil {
		return err
	} else if created {
		logger.Warn("default_admin_account_created",
			"username", admin.DefaultUsername,
			"detail", "sign in and change the password immediately; data endpoints stay blocked until then",
		)
	}

	if err := persistentStore.EnsureUpstreamSchema(parent); err != nil {
		return err
	}

	configuration, err := persistentStore.LoadConfiguration(parent)
	if err != nil {
		return err
	}

	breakerStore, err := store.NewBreakerAdapter(parent, persistentStore)
	if err != nil {
		return err
	}
	circuitBreaker := &breaker.Breaker{
		Policy: cfg.Breaker,
		Store:  breakerStore,
	}

	applyRequestTimeout(&configuration, cfg.RequestTimeout)

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.DialContext = (&net.Dialer{
		Timeout:   cfg.ConnectTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	baseTransport.TLSHandshakeTimeout = cfg.TLSHandshakeTimeout
	baseTransport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	baseTransport.IdleConnTimeout = cfg.IdleConnTimeout

	transportPool := proxy.NewTransportPool(baseTransport)
	// A generation request is answered only when the upstream has produced the
	// artifact, which is minutes for a video and can easily be longer than the
	// response-header timeout a chat completion is held to. The media pool carries
	// the same connection settings with that one bound lifted: what a generation is
	// bounded by is the gateway's media timeout, applied per request, and a
	// response-header timeout underneath it would cut the request off first.
	mediaTransport := baseTransport.Clone()
	mediaTransport.ResponseHeaderTimeout = 0
	mediaTransportPool := proxy.NewTransportPool(mediaTransport)
	// The selector and the resolver are updated in place by a console write
	// rather than replaced, so a request already in flight finishes with the
	// configuration it started with.
	selector := router.NewMemorySelectorWithFilter(configuration.Routes, circuitBreaker)
	proxyResolver := buildProxyResolver(configuration)
	// A pool hands each account its own exit address. The lease is acquired on the
	// first request through that account and remembered, so the address an upstream
	// sees does not change with the gateway's uptime.
	poolManager := proxypool.NewManager(persistentStore)
	poolBindings := newPoolBindings()
	poolBindings.Set(configuration)
	proxyResolver.PoolURL = func(credential string) (string, error) {
		binding, bound := poolBindings.Lookup(credential)
		if !bound {
			return "", nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		address, err := poolManager.Acquire(ctx, binding.Pool, binding.SiteID, binding.OwnerKey)
		if err != nil {
			// A pool that cannot hand out a lease is reported rather than silently
			// bypassed: leaving through the gateway's own address instead would send
			// the account out from the address the pool exists to avoid.
			logger.Warn("proxy_pool_lease_failed",
				"pool", binding.Pool.Name, "site_id", binding.SiteID, "error", err.Error())
			return "", err
		}
		return address, nil
	}
	// The account health schedule reads the same lines the gateway routes with. A
	// console write replaces the snapshot it reads, so a probe follows the
	// configuration rather than a copy taken at startup.
	lines := &lineSource{}
	lines.Set(configuration)
	probeService := probe.NewService()
	probeService.Prober = &probe.Prober{
		Resolver: proxyResolver,
		Pool:     transportPool,
		Timeout:  cfg.ProbeTimeout,
	}
	probeService.Lines = lines.Lines
	probeService.Circuits = breakerStore.Snapshot
	probeService.Store = persistentStore
	probeService.Logger = logger
	// applyProbeSettings installs the probe policy in force. It is called at
	// startup and after every console write, so an interval or a concurrency
	// change applies to the next scan.
	applyProbeSettings := func(applied policy.Policy) {
		probeService.SetSettings(probe.Settings{
			Startup:     applied.Probe.Startup,
			Idle:        time.Duration(applied.Probe.IdleSeconds) * time.Second,
			Concurrency: applied.Probe.Concurrency,
			Keep:        applied.Probe.Keep,
			BreakerMode: applied.Breaker.Mode,
		})
	}
	// Attempts feed the idle clock: a line that has just served a request is not
	// one to spend a probe on.
	observer := observingBreaker{
		breaker: circuitBreaker,
		note: func(attempt domain.Attempt) {
			probeService.NoteCall(attempt.ChannelID, attempt.Model)
		},
	}
	engine := &proxy.Engine{
		Selector:           selector,
		Observer:           observer,
		Client:             &http.Client{Transport: baseTransport},
		MediaClient:        &http.Client{Transport: mediaTransport},
		ProxyResolver:      proxyResolver,
		TransportPool:      transportPool,
		MediaTransportPool: mediaTransportPool,
		StreamIdleTimeout:  cfg.StreamIdleTimeout,
		Policy:             cfg.Retry,
		Failover:           &cfg.Failover,
	}
	// The runtime policy is the environment's settings with whatever the console
	// stored on top of them. It is installed once here and again after every
	// console write, so a retry, failover, or cooldown change applies to the next
	// request rather than at the next restart.
	runtime, warnings := policy.FromSettings(configuration.Settings, cfg.Policy())
	for _, warning := range warnings {
		logger.Warn("policy_setting_ignored", "detail", warning)
	}
	engine.SetPolicy(proxy.DispatchPolicy{Retry: runtime.Retry, Failover: runtime.Failover})
	circuitBreaker.SetPolicy(runtime.Breaker)
	applyProbeSettings(runtime)

	gatewayAPI := &api.Server{
		Engine:              engine,
		Authenticator:       persistentStore,
		ManagementToken:     cfg.ManagementToken,
		Sessions:            persistentStore,
		ConfigStore:         persistentStore,
		BreakerSnapshotter:  breakerStore,
		BreakerReset:        breakerStore,
		RequestLog:          persistentStore,
		Usage:               persistentStore,
		Probes:              probeService,
		PinStore:            persistentStore,
		Pools:               poolManager,
		MediaJobs:           persistentStore,
		PolicyDefaults:      cfg.Policy(),
		MaxRequestBodyBytes: cfg.MaxRequestBodyBytes,
		MediaRequestTimeout: cfg.MediaRequestTimeout,
		StreamKeepAlive:     cfg.StreamKeepAlive,
		Logger:              logger,
		StartedAt:           time.Now().UTC(),
	}
	gatewayAPI.SetConfiguration(configuration)
	// A configuration written from the console is reloaded and installed here, so
	// an added channel or a changed weight serves traffic immediately instead of
	// at the next restart.
	gatewayAPI.Applier = func(updated store.Configuration) {
		applyRequestTimeout(&updated, cfg.RequestTimeout)
		selector.SetRoutes(updated.Routes)
		proxyResolver.SetConfig(proxyConfiguration(updated))
		lines.Set(updated)
		poolBindings.Set(updated)
		applied, ignored := policy.FromSettings(updated.Settings, cfg.Policy())
		for _, warning := range ignored {
			logger.Warn("policy_setting_ignored", "detail", warning)
		}
		engine.SetPolicy(proxy.DispatchPolicy{Retry: applied.Retry, Failover: applied.Failover})
		circuitBreaker.SetPolicy(applied.Breaker)
		applyProbeSettings(applied)
	}
	gatewayAPI.Ready.Store(true)
	// The schedule starts with the gateway: its startup sweep is what says which
	// credentials still work after a restart, and its scan is what keeps checking
	// the lines nobody has used.
	probeService.Start(parent)
	defer probeService.Stop()

	httpServer := &http.Server{
		Addr:              cfg.Address,
		Handler:           gatewayAPI.Handler(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	signalContext, stopSignals := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	serveErrors := make(chan error, 1)
	go func() {
		logger.Info("gateway_started",
			"version", version.Short(),
			"address", cfg.Address,
			"model_count", len(configuration.Models),
			"channel_count", len(configuration.Channels),
		)
		serveErrors <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalContext.Done():
		gatewayAPI.Ready.Store(false)
		logger.Info("gateway_shutdown_started")

		shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			_ = httpServer.Close()
			return err
		}

		if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		logger.Info("gateway_shutdown_complete")
		return nil
	}
}

// applyRequestTimeout fills in the configured request timeout for every channel
// that does not carry one of its own.
//
// Both views of the channels are updated: the flat list the management snapshot
// reports, and the per-route lists the selector actually hands to the engine.
// They are independent copies of the same rows, so filling in only one of them
// would leave the request path without the configured timeout.
func applyRequestTimeout(configuration *store.Configuration, fallback time.Duration) {
	for index := range configuration.Routes {
		for channelIndex := range configuration.Routes[index].Channels {
			channel := &configuration.Routes[index].Channels[channelIndex]
			if channel.RequestTimeout <= 0 {
				channel.RequestTimeout = fallback
			}
		}
	}
	for index := range configuration.Channels {
		if configuration.Channels[index].RequestTimeout <= 0 {
			configuration.Channels[index].RequestTimeout = fallback
		}
	}
}

func buildProxyResolver(configuration store.Configuration) *proxy.Resolver {
	return &proxy.Resolver{
		Config:      proxyConfiguration(configuration),
		SystemProxy: http.ProxyFromEnvironment,
	}
}

// proxyConfiguration derives the proxy decisions from a configuration snapshot.
// The site entry is keyed by host because that is what a request knows, and the
// key entry by credential because that is what identifies the upstream account.
func proxyConfiguration(configuration store.Configuration) proxy.ProxyConfig {
	proxyConfig := proxy.ProxyConfig{
		Sites: make(map[string]string),
		Keys:  make(map[string]string),
	}
	for _, profile := range configuration.ProxyProfiles {
		if profile.Enabled && profile.IsDefault {
			proxyConfig.Default = profile.URL
			break
		}
	}
	for _, channel := range configuration.Channels {
		if targetURL, err := url.Parse(channel.BaseURL); err == nil {
			host := strings.ToLower(targetURL.Hostname())
			if host != "" {
				switch {
				case channel.SiteProxyURL != "":
					proxyConfig.Sites[host] = channel.SiteProxyURL
				case channel.SiteUseSystemProxy:
					proxyConfig.Sites[host] = "system"
				}
			}
		}
		switch {
		case channel.ProxyURL != "":
			proxyConfig.Keys[channel.APIKey] = channel.ProxyURL
		case channel.UseSystemProxy:
			proxyConfig.Keys[channel.APIKey] = "system"
		}
	}
	return proxyConfig
}
