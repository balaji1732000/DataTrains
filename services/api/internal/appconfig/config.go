package appconfig

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvironmentLocal      = "local"
	EnvironmentTest       = "test"
	EnvironmentStaging    = "staging"
	EnvironmentProduction = "production"
)

type Common struct {
	Environment       string
	DatabaseURL       string
	DatabaseMaxConns  int32
	MigrationsDir     string
	RunMigrations     bool
	BlobBackend       string
	BlobRoot          string
	R2Endpoint        string
	R2Bucket          string
	R2AccessKeyID     string
	R2SecretAccessKey string
	AuthMode          string
	OIDCIssuer        string
	OIDCAudience      string
	SchemaPath        string
	ShutdownTimeout   time.Duration
	GCPProject        string
}

type Migration struct {
	Environment      string
	DatabaseURL      string
	DatabaseMaxConns int32
	MigrationsDir    string
}

type IdentityBootstrap struct {
	Migration
	Issuer           string
	Subject          string
	Email            string
	DisplayName      string
	OrganizationName string
}

type API struct {
	Common
	Address                string
	AllowedOrigins         []string
	UploadAuthorizationTTL time.Duration
}

type Worker struct {
	Common
	PollInterval time.Duration
	Drain        bool
	FFprobePath  string
	FFmpegPath   string
}

func LoadAPI() (API, error)       { return loadAPI(os.Getenv) }
func LoadWorker() (Worker, error) { return loadWorker(os.Getenv) }
func LoadMigration() (Migration, error) {
	return loadMigration(os.Getenv)
}
func LoadIdentityBootstrap() (IdentityBootstrap, error) {
	return loadIdentityBootstrap(os.Getenv)
}

func loadAPI(getenv func(string) string) (API, error) {
	common, err := loadCommon(getenv)
	if err != nil {
		return API{}, err
	}
	address, err := apiAddress(getenv)
	if err != nil {
		return API{}, err
	}
	origins, err := allowedOrigins(getenv, common.Environment)
	if err != nil {
		return API{}, err
	}
	uploadTTL, err := durationValue(getenv, "TRAJECTORY_UPLOAD_AUTHORIZATION_TTL", 15*time.Minute, time.Minute, time.Hour)
	if err != nil {
		return API{}, err
	}
	return API{Common: common, Address: address, AllowedOrigins: origins, UploadAuthorizationTTL: uploadTTL}, nil
}

func loadWorker(getenv func(string) string) (Worker, error) {
	common, err := loadCommon(getenv)
	if err != nil {
		return Worker{}, err
	}
	pollInterval, err := durationValue(getenv, "TRAJECTORY_WORKER_POLL_INTERVAL", time.Second, 100*time.Millisecond, time.Minute)
	if err != nil {
		return Worker{}, err
	}
	drain, err := boolValue(getenv, "TRAJECTORY_WORKER_DRAIN", false)
	if err != nil {
		return Worker{}, err
	}
	return Worker{
		Common: common, PollInterval: pollInterval, Drain: drain,
		FFprobePath: valueOr(getenv, "TRAJECTORY_FFPROBE_PATH", "ffprobe"),
		FFmpegPath:  valueOr(getenv, "TRAJECTORY_FFMPEG_PATH", "ffmpeg"),
	}, nil
}

