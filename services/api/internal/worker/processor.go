package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
	"trajectory.local/api/internal/domain"
)

type Processor struct {
	store                  *database.Store
	blobs                  blobstore.BlobStore
	validator              *Validator
	mediaInspector         MediaInspector
	videoRedactor          VideoRedactor
	lease                  time.Duration
	maxAttempts            int
	now                    func() time.Time
	nextRetentionSweep     time.Time
	retentionSweepInterval time.Duration
}

func (processor *Processor) WithVideoRedaction(inspector MediaInspector, redactor VideoRedactor) (*Processor, error) {
	if processor == nil || inspector == nil || redactor == nil {
		return nil, errors.New("processor, media inspector, and video redactor are required")
	}
	processor.mediaInspector = inspector
	processor.videoRedactor = redactor
	return processor, nil
}

func NewProcessor(store *database.Store, blobs blobstore.BlobStore, validator *Validator) (*Processor, error) {
	if store == nil || blobs == nil || validator == nil {
		return nil, errors.New("database, blob store, and validator are required")
	}
	return &Processor{
		store: store, blobs: blobs, validator: validator,
		lease: 2 * time.Minute, maxAttempts: 3, now: time.Now,
		retentionSweepInterval: 15 * time.Minute,
	}, nil
}

// ProcessNext leases and handles at most one job. The boolean reports whether a
// job was claimed, allowing callers to back off when the queue is empty.
func (processor *Processor) ProcessNext(ctx context.Context, workerID string) (bool, error) {
	job, err := processor.store.ClaimValidationJob(ctx, workerID, processor.lease, processor.now())
	if errors.Is(err, database.ErrNoJobAvailable) {
		if processor.mediaInspector != nil && processor.videoRedactor != nil {
			processed, redactionErr := processor.processRedaction(ctx, workerID)
			if redactionErr != nil || processed {
				return processed, redactionErr
			}
		}
		processed, deletionErr := processor.processDeletion(ctx, workerID)
		if deletionErr != nil || processed {
			return processed, deletionErr
		}
		return processor.processRetention(ctx, workerID)
	}
	if err != nil {
		return false, fmt.Errorf("claim validation job: %w", err)
	}
	artifacts, err := processor.store.ListArtifacts(ctx, job.SessionID)
	if err != nil {
		return true, processor.retry(ctx, job, workerID, fmt.Errorf("list artifacts: %w", err))
	}
	normalized, problems, err := processor.validator.Validate(ctx, job.SessionID, artifacts)
	if err != nil {
		return true, processor.retry(ctx, job, workerID, err)
	}
	result := domain.ValidationResult{
		Valid:            len(problems) == 0,
		Errors:           problems,
		SchemaVersion:    SchemaVersion,
		ValidatorVersion: ValidatorVersion,
	}
	if result.Valid {
		result.NormalizedKey = "derived/sessions/" + job.SessionID + "/trajectory.json"
		if _, err := processor.blobs.Put(ctx, result.NormalizedKey, bytes.NewReader(normalized)); err != nil {
			return true, processor.retry(ctx, job, workerID, fmt.Errorf("write normalized trajectory: %w", err))
		}
		actions, err := actionsJSONL(normalized)
		if err != nil {
			return true, processor.retry(ctx, job, workerID, fmt.Errorf("encode normalized actions: %w", err))
		}
		if _, err := processor.blobs.Put(ctx, "derived/sessions/"+job.SessionID+"/actions.jsonl", bytes.NewReader(actions)); err != nil {
			return true, processor.retry(ctx, job, workerID, fmt.Errorf("write normalized actions: %w", err))
		}
	}
	if _, err := processor.store.CompleteValidationJob(ctx, job.ID, workerID, result, processor.now()); err != nil {
		return true, fmt.Errorf("complete validation job: %w", err)
	}
	return true, nil
}

