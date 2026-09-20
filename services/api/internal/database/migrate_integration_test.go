package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppliedMigrationChecksumCannotChange(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	directory := t.TempDir()
	suffix := time.Now().UTC().Format("20060102150405.000000000")
	name := "900_checksum_" + strings.ReplaceAll(suffix, ".", "") + ".sql"
	table := "migration_checksum_" + strings.ReplaceAll(suffix, ".", "")
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("CREATE TABLE "+table+" (id integer PRIMARY KEY);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.Pool().Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
		_, _ = store.Pool().Exec(context.Background(), `DELETE FROM schema_migrations WHERE name=$1`, name)
	}()
	if err := Migrate(ctx, store.Pool(), directory); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if err := Migrate(ctx, store.Pool(), directory); err != nil {
		t.Fatalf("unchanged migration is not idempotent: %v", err)
	}
	if err := os.WriteFile(path, []byte("CREATE TABLE "+table+" (id bigint PRIMARY KEY);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, store.Pool(), directory); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("changed applied migration error = %v", err)
	}
}
