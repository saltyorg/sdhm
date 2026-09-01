package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/saltyorg/sdhm/command"
	"github.com/saltyorg/sdhm/daemon"
	"github.com/saltyorg/sdhm/docker"
	"github.com/saltyorg/sdhm/health"
	"github.com/saltyorg/sdhm/hosts"
)

type daemonRunner interface {
	Run(context.Context) error
}

type hostsRegenerator interface {
	Regenerate(context.Context) error
}

type daemonWiring struct {
	newDocker       func() (daemon.NetworkSource, error)
	newStore        func(string, string, string, string) daemon.HostStore
	newTracker      func() *health.Tracker
	newHandler      func(*health.Tracker) http.Handler
	newHealthServer func(string, http.Handler) daemon.HealthServer
	newDaemon       func(daemon.Config, daemon.NetworkSource, daemon.HostStore, *health.Tracker, daemon.HealthServer, *slog.Logger) (daemonRunner, error)
}

func runDaemon(ctx context.Context, cfg command.Config) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if os.Geteuid() != 0 {
		logger.Warn("SDHM should run as root to modify the hosts file")
	}
	logger.Info(
		"starting SDHM",
		"version", version,
		"networks", cfg.Networks,
		"default_network", cfg.DefaultNetwork,
	)
	return runDaemonWith(ctx, cfg, logger, productionDaemonWiring())
}

func productionDaemonWiring() daemonWiring {
	return daemonWiring{
		newDocker: func() (daemon.NetworkSource, error) {
			return docker.New()
		},
		newStore: func(hostsPath, backupPath, sectionName, defaultNetwork string) daemon.HostStore {
			return hosts.NewStore(hostsPath, backupPath, sectionName, defaultNetwork)
		},
		newTracker: health.NewTracker,
		newHandler: health.NewHandler,
		newHealthServer: func(addr string, handler http.Handler) daemon.HealthServer {
			return health.NewServer(addr, handler)
		},
		newDaemon: func(
			cfg daemon.Config,
			source daemon.NetworkSource,
			store daemon.HostStore,
			tracker *health.Tracker,
			server daemon.HealthServer,
			logger *slog.Logger,
		) (daemonRunner, error) {
			return daemon.New(cfg, source, store, tracker, server, logger)
		},
	}
}

func runDaemonWith(ctx context.Context, cfg command.Config, logger *slog.Logger, wiring daemonWiring) error {
	source, err := wiring.newDocker()
	if err != nil {
		return fmt.Errorf("create Docker source: %w", err)
	}

	store := wiring.newStore(cfg.HostsFile, cfg.BackupFile, cfg.SectionName, cfg.DefaultNetwork)
	tracker := wiring.newTracker()
	handler := wiring.newHandler(tracker)
	mux := http.NewServeMux()
	mux.Handle("GET /health", handler)
	server := wiring.newHealthServer(net.JoinHostPort(cfg.HealthAddr, fmt.Sprint(cfg.HealthPort)), mux)
	runner, err := wiring.newDaemon(
		daemon.Config{
			Networks:         cfg.Networks,
			DefaultNetwork:   cfg.DefaultNetwork,
			PeriodicInterval: cfg.PeriodicInterval,
			DebounceDelay:    cfg.DebounceDelay,
			MaxDebounceDelay: cfg.MaxDebounceDelay,
		},
		source,
		store,
		tracker,
		server,
		logger,
	)
	if err != nil {
		constructionErr := fmt.Errorf("construct daemon: %w", err)
		closeErr := source.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("close Docker source after failed daemon construction: %w", closeErr)
		}
		return errors.Join(constructionErr, closeErr)
	}

	return runner.Run(ctx)
}

func regenerateHosts(ctx context.Context, cfg command.RegenerateConfig) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if os.Geteuid() != 0 {
		logger.Warn("SDHM should run as root to modify the hosts file")
	}
	logger.Info("regenerating hosts file", "path", cfg.HostsFile)

	err := regenerateHostsWith(ctx, cfg, func(hostsPath, backupPath, sectionName, defaultNetwork string) hostsRegenerator {
		return hosts.NewStore(hostsPath, backupPath, sectionName, defaultNetwork)
	})
	if err != nil {
		return err
	}

	logger.Info("hosts file regenerated", "path", cfg.HostsFile)
	return nil
}

func regenerateHostsWith(
	ctx context.Context,
	cfg command.RegenerateConfig,
	newStore func(string, string, string, string) hostsRegenerator,
) error {
	store := newStore(cfg.HostsFile, cfg.BackupFile, cfg.SectionName, "")
	if err := store.Regenerate(ctx); err != nil {
		return fmt.Errorf("regenerate hosts file: %w", err)
	}
	return nil
}
