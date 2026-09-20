package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"trajectory.local/api/internal/domain"
)

var (
	ErrReleaseEligibility = errors.New("release sessions must be accepted, valid, reviewed, and belong to the project")
	ErrReleaseConflict    = errors.New("release name already exists with different content")
)

type ReleaseCandidate struct {
	SessionID, NormalizedKey string
	Review                   domain.Review
}

func (store *Store) ListReleaseCandidates(ctx context.Context, projectID string, sessionIDs []string) ([]ReleaseCandidate, error) {
	var projectExists bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1)`, projectID).Scan(&projectExists); err != nil {
		return nil, err
	}
	if !projectExists {
		return nil, ErrNotFound
	}
	rows, err := store.pool.Query(ctx, `
		SELECT s.id, vr.normalized_key,
		       r.id, r.reviewer_id, r.rubric_version, r.scores, r.comments, r.decision, r.pii_review, r.created_at
		FROM sessions s
		JOIN assignments a ON a.id=s.assignment_id
		JOIN tasks t ON t.id=a.task_id
		JOIN task_templates tt ON tt.id=t.template_id
		JOIN validation_results vr ON vr.session_id=s.id AND vr.valid
		JOIN LATERAL (
			SELECT * FROM reviews WHERE session_id=s.id ORDER BY created_at DESC, id DESC LIMIT 1
		) r ON true
		WHERE tt.project_id=$1 AND s.state='ACCEPTED' AND r.decision='accepted' AND r.pii_review='passed'
		  AND NOT EXISTS (
			SELECT 1 FROM deletion_requests d
			WHERE d.session_id=s.id AND d.state IN ('queued','leased','blocked')
		  )
		  AND (cardinality($2::text[]) = 0 OR s.id=ANY($2::text[]))
		ORDER BY s.id`, projectID, sessionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]ReleaseCandidate, 0)
	for rows.Next() {
		var candidate ReleaseCandidate
		candidate.Review.SessionID = candidate.SessionID
		if err := rows.Scan(
			&candidate.SessionID, &candidate.NormalizedKey,
			&candidate.Review.ID, &candidate.Review.ReviewerID, &candidate.Review.RubricVersion,
			&candidate.Review.Scores, &candidate.Review.Comments, &candidate.Review.Decision,
			&candidate.Review.PIIReview, &candidate.Review.CreatedAt,
		); err != nil {
			return nil, err
		}
		candidate.Review.SessionID = candidate.SessionID
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sessionIDs) > 0 && len(candidates) != len(sessionIDs) {
		return nil, ErrReleaseEligibility
	}
	if len(candidates) == 0 {
		return nil, ErrReleaseEligibility
	}
	return candidates, nil
}

func (store *Store) CommitRelease(ctx context.Context, release domain.DatasetRelease, actor, requestID string, now time.Time) (domain.DatasetRelease, error) {
	if release.ID == "" || release.ProjectID == "" || release.Name == "" || release.SchemaVersion == "" || release.ExporterVersion == "" || release.PipelineVersion == "" || len(release.SourceSessionIDs) == 0 {
		return domain.DatasetRelease{}, errors.New("release metadata and source sessions are required")
	}
	if release.Profile != "trajectory_only" && release.Profile != "redacted_video" {
		return domain.DatasetRelease{}, errors.New("release profile must be trajectory_only or redacted_video")
	}
	if len(release.ConfigurationHash) != 64 || len(release.ManifestHash) != 64 || len(release.BundleHash) != 64 || release.BundleSize < 1 || !validReleaseObjectKeys(release.ID, release.ObjectKeys) {
		return domain.DatasetRelease{}, errors.New("release configuration, manifest, and bundle integrity metadata are required")
	}
	release.CreatedAt = now.UTC()
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return domain.DatasetRelease{}, err
	}
	defer tx.Rollback(ctx)
	var existing domain.DatasetRelease
	var existingSources, existingObjectKeys []byte
	err = tx.QueryRow(ctx, `
		SELECT id, project_id, name, release_profile, schema_version, exporter_version, pipeline_version,
		       source_session_ids, configuration_hash, manifest_hash,
		       COALESCE(bundle_hash,''), COALESCE(bundle_size,0), created_at,
		       COALESCE(purged_at,'epoch'::timestamptz), object_keys
		FROM dataset_releases WHERE project_id=$1 AND name=$2 FOR UPDATE`, release.ProjectID, release.Name).
		Scan(&existing.ID, &existing.ProjectID, &existing.Name, &existing.Profile, &existing.SchemaVersion, &existing.ExporterVersion,
			&existing.PipelineVersion, &existingSources, &existing.ConfigurationHash, &existing.ManifestHash,
			&existing.BundleHash, &existing.BundleSize, &existing.CreatedAt, &existing.PurgedAt, &existingObjectKeys)
	if err == nil {
		if unmarshalErr := json.Unmarshal(existingSources, &existing.SourceSessionIDs); unmarshalErr != nil {
			return domain.DatasetRelease{}, unmarshalErr
		}
		if unmarshalErr := json.Unmarshal(existingObjectKeys, &existing.ObjectKeys); unmarshalErr != nil {
			return domain.DatasetRelease{}, unmarshalErr
		}
		if existing.ID != release.ID || existing.ConfigurationHash != release.ConfigurationHash || existing.ManifestHash != release.ManifestHash {
			return domain.DatasetRelease{}, ErrReleaseConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.DatasetRelease{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.DatasetRelease{}, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id, assignment_id, state, updated_at FROM sessions
		WHERE id=ANY($1::text[]) ORDER BY id FOR UPDATE`, release.SourceSessionIDs)
	if err != nil {
		return domain.DatasetRelease{}, err
	}
	sessions := make([]domain.Session, 0, len(release.SourceSessionIDs))
	for rows.Next() {
		var session domain.Session
		if err := rows.Scan(&session.ID, &session.AssignmentID, &session.State, &session.UpdatedAt); err != nil {
			rows.Close()
			return domain.DatasetRelease{}, err
		}
		sessions = append(sessions, session)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return domain.DatasetRelease{}, err
	}
	if len(sessions) != len(release.SourceSessionIDs) {
		return domain.DatasetRelease{}, ErrReleaseEligibility
	}
	for _, session := range sessions {
		if session.State != domain.SessionAccepted {
			return domain.DatasetRelease{}, ErrReleaseEligibility
		}
	}
	var deletionPending bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM deletion_requests
			WHERE session_id=ANY($1::text[]) AND state IN ('queued','leased','blocked')
		)`, release.SourceSessionIDs).Scan(&deletionPending); err != nil {
		return domain.DatasetRelease{}, err
	}
	if deletionPending {
		return domain.DatasetRelease{}, fmt.Errorf("%w: a source session has an active deletion request", ErrReleaseEligibility)
	}
	sources, err := json.Marshal(release.SourceSessionIDs)
	if err != nil {
		return domain.DatasetRelease{}, err
	}
	objectKeys, err := json.Marshal(release.ObjectKeys)
	if err != nil {
		return domain.DatasetRelease{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO dataset_releases (
			id, project_id, name, release_profile, schema_version, exporter_version, pipeline_version,
			source_session_ids, configuration_hash, manifest_hash, bundle_hash, bundle_size, created_at, object_keys
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		release.ID, release.ProjectID, release.Name, release.Profile, release.SchemaVersion, release.ExporterVersion,
		release.PipelineVersion, string(sources), release.ConfigurationHash, release.ManifestHash,
		release.BundleHash, release.BundleSize, release.CreatedAt, objectKeys); err != nil {
		return domain.DatasetRelease{}, err
	}
	for index := range sessions {
		session := &sessions[index]
		if _, err := tx.Exec(ctx, `INSERT INTO release_sessions (release_id, session_id, ordinal) VALUES ($1,$2,$3)`, release.ID, session.ID, index); err != nil {
			return domain.DatasetRelease{}, err
		}
		if err := session.Transition(domain.TransitionCommand{To: domain.SessionReleased, Actor: actor, RequestID: requestID, Timestamp: now}); err != nil {
			return domain.DatasetRelease{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET state=$1, updated_at=$2 WHERE id=$3`, session.State, session.UpdatedAt, session.ID); err != nil {
			return domain.DatasetRelease{}, err
		}
		if err := insertAudit(ctx, tx, session.AuditEvents[len(session.AuditEvents)-1]); err != nil {
			return domain.DatasetRelease{}, err
		}
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		Actor: actor, Action: "dataset_release.created", Resource: "dataset-release/" + release.ID,
		Timestamp: now.UTC(), RequestID: requestID,
		Metadata: map[string]string{"project_id": release.ProjectID, "name": release.Name, "profile": release.Profile, "manifest_hash": release.ManifestHash, "session_count": fmt.Sprint(len(sessions))},
	}); err != nil {
		return domain.DatasetRelease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.DatasetRelease{}, err
	}
	return release, nil
}

