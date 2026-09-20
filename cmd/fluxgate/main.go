package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yhw5231/fluxgate/internal/api"
	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/config"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/router"
	"github.com/yhw5231/fluxgate/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
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

	for index := range configuration.Channels {
		if configuration.Channels[index].RequestTimeout <= 0 {
			configuration.Channels[index].RequestTimeout = cfg.RequestTimeout
		}
	}

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.DialContext = (&net.Dialer{
		Timeout:   cfg.ConnectTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	baseTransport.TLSHandshakeTimeout = cfg.TLSHandshakeTimeout
	baseTransport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	baseTransport.IdleConnTimeout = cfg.IdleConnTimeout

	transportPool := proxy.NewTransportPool(baseTransport)
	engine := &proxy.Engine{
		Selector:      router.NewMemorySelectorWithFilter(configuration.Routes, circuitBreaker),
		Observer:      circuitBreaker,
		Client:        &http.Client{Transport: baseTransport},
		ProxyResolver: buildProxyResolver(configuration),
		TransportPool: transportPool,
		Policy:        cfg.Retry,
	}

	gatewayAPI := &api.Server{
		Engine:              engine,
		Authenticator:       persistentStore,
		ManagementToken:     cfg.ManagementToken,
		Configuration:       configuration,
		Routes:              configuration.Routes,
		BreakerSnapshotter:  breakerStore,
		Models:              configuration.Models,
		MaxRequestBodyBytes: cfg.MaxRequestBodyBytes,
		Logger:              logger,
		StartedAt:           time.Now().UTC(),
	}
	gatewayAPI.Ready.Store(true)

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

func buildProxyResolver(configuration store.Configuration) *proxy.Resolver {
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
	return &proxy.Resolver{
		Config:      proxyConfig,
		SystemProxy: http.ProxyFromEnvironment,
	}
}
