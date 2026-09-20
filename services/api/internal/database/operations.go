package database

import (
	"context"
	"time"
)

type QueueSnapshot struct {
	ProcessingQueued, ProcessingLeased, ProcessingDeadLetter int64
	RedactionQueued, RedactionLeased, RedactionDeadLetter    int64
	DeletionQueued, DeletionLeased, DeletionBlocked          int64
	DeletionDeadLetter                                       int64
	RetentionQueued, RetentionLeased, RetentionBlocked       int64
	RetentionDeadLetter                                      int64
	OldestReadyAgeSeconds                                    int64
}

// GetQueueSnapshot returns aggregate operational state only. It deliberately
// excludes organization, contributor, task, and object identifiers so the
// values are safe for structured operational logs.
func (store *Store) GetQueueSnapshot(ctx context.Context, now time.Time) (QueueSnapshot, error) {
	var snapshot QueueSnapshot
	err := store.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM processing_jobs WHERE state='queued'),
		  (SELECT count(*) FROM processing_jobs WHERE state='leased'),
		  (SELECT count(*) FROM processing_jobs WHERE state='dead_letter'),
		  (SELECT count(*) FROM redaction_jobs WHERE state='queued'),
		  (SELECT count(*) FROM redaction_jobs WHERE state='leased'),
		  (SELECT count(*) FROM redaction_jobs WHERE state='dead_letter'),
		  (SELECT count(*) FROM deletion_requests WHERE state='queued'),
		  (SELECT count(*) FROM deletion_requests WHERE state='leased'),
		  (SELECT count(*) FROM deletion_requests WHERE state='blocked'),
		  (SELECT count(*) FROM deletion_requests WHERE state='dead_letter'),
		  (SELECT count(*) FROM retention_purge_requests WHERE state='queued'),
		  (SELECT count(*) FROM retention_purge_requests WHERE state='leased'),
		  (SELECT count(*) FROM retention_purge_requests WHERE state='blocked'),
		  (SELECT count(*) FROM retention_purge_requests WHERE state='dead_letter'),
		  GREATEST(0, COALESCE(EXTRACT(EPOCH FROM ($1::timestamptz - LEAST(
		    (SELECT min(available_at) FROM processing_jobs WHERE state='queued' AND available_at <= $1),
		    (SELECT min(available_at) FROM redaction_jobs WHERE state='queued' AND available_at <= $1),
		    (SELECT min(available_at) FROM deletion_requests WHERE state='queued' AND available_at <= $1),
		    (SELECT min(available_at) FROM retention_purge_requests WHERE state='queued' AND available_at <= $1)
		  )))::bigint, 0))`, now.UTC()).Scan(
		&snapshot.ProcessingQueued, &snapshot.ProcessingLeased, &snapshot.ProcessingDeadLetter,
		&snapshot.RedactionQueued, &snapshot.RedactionLeased, &snapshot.RedactionDeadLetter,
		&snapshot.DeletionQueued, &snapshot.DeletionLeased, &snapshot.DeletionBlocked,
		&snapshot.DeletionDeadLetter, &snapshot.RetentionQueued, &snapshot.RetentionLeased,
		&snapshot.RetentionBlocked, &snapshot.RetentionDeadLetter, &snapshot.OldestReadyAgeSeconds)
	return snapshot, err
}