func validReleaseObjectKeys(releaseID string, keys []string) bool {
	if releaseID == "" || len(keys) < 7 {
		return false
	}
	prefix := "releases/" + releaseID + "/"
	bundle := "bundles/" + releaseID + ".zip"
	seenManifest, seenBundle := false, false
	for index, key := range keys {
		if key == "" || strings.Contains(key, "\\") || strings.Contains(key, "../") ||
			(index > 0 && keys[index-1] >= key) || (key != bundle && !strings.HasPrefix(key, prefix)) {
			return false
		}
		seenManifest = seenManifest || key == prefix+"manifest.json"
		seenBundle = seenBundle || key == bundle
	}
	return seenManifest && seenBundle
}

func (store *Store) GetRelease(ctx context.Context, releaseID string) (domain.DatasetRelease, error) {
	var release domain.DatasetRelease
	var sources, objectKeys []byte
	err := store.pool.QueryRow(ctx, `
		SELECT id, project_id, name, release_profile, schema_version, exporter_version, pipeline_version,
		       source_session_ids, configuration_hash, manifest_hash,
		       COALESCE(bundle_hash,''), COALESCE(bundle_size,0), created_at,
		       COALESCE(purged_at,'epoch'::timestamptz), object_keys
		FROM dataset_releases WHERE id=$1`, releaseID).
		Scan(&release.ID, &release.ProjectID, &release.Name, &release.Profile, &release.SchemaVersion, &release.ExporterVersion,
			&release.PipelineVersion, &sources, &release.ConfigurationHash, &release.ManifestHash,
			&release.BundleHash, &release.BundleSize, &release.CreatedAt, &release.PurgedAt, &objectKeys)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DatasetRelease{}, ErrNotFound
	}
	if err != nil {
		return domain.DatasetRelease{}, err
	}
	if err := json.Unmarshal(sources, &release.SourceSessionIDs); err != nil {
		return domain.DatasetRelease{}, err
	}
	if err := json.Unmarshal(objectKeys, &release.ObjectKeys); err != nil {
		return domain.DatasetRelease{}, err
	}
	return release, nil
}

