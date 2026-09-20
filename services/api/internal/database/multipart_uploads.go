package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

type MultipartUpload struct {
	ID              string
	SessionID       string
	Key             string
	StorageKey      string
	StorageUploadID string
	SHA256          string
	Size            int64
	MediaType       string
	PartSize        int64
	State           string
	ExpiresAt       time.Time
}

type MultipartUploadPart struct {
	Number int32  `json:"part_number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size_bytes"`
}

func (store *Store) FindMultipartUpload(ctx context.Context, sessionID, key string) (MultipartUpload, error) {
	return scanMultipartUpload(store.pool.QueryRow(ctx, `
		SELECT id, session_id, logical_key, storage_key, storage_upload_id, sha256, size_bytes,
		       media_type, part_size_bytes, state, expires_at
		FROM artifact_multipart_uploads WHERE session_id=$1 AND logical_key=$2`, sessionID, key))
}

func (store *Store) GetMultipartUpload(ctx context.Context, sessionID, uploadID string) (MultipartUpload, error) {
	return scanMultipartUpload(store.pool.QueryRow(ctx, `
		SELECT id, session_id, logical_key, storage_key, storage_upload_id, sha256, size_bytes,
		       media_type, part_size_bytes, state, expires_at
		FROM artifact_multipart_uploads WHERE session_id=$1 AND id=$2`, sessionID, uploadID))
}

func scanMultipartUpload(row pgx.Row) (MultipartUpload, error) {
	var upload MultipartUpload
	err := row.Scan(&upload.ID, &upload.SessionID, &upload.Key, &upload.StorageKey, &upload.StorageUploadID,
		&upload.SHA256, &upload.Size, &upload.MediaType, &upload.PartSize, &upload.State, &upload.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MultipartUpload{}, ErrNotFound
	}
	return upload, err
}

func (store *Store) CreateMultipartUpload(ctx context.Context, upload MultipartUpload, actor, requestID string, now time.Time) (MultipartUpload, bool, error) {
	identifier, err := newID("upload")
	if err != nil {
		return MultipartUpload{}, false, err
	}
	upload.ID = identifier
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return MultipartUpload{}, false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		INSERT INTO artifact_multipart_uploads
		  (id, session_id, logical_key, storage_key, storage_upload_id, sha256, size_bytes, media_type,
		   part_size_bytes, state, expires_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'active',$10,$11,$11)
		ON CONFLICT (logical_key) DO NOTHING`,
		upload.ID, upload.SessionID, upload.Key, upload.StorageKey, upload.StorageUploadID, upload.SHA256,
		upload.Size, upload.MediaType, upload.PartSize, upload.ExpiresAt.UTC(), now.UTC())
	if err != nil {
		return MultipartUpload{}, false, err
	}
	if tag.RowsAffected() == 0 {
		existing, err := scanMultipartUpload(tx.QueryRow(ctx, `
			SELECT id, session_id, logical_key, storage_key, storage_upload_id, sha256, size_bytes,
			       media_type, part_size_bytes, state, expires_at
			FROM artifact_multipart_uploads WHERE logical_key=$1`, upload.Key))
		if err != nil {
			return MultipartUpload{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return MultipartUpload{}, false, err
		}
		return existing, false, nil
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "artifact.multipart_started", Resource: "multipart-upload/" + upload.ID,
		RequestID: requestID, Timestamp: now.UTC(),
		Metadata: map[string]string{"session_id": upload.SessionID, "logical_key": upload.Key, "sha256": upload.SHA256},
	}); err != nil {
		return MultipartUpload{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MultipartUpload{}, false, err
	}
	upload.State = "active"
	return upload, true, nil
}

func (store *Store) RestartMultipartUpload(ctx context.Context, upload MultipartUpload, actor, requestID string, now time.Time) (MultipartUpload, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return MultipartUpload{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM artifact_multipart_parts WHERE upload_id=$1`, upload.ID); err != nil {
		return MultipartUpload{}, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE artifact_multipart_uploads SET storage_key=$2, storage_upload_id=$3, sha256=$4,
		  size_bytes=$5, media_type=$6, part_size_bytes=$7, state='active', expires_at=$8, updated_at=$9
		WHERE id=$1 AND state='aborted'`, upload.ID, upload.StorageKey, upload.StorageUploadID,
		upload.SHA256, upload.Size, upload.MediaType, upload.PartSize, upload.ExpiresAt.UTC(), now.UTC())
	if err != nil {
		return MultipartUpload{}, err
	}
	if tag.RowsAffected() == 0 {
		return MultipartUpload{}, fmt.Errorf("%w: multipart upload cannot be restarted", ErrNotFound)
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "artifact.multipart_restarted", Resource: "multipart-upload/" + upload.ID,
		RequestID: requestID, Timestamp: now.UTC(),
		Metadata: map[string]string{"session_id": upload.SessionID, "logical_key": upload.Key, "sha256": upload.SHA256},
	}); err != nil {
		return MultipartUpload{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MultipartUpload{}, err
	}
	upload.State = "active"
	return upload, nil
}

func (store *Store) ListMultipartUploadParts(ctx context.Context, uploadID string) ([]MultipartUploadPart, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT part_number, etag, size_bytes FROM artifact_multipart_parts
		WHERE upload_id=$1 ORDER BY part_number`, uploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parts := make([]MultipartUploadPart, 0)
	for rows.Next() {
		var part MultipartUploadPart
		if err := rows.Scan(&part.Number, &part.ETag, &part.Size); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, rows.Err()
}

func (store *Store) RecordMultipartUploadPart(ctx context.Context, uploadID string, part MultipartUploadPart, now time.Time) error {
	tag, err := store.pool.Exec(ctx, `
		INSERT INTO artifact_multipart_parts (upload_id, part_number, etag, size_bytes, created_at)
		SELECT id, $2, $3, $4, $5 FROM artifact_multipart_uploads
		WHERE id=$1 AND state='active' AND expires_at>$5
		ON CONFLICT (upload_id, part_number)
		DO UPDATE SET etag=EXCLUDED.etag, size_bytes=EXCLUDED.size_bytes, created_at=EXCLUDED.created_at`,
		uploadID, part.Number, part.ETag, part.Size, now.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: multipart upload is not active", ErrNotFound)
	}
	return nil
}

func (store *Store) SetMultipartUploadState(ctx context.Context, uploadID, from, to string, now time.Time) error {
	tag, err := store.pool.Exec(ctx, `
		UPDATE artifact_multipart_uploads SET state=$3, updated_at=$4 WHERE id=$1 AND state=$2`,
		uploadID, from, to, now.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: multipart upload is not %s", ErrNotFound, from)
	}
	return nil
}
