package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"smtp-store/internal/config"
	"smtp-store/internal/smtpserver"
	"smtp-store/internal/storage"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "config.yaml", "path to YAML config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err, "path", configPath)
		os.Exit(1)
	}

	store := storage.New(cfg.StorageRoot)
	srv, err := smtpserver.New(cfg, store, logger)
	if err != nil {
		logger.Error("failed to initialize SMTP server", "error", err)
		os.Exit(1)
	}

	logger.Info(
		"smtp-store starting",
		"listen_addr", cfg.ListenAddr,
		"hostname", cfg.Hostname,
		"storage_root", cfg.StorageRoot,
		"starttls", cfg.TLS.Enabled,
		"verbose_logs", cfg.VerboseLogs,
	)

	if err := srv.ListenAndServe(); err != nil {
		logger.Error("smtp server stopped", "error", err)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
