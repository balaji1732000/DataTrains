package appconfig

import (
	"strings"
	"testing"
)

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestCloudRunAddressUsesPort(t *testing.T) {
	config, err := loadAPI(environment(map[string]string{
		"PORT":                       "9090",
		"TRAJECTORY_ENVIRONMENT":     "staging",
		"TRAJECTORY_DATABASE_URL":    "postgres://runtime@example.invalid/trajectory?sslmode=require",
		"TRAJECTORY_ALLOWED_ORIGINS": "https://admin.example.com,tauri://localhost",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if config.Address != "0.0.0.0:9090" {
		t.Fatalf("expected Cloud Run address, got %q", config.Address)
	}
	if config.RunMigrations {
		t.Fatal("staging must not run migrations during API startup by default")
	}
}

func TestProductionRejectsLocalBlobStorage(t *testing.T) {
	_, err := loadAPI(environment(map[string]string{
		"TRAJECTORY_ENVIRONMENT":     "production",
		"TRAJECTORY_DATABASE_URL":    "postgres://runtime@example.invalid/trajectory?sslmode=require",
		"TRAJECTORY_ALLOWED_ORIGINS": "https://admin.example.com",
	}))
	if err == nil || !strings.Contains(err.Error(), "cannot use the ephemeral local blob backend") {
		t.Fatalf("expected production local storage rejection, got %v", err)
	}
}

func TestProductionAcceptsCompleteR2Configuration(t *testing.T) {
	config, err := loadAPI(environment(map[string]string{
		"PORT":                            "8080",
		"TRAJECTORY_ENVIRONMENT":          "production",
		"TRAJECTORY_DATABASE_URL":         "postgres://runtime@example.invalid/trajectory?sslmode=require",
		"TRAJECTORY_ALLOWED_ORIGINS":      "https://admin.example.com",
		"TRAJECTORY_BLOB_BACKEND":         "r2",
		"TRAJECTORY_R2_ENDPOINT":          "https://account.r2.cloudflarestorage.com",
		"TRAJECTORY_R2_BUCKET":            "datatrains-production",
		"TRAJECTORY_R2_ACCESS_KEY_ID":     "access-key",
		"TRAJECTORY_R2_SECRET_ACCESS_KEY": "secret-key",
		"TRAJECTORY_OIDC_ISSUER":          "https://identity.example.com",
		"TRAJECTORY_OIDC_AUDIENCE":        "datatrains-api",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if config.BlobBackend != "r2" || config.R2Bucket != "datatrains-production" {
		t.Fatalf("unexpected R2 configuration: %#v", config.Common)
	}
}

func TestProductionOIDCIssuerPreservesTrailingSlash(t *testing.T) {
	config, err := loadAPI(environment(map[string]string{
		"TRAJECTORY_ENVIRONMENT":          "production",
		"TRAJECTORY_DATABASE_URL":         "postgres://runtime@example.invalid/trajectory?sslmode=require",
		"TRAJECTORY_ALLOWED_ORIGINS":      "https://admin.example.com",
		"TRAJECTORY_BLOB_BACKEND":         "r2",
		"TRAJECTORY_R2_ENDPOINT":          "https://account.r2.cloudflarestorage.com",
		"TRAJECTORY_R2_BUCKET":            "datatrains-production",
		"TRAJECTORY_R2_ACCESS_KEY_ID":     "access-key",
		"TRAJECTORY_R2_SECRET_ACCESS_KEY": "secret-key",
		"TRAJECTORY_OIDC_ISSUER":          "https://identity.example.com/",
		"TRAJECTORY_OIDC_AUDIENCE":        "datatrains-api",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if config.OIDCIssuer != "https://identity.example.com/" {
		t.Fatalf("OIDC issuer = %q, want trailing slash preserved", config.OIDCIssuer)
	}
}

func TestBootstrapAdminIssuerPreservesTrailingSlash(t *testing.T) {
	config, err := loadIdentityBootstrap(environment(map[string]string{
		"TRAJECTORY_ENVIRONMENT":                  "production",
		"TRAJECTORY_DATABASE_URL":                 "postgres://owner@example.invalid/trajectory?sslmode=require",
		"TRAJECTORY_BOOTSTRAP_ADMIN_ISSUER":       "https://identity.example.com/",
		"TRAJECTORY_BOOTSTRAP_ADMIN_SUBJECT":      "auth0|administrator",
		"TRAJECTORY_BOOTSTRAP_ADMIN_EMAIL":        "admin@example.com",
		"TRAJECTORY_BOOTSTRAP_ADMIN_DISPLAY_NAME": "DataTrains Admin",
		"TRAJECTORY_BOOTSTRAP_ORGANIZATION_NAME":  "DataTrains",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if config.Issuer != "https://identity.example.com/" {
		t.Fatalf("bootstrap issuer = %q, want trailing slash preserved", config.Issuer)
	}
}

func TestProductionRejectsLocalAuthentication(t *testing.T) {
	_, err := loadAPI(environment(map[string]string{
		"TRAJECTORY_ENVIRONMENT":          "production",
		"TRAJECTORY_DATABASE_URL":         "postgres://runtime@example.invalid/trajectory?sslmode=require",
		"TRAJECTORY_ALLOWED_ORIGINS":      "https://admin.example.com",
		"TRAJECTORY_BLOB_BACKEND":         "r2",
		"TRAJECTORY_R2_ENDPOINT":          "https://account.r2.cloudflarestorage.com",
		"TRAJECTORY_R2_BUCKET":            "datatrains-production",
		"TRAJECTORY_R2_ACCESS_KEY_ID":     "access-key",
		"TRAJECTORY_R2_SECRET_ACCESS_KEY": "secret-key",
		"TRAJECTORY_AUTH_MODE":            "local",
	}))
	if err == nil || !strings.Contains(err.Error(), "requires OpenID") {
		t.Fatalf("expected production local authentication rejection, got %v", err)
	}
}

func TestMigrationDoesNotRequireBlobCredentials(t *testing.T) {
	config, err := loadMigration(environment(map[string]string{
		"TRAJECTORY_ENVIRONMENT":  "production",
		"TRAJECTORY_DATABASE_URL": "postgres://runtime@example.invalid/trajectory?sslmode=require",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if config.DatabaseMaxConns != 1 {
		t.Fatalf("expected one migration connection, got %d", config.DatabaseMaxConns)
	}
}

func TestIdentityBootstrapRequiresCompleteIdentity(t *testing.T) {
	_, err := loadIdentityBootstrap(environment(map[string]string{}))
	if err == nil || !strings.Contains(err.Error(), "bootstrap admin") {
		t.Fatalf("expected incomplete bootstrap identity to fail, got %v", err)
	}
}

func TestNonLocalConfigurationRequiresDatabaseAndOrigins(t *testing.T) {
	_, err := loadAPI(environment(map[string]string{"TRAJECTORY_ENVIRONMENT": "staging"}))
	if err == nil || !strings.Contains(err.Error(), "TRAJECTORY_DATABASE_URL") {
		t.Fatalf("expected database URL validation, got %v", err)
	}

	_, err = loadAPI(environment(map[string]string{
		"TRAJECTORY_ENVIRONMENT":  "staging",
		"TRAJECTORY_DATABASE_URL": "postgres://runtime@example.invalid/trajectory?sslmode=require",
	}))
	if err == nil || !strings.Contains(err.Error(), "TRAJECTORY_ALLOWED_ORIGINS") {
		t.Fatalf("expected origin validation, got %v", err)
	}
}

func TestNonLocalDatabaseRequiresExplicitTLS(t *testing.T) {
	for _, databaseURL := range []string{
		"postgres://user@example.invalid/trajectory",
		"postgres://user@example.invalid/trajectory?sslmode=disable",
		"host=example.invalid user=runtime sslmode=require",
	} {
		_, err := loadMigration(environment(map[string]string{
			"TRAJECTORY_ENVIRONMENT":  "production",
			"TRAJECTORY_DATABASE_URL": databaseURL,
		}))
		if err == nil || !strings.Contains(err.Error(), "TRAJECTORY_DATABASE_URL") {
			t.Fatalf("expected insecure database URL %q to fail, got %v", databaseURL, err)
		}
	}
}

func TestAllowedOriginsRejectWildcard(t *testing.T) {
	_, err := allowedOrigins(environment(map[string]string{
		"TRAJECTORY_ALLOWED_ORIGINS": "https://admin.example.com,*",
	}), EnvironmentProduction)
	if err == nil || !strings.Contains(err.Error(), "wildcard") {
		t.Fatalf("expected wildcard rejection, got %v", err)
	}
}

func TestWorkerDrainConfiguration(t *testing.T) {
	config, err := loadWorker(environment(map[string]string{
		"TRAJECTORY_WORKER_DRAIN": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Drain {
		t.Fatal("expected drain mode")
	}
}
