package blobstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type fakeR2Client struct {
	put               func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error)
	get               func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error)
	head              func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	delete            func(context.Context, *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	copy              func(context.Context, *s3.CopyObjectInput) (*s3.CopyObjectOutput, error)
	createMultipart   func(context.Context, *s3.CreateMultipartUploadInput) (*s3.CreateMultipartUploadOutput, error)
	completeMultipart func(context.Context, *s3.CompleteMultipartUploadInput) (*s3.CompleteMultipartUploadOutput, error)
	abortMultipart    func(context.Context, *s3.AbortMultipartUploadInput) (*s3.AbortMultipartUploadOutput, error)
}

func (client *fakeR2Client) PutObject(ctx context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return client.put(ctx, input)
}

func (client *fakeR2Client) GetObject(ctx context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return client.get(ctx, input)
}

func (client *fakeR2Client) HeadObject(ctx context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return client.head(ctx, input)
}

func (client *fakeR2Client) DeleteObject(ctx context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return client.delete(ctx, input)
}

func (client *fakeR2Client) CopyObject(ctx context.Context, input *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	return client.copy(ctx, input)
}

func (client *fakeR2Client) CreateMultipartUpload(ctx context.Context, input *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	return client.createMultipart(ctx, input)
}

func (client *fakeR2Client) CompleteMultipartUpload(ctx context.Context, input *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	return client.completeMultipart(ctx, input)
}

func (client *fakeR2Client) AbortMultipartUpload(ctx context.Context, input *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	return client.abortMultipart(ctx, input)
}

func TestValidateR2Config(t *testing.T) {
	valid := R2Config{Endpoint: "https://account.r2.cloudflarestorage.com/", Bucket: "artifacts", AccessKeyID: "id", SecretKey: "secret"}
	endpoint, err := validateR2Config(valid)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://account.r2.cloudflarestorage.com" {
		t.Fatalf("unexpected normalized endpoint %q", endpoint)
	}

	for _, endpoint := range []string{
		"http://account.r2.cloudflarestorage.com",
		"https://user:secret@account.r2.cloudflarestorage.com",
		"https://account.r2.cloudflarestorage.com/path",
		"https://account.r2.cloudflarestorage.com?token=secret",
	} {
		invalid := valid
		invalid.Endpoint = endpoint
		if _, err := validateR2Config(invalid); err == nil {
			t.Fatalf("expected endpoint %q to be rejected", endpoint)
		}
	}
}

func TestR2PutHashesAndProtectsRawObjects(t *testing.T) {
	var captured *s3.PutObjectInput
	client := &fakeR2Client{}
	client.put = func(_ context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
		captured = input
		if _, err := io.ReadAll(input.Body); err != nil {
			t.Fatal(err)
		}
		return &s3.PutObjectOutput{}, nil
	}
	store := &R2BlobStore{bucket: "artifacts", client: client}
	info, err := store.Put(context.Background(), "raw/session/video.mp4", strings.NewReader("capture"))
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(captured.Bucket) != "artifacts" || aws.ToString(captured.Key) != "raw/session/video.mp4" {
		t.Fatalf("unexpected R2 destination: %#v", captured)
	}
	if aws.ToString(captured.IfNoneMatch) != "*" {
		t.Fatal("raw writes must use an atomic create-only precondition")
	}
	if info.Size != 7 || info.SHA256 != "460ee6aa3a80359181b794cc31a7185addba77626e9f719c10e3c8efb8668a1d" {
		t.Fatalf("unexpected blob info: %#v", info)
	}

	client.put = func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
		return nil, responseError(http.StatusPreconditionFailed)
	}
	_, err = store.Put(context.Background(), "raw/session/video.mp4", strings.NewReader("replacement"))
	if !errors.Is(err, ErrImmutableRaw) {
		t.Fatalf("expected immutable raw error, got %v", err)
	}
}

func TestR2StatVerifiesObjectContent(t *testing.T) {
	modified := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	client := &fakeR2Client{
		head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
			return &s3.HeadObjectOutput{ContentLength: aws.Int64(7), LastModified: &modified}, nil
		},
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("capture"))}, nil
		},
	}
	store := &R2BlobStore{bucket: "artifacts", client: client}
	info, err := store.Stat(context.Background(), "raw/session/video.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 7 || info.SHA256 != "460ee6aa3a80359181b794cc31a7185addba77626e9f719c10e3c8efb8668a1d" || !info.LastModified.Equal(modified) {
		t.Fatalf("unexpected stat result: %#v", info)
	}
}

func TestR2ExistsMapsOnlyNotFoundToFalse(t *testing.T) {
	client := &fakeR2Client{
		head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
			return nil, responseError(http.StatusNotFound)
		},
	}
	store := &R2BlobStore{bucket: "artifacts", client: client}
	exists, err := store.Exists(context.Background(), "derived/session/export.zip")
	if err != nil || exists {
		t.Fatalf("expected a clean not-found result, got exists=%v err=%v", exists, err)
	}

	client.head = func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		return nil, responseError(http.StatusForbidden)
	}
	if _, err := store.Exists(context.Background(), "derived/session/export.zip"); err == nil {
		t.Fatal("permission errors must not be treated as missing objects")
	}
}

func TestR2DeleteRefusesRawObjects(t *testing.T) {
	called := false
	client := &fakeR2Client{
		delete: func(context.Context, *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
			called = true
			return &s3.DeleteObjectOutput{}, nil
		},
	}
	store := &R2BlobStore{bucket: "artifacts", client: client}
	if err := store.Delete(context.Background(), "raw/session/video.mp4"); !errors.Is(err, ErrImmutableRaw) {
		t.Fatalf("expected immutable raw error, got %v", err)
	}
	if called {
		t.Fatal("raw deletion must be rejected before contacting R2")
	}
}

