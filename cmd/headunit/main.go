package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"headunit/internal/config"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(context.Background(), logger); err != nil {
		logger.Error("headunit exited with error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info(
		"headunit starting",
		"port", cfg.Port,
		"music_dir", cfg.MusicDir,
		"data_dir", cfg.DataDir,
		"scan_interval", cfg.ScanInterval,
	)

	<-ctx.Done()
	logger.Info("headunit shutting down")
	return nil
}
