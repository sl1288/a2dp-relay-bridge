// Command a2dp-bridge connects Music Assistant to Bluetooth headphones through
// ESP32 relay nodes.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sl1288/a2dp-relay-bridge/internal/bridge"
	"github.com/sl1288/a2dp-relay-bridge/internal/config"
	"github.com/sl1288/a2dp-relay-bridge/internal/hamqtt"
	"github.com/sl1288/a2dp-relay-bridge/internal/web"
)

func main() {
	cfgPath := flag.String("config", "bridge.yaml", "configuration file")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("loading configuration failed", "err", err)
		os.Exit(1)
	}
	store, err := config.OpenStore(cfg.StateFile)
	if err != nil {
		log.Error("loading state failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b := bridge.New(cfg, store, log)
	go func() {
		ac := web.AuthConfig{HomeAssistant: cfg.Web.Auth.HomeAssistant, AdminOnly: cfg.Web.Auth.AdminOnly,
			SessionFile: filepath.Join(filepath.Dir(cfg.StateFile), "sessions.json")}
		if err := web.New(b, log, ac).ListenAndServe(ctx, cfg.Web.Listen); err != nil {
			log.Error("web interface failed", "err", err)
			stop()
		}
	}()
	if cfg.MQTT.Broker != "" {
		go func() { _ = hamqtt.New(cfg.MQTT, b, log).Run(ctx) }()
	}
	log.Info("a2dp bridge starting", "version", bridge.Version, "nodes", len(cfg.Nodes), "sendspin", cfg.MusicAssistant.SendspinURL)
	_ = b.Run(ctx)
	log.Info("a2dp bridge stopped")
}