func (store *Store) GetReleaseByName(ctx context.Context, projectID, name string) (domain.DatasetRelease, error) {
	var release domain.DatasetRelease
	var sources, objectKeys []byte
	err := store.pool.QueryRow(ctx, `
		SELECT id, project_id, name, release_profile, schema_version, exporter_version, pipeline_version,
		       source_session_ids, configuration_hash, manifest_hash,
		       COALESCE(bundle_hash,''), COALESCE(bundle_size,0), created_at,
		       COALESCE(purged_at,'epoch'::timestamptz), object_keys
		FROM dataset_releases WHERE project_id=$1 AND name=$2`, projectID, name).
		Scan(&release.ID, &release.ProjectID, &release.Name, &release.Profile, &release.SchemaVersion, &release.ExporterVersion,
			&release.PipelineVersion, &sources, &release.ConfigurationHash, &release.ManifestHash,
			&release.BundleHash, &release.BundleSize, &release.CreatedAt, &release.PurgedAt, &objectKeys)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DatasetRelease{}, ErrNotFound
	}
	if err != nil {
		return domain.DatasetRelease{}, err
	}
	if err := json.Unmarshal(sources, &release.SourceSessionIDs); err != nil {
		return domain.DatasetRelease{}, err
	}
	if err := json.Unmarshal(objectKeys, &release.ObjectKeys); err != nil {
		return domain.DatasetRelease{}, err
	}
	return release, nil
}
