package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"smtp-store/internal/classify"
	"smtp-store/internal/config"
	"smtp-store/internal/fileindex"
	"smtp-store/internal/mqttnotify"
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
	ctx := context.Background()

	if err := storage.CheckWritable(cfg.StorageRoot); err != nil {
		logger.Error("storage root is not writable", "error", err, "storage_root", cfg.StorageRoot)
		os.Exit(1)
	}

	errCh := make(chan error, 3)
	var storageFaultSignaled atomic.Bool
	storageFaultHandler := func(err error) {
		if !storageFaultSignaled.CompareAndSwap(false, true) {
			return
		}
		logger.Error("storage fault detected; exiting for systemd restart", "error", err, "storage_root", cfg.StorageRoot)
		go func() {
			time.Sleep(2 * time.Second)
			errCh <- fmt.Errorf("storage fault: %w", err)
		}()
	}
	startStorageWatchdog(ctx, cfg.StorageRoot, logger, storageFaultHandler)

	var index *fileindex.Index
	if cfg.IndexEnabled() {
		index, err = fileindex.Open(cfg.IndexPath, cfg.StorageRoot, logger)
		if err != nil {
			logger.Error("failed to initialize file index", "error", err, "path", cfg.IndexPath)
			os.Exit(1)
		}
		defer index.Close()
		go func() {
			if err := index.Backfill(ctx); err != nil {
				logger.Warn("file index backfill stopped", "error", err)
			}
		}()
	}

	var notifier *mqttnotify.Publisher
	if cfg.MQTTEnabled() {
		notifier = mqttnotify.New(cfg, logger)
		if err := notifier.Start(ctx); err != nil {
			logger.Error("failed to initialize mqtt publisher", "error", err)
			os.Exit(1)
		}
	}

	var classifierSvc *classify.Service
	if cfg.ClassificationEnabled() {
		classifierSvc, err = classify.NewService(cfg, logger)
		if err != nil {
			logger.Error("failed to initialize classification service", "error", err)
			os.Exit(1)
		}
		if notifier != nil {
			classifierSvc.SetNotificationPublisher(notifier)
		}
		if index != nil {
			classifierSvc.SetMetadataIndexer(index)
		}
		classifierSvc.Start(ctx)
	}

	smtpSrv, err := smtpserver.NewWithStorageFaultHandler(cfg, store, logger, classifierSvc, index, storageFaultHandler)
	if err != nil {
		logger.Error("failed to initialize SMTP server", "error", err)
		os.Exit(1)
	}

	webSrv, err := webui.NewWithIndex(cfg, logger, index)
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
		"index_enabled", cfg.IndexEnabled(),
		"index_path", cfg.IndexPath,
		"starttls", cfg.TLS.Enabled,
		"verbose_logs", cfg.VerboseLogs,
		"ui_users", len(cfg.UIUsers),
		"classification_enabled", cfg.ClassificationEnabled(),
		"classification_provider", cfg.Classification.Provider,
		"classification_model", cfg.Classification.Model,
		"mqtt_enabled", cfg.MQTTEnabled(),
	)

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

func startStorageWatchdog(ctx context.Context, root string, logger *slog.Logger, storageFaultHandler func(error)) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := storage.CheckWritable(root); err != nil {
					logger.Error("storage watchdog failed", "error", err, "storage_root", root)
					storageFaultHandler(err)
					return
				}
			}
		}
	}()
}
