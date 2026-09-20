package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"trajectory.local/api/internal/appconfig"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/id"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	result, err := run()
	if err != nil {
		logger.Error("administrator_bootstrap_failed", "error", err)
		os.Exit(1)
	}
	logger.Info("administrator_bootstrap_completed", "account_id", result.AccountID, "organization_id", result.OrganizationID)
}

func run() (database.AdministratorBootstrapResult, error) {
	config, err := appconfig.LoadIdentityBootstrap()
	if err != nil {
		return database.AdministratorBootstrapResult{}, fmt.Errorf("load configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := database.OpenWithMaxConnections(ctx, config.DatabaseURL, config.DatabaseMaxConns)
	if err != nil {
		return database.AdministratorBootstrapResult{}, err
	}
	defer store.Close()
	requestID, err := id.New("req")
	if err != nil {
		return database.AdministratorBootstrapResult{}, err
	}
	result, err := store.BootstrapAdministrator(ctx, database.AdministratorBootstrap{
		Issuer: config.Issuer, Subject: config.Subject, Email: config.Email,
		DisplayName: config.DisplayName, OrganizationName: config.OrganizationName,
	}, requestID, time.Now())
	if err != nil {
		return database.AdministratorBootstrapResult{}, err
	}
	return result, nil
}
