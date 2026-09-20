package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"trajectory.local/api/internal/appconfig"
	"trajectory.local/api/internal/authn"
	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/httpapi"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("api_stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := appconfig.LoadAPI()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	startupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := database.OpenWithMaxConnections(startupContext, config.DatabaseURL, config.DatabaseMaxConns)
	if err != nil {
		return err
	}
	defer store.Close()
	if config.RunMigrations {
		if err := database.Migrate(startupContext, store.Pool(), config.MigrationsDir); err != nil {
			return err
		}
	}
	blobs, err := blobstore.Open(startupContext, blobstore.Config{
		Backend:       config.BlobBackend,
		LocalRoot:     config.BlobRoot,
		R2Endpoint:    config.R2Endpoint,
		R2Bucket:      config.R2Bucket,
		R2AccessKeyID: config.R2AccessKeyID,
		R2SecretKey:   config.R2SecretAccessKey,
	})
	if err != nil {
		return err
	}
	authenticator, err := authn.New(startupContext, authn.Config{
		Mode: config.AuthMode, Issuer: config.OIDCIssuer, Audience: config.OIDCAudience,
	})
	if err != nil {
		return err
	}
	schemaDocument, err := os.ReadFile(config.SchemaPath)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              config.Address,
		Handler:           httpapi.NewWithBlobStore(store, blobs, schemaDocument).WithAuthenticator(authenticator).WithAllowedOrigins(config.AllowedOrigins).WithUploadAuthorizationTTL(config.UploadAuthorizationTTL).WithLogger(logger).WithTraceProject(config.GCPProject).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// Artifact bodies may be several GiB. Keep finite limits for stalled local
		// clients without imposing the short control-plane timeout on uploads.
		ReadTimeout:  30 * time.Minute,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	runContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.ListenAndServe()
	}()

	logger.Info("api_started", "address", config.Address, "environment", config.Environment)
	select {
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-runContext.Done():
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			logger.Error("api_shutdown_failed", "error", err)
			_ = server.Close()
		}
		if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	logger.Info("api_stopped")
	return nil
}