func TestR2PurgeIsTheExplicitRawErasureBoundary(t *testing.T) {
	var deleted string
	client := &fakeR2Client{
		delete: func(_ context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
			deleted = aws.ToString(input.Key)
			return &s3.DeleteObjectOutput{}, nil
		},
	}
	store := &R2BlobStore{bucket: "artifacts", client: client}
	key := "raw/sessions/sess_erasure/video/000001.mp4"
	if err := store.Purge(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if deleted != key {
		t.Fatalf("purged key = %q", deleted)
	}
}

func TestR2MultipartLifecycleProtectsRawObject(t *testing.T) {
	var completed *s3.CompleteMultipartUploadInput
	aborted := false
	client := &fakeR2Client{
		createMultipart: func(_ context.Context, input *s3.CreateMultipartUploadInput) (*s3.CreateMultipartUploadOutput, error) {
			if aws.ToString(input.Key) != "raw/sessions/sess_1/video/000001.mp4" || input.Metadata["trajectory-sha256"] != strings.Repeat("a", 64) {
				t.Fatalf("unexpected multipart create input: %#v", input)
			}
			return &s3.CreateMultipartUploadOutput{UploadId: aws.String("r2-upload-1")}, nil
		},
		completeMultipart: func(_ context.Context, input *s3.CompleteMultipartUploadInput) (*s3.CompleteMultipartUploadOutput, error) {
			completed = input
			return &s3.CompleteMultipartUploadOutput{}, nil
		},
		abortMultipart: func(_ context.Context, input *s3.AbortMultipartUploadInput) (*s3.AbortMultipartUploadOutput, error) {
			aborted = aws.ToString(input.UploadId) == "r2-upload-1"
			return &s3.AbortMultipartUploadOutput{}, nil
		},
	}
	store := &R2BlobStore{bucket: "artifacts", client: client}
	uploadID, err := store.CreateMultipartUpload(context.Background(), "raw/sessions/sess_1/video/000001.mp4", "video/mp4", strings.Repeat("a", 64))
	if err != nil || uploadID != "r2-upload-1" {
		t.Fatalf("create multipart = %q, %v", uploadID, err)
	}
	parts := []MultipartPart{{Number: 1, ETag: `"etag-one"`}, {Number: 2, ETag: `"etag-two"`}}
	if err := store.CompleteMultipartUpload(context.Background(), "raw/sessions/sess_1/video/000001.mp4", uploadID, parts); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(completed.IfNoneMatch) != "*" || len(completed.MultipartUpload.Parts) != 2 {
		t.Fatalf("raw multipart completion is not create-only: %#v", completed)
	}
	if err := store.AbortMultipartUpload(context.Background(), "raw/sessions/sess_1/video/000001.mp4", uploadID); err != nil || !aborted {
		t.Fatalf("abort multipart = %v, called=%v", err, aborted)
	}
}

func TestR2MultipartPartAuthorizationIsScopedAndShortLived(t *testing.T) {
	store, err := NewR2(context.Background(), R2Config{
		Endpoint: "https://account.r2.cloudflarestorage.com", Bucket: "artifacts",
		AccessKeyID: "access-key", SecretKey: "secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := store.AuthorizeMultipartPart(
		context.Background(), "staging/multipart/stage_1", "storage-upload-1", 3, 16<<20, 10*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.Method != http.MethodPut || parsed.Scheme != "https" || parsed.Host != "account.r2.cloudflarestorage.com" {
		t.Fatalf("unexpected multipart part target: %#v", authorization)
	}
	if parsed.Path != "/artifacts/staging/multipart/stage_1" || parsed.Query().Get("partNumber") != "3" || parsed.Query().Get("uploadId") != "storage-upload-1" {
		t.Fatalf("authorization was not scoped to one upload part: %s", authorization.URL)
	}
	if authorization.ExpiresAt.Before(time.Now().UTC().Add(9 * time.Minute)) {
		t.Fatalf("authorization expiry is unexpectedly short: %s", authorization.ExpiresAt)
	}
}

func TestR2MultipartPromotionIsCreateOnlyAndRemovesStaging(t *testing.T) {
	var copied *s3.CopyObjectInput
	var deleted string
	client := &fakeR2Client{
		copy: func(_ context.Context, input *s3.CopyObjectInput) (*s3.CopyObjectOutput, error) {
			copied = input
			return &s3.CopyObjectOutput{}, nil
		},
		delete: func(_ context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
			deleted = aws.ToString(input.Key)
			return &s3.DeleteObjectOutput{}, nil
		},
	}
	store := &R2BlobStore{bucket: "artifacts", client: client}
	if err := store.PromoteMultipartUpload(context.Background(), "staging/multipart/stage_1", "raw/sessions/sess_1/video/000001.mp4"); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(copied.IfNoneMatch) != "*" || aws.ToString(copied.Key) != "raw/sessions/sess_1/video/000001.mp4" {
		t.Fatalf("multipart promotion was not scoped create-only: %#v", copied)
	}
	if !strings.Contains(aws.ToString(copied.CopySource), "staging%2Fmultipart%2Fstage_1") || deleted != "staging/multipart/stage_1" {
		t.Fatalf("unexpected promotion source/cleanup: source=%q deleted=%q", aws.ToString(copied.CopySource), deleted)
	}
}

func responseError(status int) error {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      errors.New(http.StatusText(status)),
	}
}
