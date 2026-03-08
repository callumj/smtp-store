package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"smtp-store/internal/classify"
	"smtp-store/internal/config"
	"smtp-store/internal/smtpserver"
	"smtp-store/internal/storage"
	"smtp-store/internal/webui"
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
	var classifierSvc *classify.Service
	if cfg.ClassificationEnabled() {
		classifierSvc, err = classify.NewService(cfg, logger)
		if err != nil {
			logger.Error("failed to initialize classification service", "error", err)
			os.Exit(1)
		}
		classifierSvc.Start(context.Background())
	}

	smtpSrv, err := smtpserver.New(cfg, store, logger, classifierSvc)
	if err != nil {
		logger.Error("failed to initialize SMTP server", "error", err)
		os.Exit(1)
	}

	webSrv, err := webui.New(cfg, logger)
	if err != nil {
		logger.Error("failed to initialize web server", "error", err)
		os.Exit(1)
	}

	logger.Info(
		"smtp-store starting",
		"smtp_listen_addr", cfg.ListenAddr,
		"web_listen_addr", cfg.Web.ListenAddr,
		"hostname", cfg.Hostname,
		"storage_root", cfg.StorageRoot,
		"starttls", cfg.TLS.Enabled,
		"verbose_logs", cfg.VerboseLogs,
		"ui_users", len(cfg.UIUsers),
		"classification_enabled", cfg.ClassificationEnabled(),
		"classification_provider", cfg.Classification.Provider,
		"classification_model", cfg.Classification.Model,
	)

	errCh := make(chan error, 2)
	go func() {
		errCh <- smtpSrv.ListenAndServe()
	}()
	go func() {
		errCh <- webSrv.ListenAndServe()
	}()

	err = <-errCh
	if errors.Is(err, http.ErrServerClosed) {
		return
	}

	logger.Error("server stopped", "error", err)
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