func loadCommon(getenv func(string) string) (Common, error) {
	environment := strings.ToLower(strings.TrimSpace(valueOr(getenv, "TRAJECTORY_ENVIRONMENT", EnvironmentLocal)))
	switch environment {
	case EnvironmentLocal, EnvironmentTest, EnvironmentStaging, EnvironmentProduction:
	default:
		return Common{}, fmt.Errorf("TRAJECTORY_ENVIRONMENT must be local, test, staging, or production")
	}

	databaseURL := strings.TrimSpace(getenv("TRAJECTORY_DATABASE_URL"))
	if databaseURL == "" {
		if environment == EnvironmentLocal || environment == EnvironmentTest {
			databaseURL = "postgres://trajectory@127.0.0.1:55432/trajectory?sslmode=disable"
		} else {
			return Common{}, errors.New("TRAJECTORY_DATABASE_URL is required outside local and test environments")
		}
	}
	if err := validateDatabaseURL(databaseURL, environment); err != nil {
		return Common{}, err
	}

	maxConns, err := int32Value(getenv, "TRAJECTORY_DATABASE_MAX_CONNS", 4, 1, 100)
	if err != nil {
		return Common{}, err
	}
	runMigrationsDefault := environment == EnvironmentLocal || environment == EnvironmentTest
	runMigrations, err := boolValue(getenv, "TRAJECTORY_RUN_MIGRATIONS", runMigrationsDefault)
	if err != nil {
		return Common{}, err
	}
	shutdownTimeout, err := durationValue(getenv, "TRAJECTORY_SHUTDOWN_TIMEOUT", 20*time.Second, time.Second, 2*time.Minute)
	if err != nil {
		return Common{}, err
	}

	blobBackend := strings.ToLower(strings.TrimSpace(valueOr(getenv, "TRAJECTORY_BLOB_BACKEND", "local")))
	if blobBackend != "local" && blobBackend != "r2" {
		return Common{}, errors.New("TRAJECTORY_BLOB_BACKEND must be local or r2")
	}
	if environment == EnvironmentProduction && blobBackend == "local" {
		return Common{}, errors.New("production cannot use the ephemeral local blob backend; configure R2")
	}
	r2Endpoint := strings.TrimSpace(getenv("TRAJECTORY_R2_ENDPOINT"))
	r2Bucket := strings.TrimSpace(getenv("TRAJECTORY_R2_BUCKET"))
	r2AccessKeyID := strings.TrimSpace(getenv("TRAJECTORY_R2_ACCESS_KEY_ID"))
	r2SecretAccessKey := strings.TrimSpace(getenv("TRAJECTORY_R2_SECRET_ACCESS_KEY"))
	if blobBackend == "r2" {
		if r2Endpoint == "" || r2Bucket == "" || r2AccessKeyID == "" || r2SecretAccessKey == "" {
			return Common{}, errors.New("R2 backend requires TRAJECTORY_R2_ENDPOINT, TRAJECTORY_R2_BUCKET, TRAJECTORY_R2_ACCESS_KEY_ID, and TRAJECTORY_R2_SECRET_ACCESS_KEY")
		}
		if !strings.HasPrefix(r2Endpoint, "https://") {
			return Common{}, errors.New("TRAJECTORY_R2_ENDPOINT must use HTTPS")
		}
	}
	authModeDefault := "local"
	if environment == EnvironmentProduction {
		authModeDefault = "oidc"
	}
	authMode := strings.ToLower(strings.TrimSpace(valueOr(getenv, "TRAJECTORY_AUTH_MODE", authModeDefault)))
	if authMode != "local" && authMode != "oidc" {
		return Common{}, errors.New("TRAJECTORY_AUTH_MODE must be local or oidc")
	}
	if environment == EnvironmentProduction && authMode != "oidc" {
		return Common{}, errors.New("production requires OpenID authentication")
	}
	// OpenID Connect requires an exact match with provider metadata and the
	// token `iss` claim. Providers such as Auth0 include the trailing slash in
	// that identifier, so it must not be canonicalized away.
	oidcIssuer := strings.TrimSpace(getenv("TRAJECTORY_OIDC_ISSUER"))
	oidcAudience := strings.TrimSpace(getenv("TRAJECTORY_OIDC_AUDIENCE"))
	if authMode == "oidc" {
		if oidcIssuer == "" || oidcAudience == "" {
			return Common{}, errors.New("OpenID authentication requires TRAJECTORY_OIDC_ISSUER and TRAJECTORY_OIDC_AUDIENCE")
		}
		if !strings.HasPrefix(oidcIssuer, "https://") {
			return Common{}, errors.New("TRAJECTORY_OIDC_ISSUER must use HTTPS")
		}
	}

	return Common{
		Environment:       environment,
		DatabaseURL:       databaseURL,
		DatabaseMaxConns:  maxConns,
		MigrationsDir:     valueOr(getenv, "TRAJECTORY_MIGRATIONS_DIR", "migrations"),
		RunMigrations:     runMigrations,
		BlobBackend:       blobBackend,
		BlobRoot:          valueOr(getenv, "TRAJECTORY_BLOB_ROOT", "../../data/blobstore"),
		R2Endpoint:        r2Endpoint,
		R2Bucket:          r2Bucket,
		R2AccessKeyID:     r2AccessKeyID,
		R2SecretAccessKey: r2SecretAccessKey,
		AuthMode:          authMode,
		OIDCIssuer:        oidcIssuer,
		OIDCAudience:      oidcAudience,
		SchemaPath:        valueOr(getenv, "TRAJECTORY_SCHEMA_PATH", "../../packages/trajectory-schema/schemas/trajectory-v1.json"),
		ShutdownTimeout:   shutdownTimeout,
		GCPProject:        strings.TrimSpace(getenv("TRAJECTORY_GCP_PROJECT")),
	}, nil
}

func loadMigration(getenv func(string) string) (Migration, error) {
	environment := strings.ToLower(strings.TrimSpace(valueOr(getenv, "TRAJECTORY_ENVIRONMENT", EnvironmentLocal)))
	switch environment {
	case EnvironmentLocal, EnvironmentTest, EnvironmentStaging, EnvironmentProduction:
	default:
		return Migration{}, errors.New("TRAJECTORY_ENVIRONMENT must be local, test, staging, or production")
	}
	databaseURL := strings.TrimSpace(getenv("TRAJECTORY_DATABASE_URL"))
	if databaseURL == "" {
		if environment == EnvironmentLocal || environment == EnvironmentTest {
			databaseURL = "postgres://trajectory@127.0.0.1:55432/trajectory?sslmode=disable"
		} else {
			return Migration{}, errors.New("TRAJECTORY_DATABASE_URL is required outside local and test environments")
		}
	}
	if err := validateDatabaseURL(databaseURL, environment); err != nil {
		return Migration{}, err
	}
	maxConns, err := int32Value(getenv, "TRAJECTORY_DATABASE_MAX_CONNS", 1, 1, 4)
	if err != nil {
		return Migration{}, err
	}
	return Migration{
		Environment:      environment,
		DatabaseURL:      databaseURL,
		DatabaseMaxConns: maxConns,
		MigrationsDir:    valueOr(getenv, "TRAJECTORY_MIGRATIONS_DIR", "migrations"),
	}, nil
}

