package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Migrate(ctx context.Context, pool *pgxpool.Pool, directory string) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name text PRIMARY KEY,
			checksum_sha256 text,
			applied_at timestamptz NOT NULL DEFAULT now()
		);
		ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum_sha256 text`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read migrations directory: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".sql" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		script, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		digest := sha256.Sum256(script)
		checksum := hex.EncodeToString(digest[:])
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(869643760534061)`); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("lock migration %s: %w", name, err)
		}
		var storedChecksum string
		err = tx.QueryRow(ctx, `SELECT COALESCE(checksum_sha256,'') FROM schema_migrations WHERE name=$1`, name).Scan(&storedChecksum)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if err == nil {
			if storedChecksum == "" {
				if _, err := tx.Exec(ctx, `UPDATE schema_migrations SET checksum_sha256=$1 WHERE name=$2`, checksum, name); err != nil {
					_ = tx.Rollback(ctx)
					return fmt.Errorf("backfill checksum for migration %s: %w", name, err)
				}
			} else if storedChecksum != checksum {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("migration %s checksum mismatch: applied history is immutable", name)
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit skipped migration %s: %w", name, err)
			}
			continue
		}

		if _, err := tx.Exec(ctx, string(script)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name, checksum_sha256) VALUES ($1,$2)`, name, checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}
