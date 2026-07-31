// Command yandex-bridge exposes Yandex smart home devices to Apple HomeKit.
//
// It is designed to run unattended in Docker on a Raspberry Pi, which shapes
// most of the decisions in here: nothing fatal on a transient failure, nothing
// silent on a permanent one, and every piece of state that HomeKit depends on
// written to disk before it is used.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/brutella/hap"

	"yandex-bridge/internal/auth"
	"yandex-bridge/internal/bridge"
	"yandex-bridge/internal/config"
	"yandex-bridge/internal/logging"
	"yandex-bridge/internal/status"
	"yandex-bridge/internal/yandex"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if configuration failed.
		fmt.Fprintf(os.Stderr, "yandex-bridge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger, err := logging.SetupStderr(cfg.LogLevel)
	if err != nil {
		return err
	}
	logger.Info("starting yandex-bridge",
		slog.String("version", bridge.Version),
		slog.String("data_dir", cfg.DataDir),
		slog.Duration("poll_interval", cfg.PollInterval.Std()))

	// SIGINT and SIGTERM stop everything; docker stop sends SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}

	tokens, err := newTokenSource(cfg, logger)
	if err != nil {
		return err
	}

	api := yandex.New(tokens.HTTPClient(),
		yandex.WithLogger(logger),
		yandex.WithRefresher(tokens))

	registry, err := bridge.LoadRegistry(cfg.AIDPath())
	if err != nil {
		// A corrupt registry is refused rather than rebuilt: renumbering every
		// accessory would cost the user their rooms and automations.
		return fmt.Errorf("%w\n\nDelete %s only if you accept that every accessory "+
			"will be re-added to HomeKit as a new device", err, cfg.AIDPath())
	}
	logger.Info("loaded accessory id registry",
		slog.String("path", cfg.AIDPath()),
		slog.Int("known_devices", registry.Len()))

	syncer := bridge.NewSyncer(api, cfg, logger)
	health := bridge.NewHealth(cfg.HomeKit.Name+" Health", tokens, logger)
	syncer.SetObserver(health)

	reauth := make(chan struct{}, 1)
	health.SetReauthFunc(func() {
		select {
		case reauth <- struct{}{}:
		default:
		}
	})

	store := hap.NewFsStore(cfg.HAPStorePath())
	supervisor := bridge.NewSupervisor(cfg, api, registry, syncer, health, store, logger)
	statusSrv := status.NewServer(cfg.HealthAddr, health, tokens, syncer, logger)

	var wg sync.WaitGroup
	background := func(name string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
			logger.Debug("background task stopped", slog.String("task", name))
		}()
	}

	// The status server comes up before authorization, not after. On a first
	// run the bridge blocks waiting for someone to enter a device code, and
	// /healthz is where that code is visible without trawling the logs.
	background("status-server", func() {
		if err := statusSrv.Run(ctx); err != nil {
			// Losing the status endpoint is not worth taking the bridge down
			// for; HomeKit keeps working without it.
			logger.Error("status server stopped", slog.String("error", err.Error()))
		}
	})
	background("health", func() { health.Run(ctx) })

	// Block until the bridge is authorized. Publishing a HomeKit bridge full
	// of accessories that cannot do anything would be worse than waiting.
	if err := tokens.EnsureAuthorized(ctx); err != nil {
		// Whether this was a shutdown has to be decided before stop(), which
		// cancels the context itself and would make every failure look like
		// an orderly exit.
		shuttingDown := ctx.Err() != nil
		stop()
		wg.Wait()
		if shuttingDown {
			return nil
		}
		return fmt.Errorf("authorize with Yandex: %w", err)
	}

	background("syncer", func() { syncer.Run(ctx) })
	background("token-refresher", func() { tokens.RunRefresher(ctx) })
	background("reauth-watcher", func() { tokens.RunReauthWatcher(ctx, reauth) })

	err = supervisor.Run(ctx)

	stop()
	wg.Wait()

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("yandex-bridge stopped")
	return nil
}

// newTokenSource assembles the OAuth machinery.
func newTokenSource(cfg config.Config, logger *slog.Logger) (*auth.Source, error) {
	deviceID, err := auth.DeviceID(filepath.Join(cfg.DataDir, "device_id"))
	if err != nil {
		return nil, err
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "raspberry-pi"
	}

	flow := &auth.Flow{
		ClientID:     cfg.Yandex.ClientID,
		ClientSecret: cfg.Yandex.ClientSecret,
		DeviceID:     deviceID,
		DeviceName:   "yandex-bridge on " + hostname,
		Logger:       logger,
	}

	return auth.NewSource(auth.Config{
		ClientID:     cfg.Yandex.ClientID,
		ClientSecret: cfg.Yandex.ClientSecret,
		Store:        auth.NewStore(cfg.TokenPath()),
		Flow:         flow,
		Logger:       logger,
	})
}