func validateDatabaseURL(value, environment string) error {
	if environment == EnvironmentLocal || environment == EnvironmentTest {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || !matches(parsed.Scheme, "postgres", "postgresql") || parsed.Host == "" || parsed.User == nil {
		return errors.New("TRAJECTORY_DATABASE_URL must be a PostgreSQL URL outside local and test environments")
	}
	sslMode := strings.ToLower(strings.TrimSpace(parsed.Query().Get("sslmode")))
	if !matches(sslMode, "require", "verify-ca", "verify-full") {
		return errors.New("TRAJECTORY_DATABASE_URL must explicitly require TLS outside local and test environments")
	}
	return nil
}

func matches(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func loadIdentityBootstrap(getenv func(string) string) (IdentityBootstrap, error) {
	migration, err := loadMigration(getenv)
	if err != nil {
		return IdentityBootstrap{}, err
	}
	config := IdentityBootstrap{
		Migration:        migration,
		// Keep the bootstrap identity byte-for-byte compatible with the OIDC
		// issuer claim used during subsequent authentication.
		Issuer:           strings.TrimSpace(getenv("TRAJECTORY_BOOTSTRAP_ADMIN_ISSUER")),
		Subject:          strings.TrimSpace(getenv("TRAJECTORY_BOOTSTRAP_ADMIN_SUBJECT")),
		Email:            strings.TrimSpace(getenv("TRAJECTORY_BOOTSTRAP_ADMIN_EMAIL")),
		DisplayName:      strings.TrimSpace(getenv("TRAJECTORY_BOOTSTRAP_ADMIN_DISPLAY_NAME")),
		OrganizationName: strings.TrimSpace(getenv("TRAJECTORY_BOOTSTRAP_ORGANIZATION_NAME")),
	}
	if config.Issuer == "" || config.Subject == "" || config.Email == "" || config.DisplayName == "" || config.OrganizationName == "" {
		return IdentityBootstrap{}, errors.New("bootstrap admin issuer, subject, email, display name, and organization name are required")
	}
	if !strings.HasPrefix(config.Issuer, "https://") {
		return IdentityBootstrap{}, errors.New("bootstrap admin issuer must use HTTPS")
	}
	return config, nil
}

func apiAddress(getenv func(string) string) (string, error) {
	if address := strings.TrimSpace(getenv("TRAJECTORY_API_ADDRESS")); address != "" {
		if _, _, err := net.SplitHostPort(address); err != nil {
			return "", fmt.Errorf("invalid TRAJECTORY_API_ADDRESS: %w", err)
		}
		return address, nil
	}
	if rawPort := strings.TrimSpace(getenv("PORT")); rawPort != "" {
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return "", errors.New("PORT must be an integer between 1 and 65535")
		}
		return net.JoinHostPort("0.0.0.0", rawPort), nil
	}
	return "127.0.0.1:8080", nil
}

func allowedOrigins(getenv func(string) string, environment string) ([]string, error) {
	raw := strings.TrimSpace(getenv("TRAJECTORY_ALLOWED_ORIGINS"))
	if raw == "" {
		if environment == EnvironmentLocal || environment == EnvironmentTest {
			return []string{"http://localhost:1420", "http://127.0.0.1:1420", "tauri://localhost", "https://tauri.localhost"}, nil
		}
		return nil, errors.New("TRAJECTORY_ALLOWED_ORIGINS is required outside local and test environments")
	}
	seen := make(map[string]bool)
	origins := make([]string, 0)
	for _, candidate := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(candidate)
		if origin == "" || origin == "*" {
			return nil, errors.New("allowed origins must be explicit and cannot contain a wildcard")
		}
		if !seen[origin] {
			seen[origin] = true
			origins = append(origins, origin)
		}
	}
	if len(origins) == 0 {
		return nil, errors.New("at least one allowed origin is required")
	}
	return origins, nil
}

func valueOr(getenv func(string) string, name, fallback string) string {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return fallback
}

func boolValue(getenv func(string) string, name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value, nil
}

func int32Value(getenv func(string) string, name string, fallback, minimum, maximum int32) (int32, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value < int64(minimum) || value > int64(maximum) {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return int32(value), nil
}

func durationValue(getenv func(string) string, name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be a duration between %s and %s", name, minimum, maximum)
	}
	return value, nil
}