type redactionOutput struct {
	SegmentID      string  `json:"segment_id"`
	SourceKey      string  `json:"source_key"`
	Decision       string  `json:"decision"`
	OutputKey      string  `json:"output_key"`
	SHA256         string  `json:"sha256"`
	Size           int64   `json:"size"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	DurationSecond float64 `json:"duration_seconds"`
}

type redactionOutputManifest struct {
	SchemaVersion string            `json:"schema_version"`
	SessionID     string            `json:"session_id"`
	PlanID        string            `json:"plan_id"`
	PlanVersion   int               `json:"plan_version"`
	PlanHash      string            `json:"plan_hash"`
	Outputs       []redactionOutput `json:"outputs"`
}

func (processor *Processor) processRedaction(ctx context.Context, workerID string) (bool, error) {
	job, err := processor.store.ClaimRedactionJob(ctx, workerID, processor.lease, processor.now())
	if errors.Is(err, database.ErrNoRedactionJob) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim redaction job: %w", err)
	}
	plan, err := processor.store.GetRedactionPlan(ctx, job.PlanID)
	if err != nil {
		return true, processor.retryRedaction(ctx, job, workerID, fmt.Errorf("load redaction plan: %w", err))
	}
	outputs := make([]redactionOutput, 0, len(plan.Document.Segments))
	for _, segment := range plan.Document.Segments {
		media, err := processor.inspectRedactionSource(ctx, segment.SourceKey)
		if err != nil {
			return true, processor.retryRedaction(ctx, job, workerID, fmt.Errorf("inspect %s: %w", segment.SegmentID, err))
		}
		maximumNS := int64(math.Ceil(media.DurationSeconds * 1e9))
		for _, region := range segment.Regions {
			if region.StartNS >= maximumNS {
				return true, processor.retryRedaction(ctx, job, workerID, fmt.Errorf("segment %s region starts after media duration", segment.SegmentID))
			}
		}
		outputKey := "derived/sessions/" + plan.SessionID + "/redactions/" + plan.ID + "/" + segment.SegmentID + ".mp4"
		info, err := processor.writeRedactedSegment(ctx, segment, outputKey)
		if err != nil {
			return true, processor.retryRedaction(ctx, job, workerID, fmt.Errorf("write %s: %w", segment.SegmentID, err))
		}
		outputs = append(outputs, redactionOutput{
			SegmentID: segment.SegmentID, SourceKey: segment.SourceKey, Decision: segment.Decision,
			OutputKey: outputKey, SHA256: info.SHA256, Size: info.Size,
			Width: media.Width, Height: media.Height, DurationSecond: media.DurationSeconds,
		})
	}
	sort.Slice(outputs, func(left, right int) bool { return outputs[left].SegmentID < outputs[right].SegmentID })
	manifest := redactionOutputManifest{
		SchemaVersion: "redaction-output/v1", SessionID: plan.SessionID, PlanID: plan.ID,
		PlanVersion: plan.Version, PlanHash: plan.DocumentHash, Outputs: outputs,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return true, processor.retryRedaction(ctx, job, workerID, fmt.Errorf("encode redaction manifest: %w", err))
	}
	manifestBytes = append(manifestBytes, '\n')
	manifestKey := "derived/sessions/" + plan.SessionID + "/redactions/" + plan.ID + "/manifest.json"
	if _, err := processor.blobs.Put(ctx, manifestKey, bytes.NewReader(manifestBytes)); err != nil {
		return true, processor.retryRedaction(ctx, job, workerID, fmt.Errorf("write redaction manifest: %w", err))
	}
	if err := processor.store.CompleteRedactionJob(ctx, job.ID, workerID, manifestKey, processor.now()); err != nil {
		return true, processor.retryRedaction(ctx, job, workerID, fmt.Errorf("complete redaction job: %w", err))
	}
	return true, nil
}

func (processor *Processor) inspectRedactionSource(ctx context.Context, key string) (MediaInfo, error) {
	input, err := processor.blobs.Get(ctx, key)
	if err != nil {
		return MediaInfo{}, err
	}
	defer input.Close()
	return processor.mediaInspector.InspectMP4(ctx, input)
}

func (processor *Processor) writeRedactedSegment(ctx context.Context, segment domain.RedactionSegment, outputKey string) (blobstore.BlobInfo, error) {
	input, err := processor.blobs.Get(ctx, segment.SourceKey)
	if err != nil {
		return blobstore.BlobInfo{}, err
	}
	defer input.Close()
	pipeReader, pipeWriter := io.Pipe()
	redacted := make(chan error, 1)
	go func() {
		err := processor.videoRedactor.Redact(ctx, input, pipeWriter, segment.Regions)
		_ = pipeWriter.CloseWithError(err)
		redacted <- err
	}()
	info, putErr := processor.blobs.Put(ctx, outputKey, pipeReader)
	if putErr != nil {
		_ = pipeReader.CloseWithError(putErr)
	}
	redactionErr := <-redacted
	if putErr != nil {
		return blobstore.BlobInfo{}, putErr
	}
	if redactionErr != nil {
		return blobstore.BlobInfo{}, redactionErr
	}
	return info, nil
}

func (processor *Processor) retryRedaction(ctx context.Context, job domain.RedactionJob, workerID string, cause error) error {
	if err := processor.store.RequeueRedactionJob(ctx, job.ID, workerID, cause.Error(), processor.maxAttempts, processor.now()); err != nil {
		return fmt.Errorf("%v; requeue redaction job: %w", cause, err)
	}
	return cause
}

func (processor *Processor) processRetention(ctx context.Context, workerID string) (bool, error) {
	now := processor.now()
	if processor.nextRetentionSweep.IsZero() || !now.Before(processor.nextRetentionSweep) {
		if _, err := processor.store.QueueExpiredRetention(ctx, workerID, now, 100); err != nil {
			return false, fmt.Errorf("queue expired retention: %w", err)
		}
		processor.nextRetentionSweep = now.Add(processor.retentionSweepInterval)
	}
	request, err := processor.store.ClaimRetentionPurge(ctx, workerID, processor.lease, now)
	if errors.Is(err, database.ErrNoRetentionPurge) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim retention purge: %w", err)
	}
	if request.State != "leased" {
		return true, nil
	}
	for _, key := range request.ObjectKeys {
		if err := processor.blobs.Purge(ctx, key); err != nil {
			return true, processor.retryRetention(ctx, request, workerID, fmt.Errorf("purge %s: %w", key, err))
		}
	}
	if err := processor.store.CompleteRetentionPurge(ctx, request.ID, workerID, processor.now()); err != nil {
		return true, processor.retryRetention(ctx, request, workerID, fmt.Errorf("complete retention purge: %w", err))
	}
	return true, nil
}

func (processor *Processor) retryRetention(ctx context.Context, request domain.RetentionPurgeRequest, workerID string, cause error) error {
	if err := processor.store.RequeueRetentionPurge(ctx, request.ID, workerID, cause.Error(), processor.maxAttempts, processor.now()); err != nil {
		return fmt.Errorf("%v; requeue retention purge: %w", cause, err)
	}
	return cause
}

func (processor *Processor) processDeletion(ctx context.Context, workerID string) (bool, error) {
	request, err := processor.store.ClaimDeletionRequest(ctx, workerID, processor.lease, processor.now())
	if errors.Is(err, database.ErrNoDeletionRequest) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim deletion request: %w", err)
	}
	if request.State != "leased" {
		return true, nil
	}
	for _, key := range request.ObjectKeys {
		if err := processor.blobs.Purge(ctx, key); err != nil {
			return true, processor.retryDeletion(ctx, request, workerID, fmt.Errorf("purge %s: %w", key, err))
		}
	}
	if err := processor.store.CompleteDeletionRequest(ctx, request.ID, workerID, processor.now()); err != nil {
		return true, processor.retryDeletion(ctx, request, workerID, fmt.Errorf("complete erasure: %w", err))
	}
	return true, nil
}

func (processor *Processor) retryDeletion(ctx context.Context, request domain.DeletionRequest, workerID string, cause error) error {
	if err := processor.store.RequeueDeletionRequest(ctx, request.ID, workerID, cause.Error(), processor.maxAttempts, processor.now()); err != nil {
		return fmt.Errorf("%v; requeue deletion request: %w", cause, err)
	}
	return cause
}

func (processor *Processor) retry(ctx context.Context, job domain.ProcessingJob, workerID string, cause error) error {
	if err := processor.store.RequeueValidationJob(ctx, job.ID, workerID, cause.Error(), processor.maxAttempts, processor.now()); err != nil {
		return fmt.Errorf("%v; requeue validation job: %w", cause, err)
	}
	return cause
}
