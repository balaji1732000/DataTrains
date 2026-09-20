package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"trajectory.local/api/internal/appconfig"
	"trajectory.local/api/internal/database"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(); err != nil {
		logger.Error("migration_failed", "error", err)
		os.Exit(1)
	}
	logger.Info("migration_completed")
}

func run() error {
	config, err := appconfig.LoadMigration()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store, err := database.OpenWithMaxConnections(ctx, config.DatabaseURL, config.DatabaseMaxConns)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := database.Migrate(ctx, store.Pool(), config.MigrationsDir); err != nil {
		return err
	}
	return nil
}
