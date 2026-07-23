package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/backup"
	"github.com/nikunj-taneja/seedhibaat/internal/config"
	"github.com/nikunj-taneja/seedhibaat/internal/meta"
	"github.com/nikunj-taneja/seedhibaat/internal/service"
	"github.com/nikunj-taneja/seedhibaat/internal/shopify"
	"github.com/nikunj-taneja/seedhibaat/internal/store"
	"github.com/nikunj-taneja/seedhibaat/internal/workflow"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seedhibaatd:", err)
		os.Exit(1)
	}
}

func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))
	if command == "validate-workflows" {
		loaded, err := workflow.LoadDir(cfg.WorkflowDir)
		if err != nil {
			return err
		}
		for _, item := range loaded {
			fmt.Printf("%s v%d %s\n", item.Definition.Name, item.Definition.Version, item.Hash[:12])
		}
		return nil
	}
	if command == "restore" {
		if len(os.Args) != 4 {
			return errors.New("restore requires an encrypted backup path and a new output database path")
		}
		if err := backup.Restore(context.Background(), os.Args[2], os.Args[3], cfg.BackupKey); err != nil {
			return err
		}
		fmt.Println(os.Args[3])
		return nil
	}
	if cfg.DatabasePath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0700); err != nil {
			return err
		}
	}
	ctx := context.Background()
	database, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	switch command {
	case "migrate":
		return nil
	case "integrity":
		return database.IntegrityCheck(ctx)
	case "backup":
		path, err := backup.Create(ctx, database, cfg.BackupDir, cfg.BackupKey, time.Now())
		if err != nil {
			return err
		}
		if _, err := backup.Prune(cfg.BackupDir, time.Now().AddDate(0, 0, -cfg.RetentionDays)); err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	case "serve":
	default:
		return fmt.Errorf("unknown command %q (use serve, migrate, integrity, backup, restore, or validate-workflows)", command)
	}
	if err := cfg.ValidateForServe(); err != nil {
		return err
	}
	shopifyClient, err := shopify.NewClient(cfg.ShopifyShopDomain, cfg.ShopifyClientID, cfg.ShopifyClientSecret, cfg.ShopifyAPIVersion)
	if err != nil {
		return err
	}
	metaClient := meta.NewClient(cfg.MetaAPIVersion, cfg.MetaAccessToken, cfg.MetaWABAID, cfg.MetaPhoneNumberID)
	processor := &service.Processor{Config: cfg, Store: database, Meta: metaClient, Shopify: shopifyClient, Logger: logger}
	httpService := &service.HTTPServer{Config: cfg, Store: database, Logger: logger}
	server := &http.Server{Addr: cfg.ListenAddr, Handler: httpService.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 2)
	go func() {
		logger.Info("daemon starting", "listen_addr", cfg.ListenAddr, "production_flow_enabled", cfg.ProductionFlowEnabled, "outbound_sending_enabled", cfg.OutboundSendingEnabled)
		errCh <- server.ListenAndServe()
	}()
	go func() { errCh <- processor.Run(runCtx) }()
	select {
	case <-runCtx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) && err != nil {
			stop()
			return err
		}
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("daemon stopped")
	return nil
}

func logLevel(value string) slog.Level {
	switch value {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
