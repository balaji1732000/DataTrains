package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"trajectory.local/api/internal/appconfig"
	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("worker_stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := appconfig.LoadWorker()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	store, err := database.OpenWithMaxConnections(ctx, config.DatabaseURL, config.DatabaseMaxConns)
	if err != nil {
		return err
	}
	defer store.Close()
	if config.RunMigrations {
		if err := database.Migrate(ctx, store.Pool(), config.MigrationsDir); err != nil {
			return err
		}
	}
	blobs, err := blobstore.Open(ctx, blobstore.Config{
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
	schemaDocument, err := os.ReadFile(config.SchemaPath)
	if err != nil {
		return err
	}
	mediaInspector, err := worker.NewFFprobeInspector(config.FFprobePath)
	if err != nil {
		return err
	}
	videoRedactor, err := worker.NewFFmpegRedactor(config.FFmpegPath)
	if err != nil {
		return err
	}
	validator, err := worker.NewValidatorWithMediaInspector(blobs, schemaDocument, mediaInspector)
	if err != nil {
		return err
	}
	processor, err := worker.NewProcessor(store, blobs, validator)
	if err != nil {
		return err
	}
	processor, err = processor.WithVideoRedaction(mediaInspector, videoRedactor)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	workerID := fmt.Sprintf("worker:%s:%d", hostname, os.Getpid())
	logger.Info("worker_started", "worker_id", workerID, "environment", config.Environment)
	logQueueSnapshot(ctx, logger, store)
	for {
		processed, err := processor.ProcessNext(ctx, workerID)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("processing_failed", "worker_id", workerID, "error", err)
		}
		if ctx.Err() != nil {
			logger.Info("worker_stopped", "worker_id", workerID, "reason", "signal")
			return nil
		}
		if processed {
			continue
		}
		if config.Drain {
			logQueueSnapshot(ctx, logger, store)
			logger.Info("worker_stopped", "worker_id", workerID, "reason", "drained")
			return nil
		}
		select {
		case <-ctx.Done():
			logger.Info("worker_stopped", "worker_id", workerID, "reason", "signal")
			return nil
		case <-time.After(config.PollInterval):
		}
	}
}

func logQueueSnapshot(ctx context.Context, logger *slog.Logger, store *database.Store) {
	snapshot, err := store.GetQueueSnapshot(ctx, time.Now())
	if err != nil {
		logger.Error("worker_queue_snapshot_failed", "error", err)
		return
	}
	logger.Info("worker_queue_snapshot",
		"processing_queued", snapshot.ProcessingQueued,
		"processing_leased", snapshot.ProcessingLeased,
		"processing_dead_letter", snapshot.ProcessingDeadLetter,
		"redaction_queued", snapshot.RedactionQueued,
		"redaction_leased", snapshot.RedactionLeased,
		"redaction_dead_letter", snapshot.RedactionDeadLetter,
		"deletion_queued", snapshot.DeletionQueued,
		"deletion_leased", snapshot.DeletionLeased,
		"deletion_blocked", snapshot.DeletionBlocked,
		"deletion_dead_letter", snapshot.DeletionDeadLetter,
		"retention_queued", snapshot.RetentionQueued,
		"retention_leased", snapshot.RetentionLeased,
		"retention_blocked", snapshot.RetentionBlocked,
		"retention_dead_letter", snapshot.RetentionDeadLetter,
		"oldest_ready_age_seconds", snapshot.OldestReadyAgeSeconds)
}
