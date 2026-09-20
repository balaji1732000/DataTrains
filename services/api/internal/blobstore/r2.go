package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type R2Config struct {
	Endpoint    string
	Bucket      string
	AccessKeyID string
	SecretKey   string
}

type r2Client interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	CopyObject(context.Context, *s3.CopyObjectInput, ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

type R2BlobStore struct {
	bucket    string
	client    r2Client
	presigner *s3.PresignClient
}

var _ MultipartUploadAuthorizer = (*R2BlobStore)(nil)

func NewR2(ctx context.Context, config R2Config) (*R2BlobStore, error) {
	endpoint, err := validateR2Config(config)
	if err != nil {
		return nil, err
	}
	sdkConfig, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(config.AccessKeyID, config.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load R2 client configuration: %w", err)
	}
	client := s3.NewFromConfig(sdkConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})
	return &R2BlobStore{bucket: config.Bucket, client: client, presigner: s3.NewPresignClient(client)}, nil
}

func (store *R2BlobStore) CreateMultipartUpload(ctx context.Context, key, mediaType, expectedSHA256 string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	if err := validateDirectUploadHash(expectedSHA256); err != nil {
		return "", err
	}
	output, err := store.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(store.bucket), Key: aws.String(key), ContentType: aws.String(mediaType),
		Metadata: map[string]string{"trajectory-sha256": expectedSHA256},
	})
	if err != nil {
		return "", fmt.Errorf("create R2 multipart upload %q: %w", key, err)
	}
	if strings.TrimSpace(aws.ToString(output.UploadId)) == "" {
		return "", fmt.Errorf("create R2 multipart upload %q: response upload ID is missing", key)
	}
	return aws.ToString(output.UploadId), nil
}

func (store *R2BlobStore) AuthorizeMultipartPart(ctx context.Context, key, uploadID string, partNumber int32, size int64, ttl time.Duration) (DirectUploadAuthorization, error) {
	if err := validateKey(key); err != nil {
		return DirectUploadAuthorization{}, err
	}
	if strings.TrimSpace(uploadID) == "" {
		return DirectUploadAuthorization{}, errors.New("multipart upload ID is required")
	}
	if partNumber < 1 || partNumber > 10_000 {
		return DirectUploadAuthorization{}, errors.New("multipart part number must be between 1 and 10000")
	}
	if size < 1 || size > 5<<30 {
		return DirectUploadAuthorization{}, errors.New("multipart part size must be between 1 byte and 5 GiB")
	}
	if ttl < time.Minute || ttl > time.Hour {
		return DirectUploadAuthorization{}, errors.New("multipart upload authorization must last between one minute and one hour")
	}
	request, err := store.presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(store.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(partNumber), ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return DirectUploadAuthorization{}, fmt.Errorf("authorize R2 multipart part %d for %q: %w", partNumber, key, err)
	}
	headers := make(map[string]string, len(request.SignedHeader))
	for name, values := range request.SignedHeader {
		headers[name] = strings.Join(values, ",")
	}
	return DirectUploadAuthorization{URL: request.URL, Method: request.Method, Headers: headers, ExpiresAt: time.Now().UTC().Add(ttl)}, nil
}

func (store *R2BlobStore) CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []MultipartPart) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if strings.TrimSpace(uploadID) == "" || len(parts) == 0 || len(parts) > 10_000 {
		return errors.New("multipart upload ID and one to 10000 parts are required")
	}
	completed := make([]types.CompletedPart, len(parts))
	var previous int32
	for index, part := range parts {
		if part.Number <= previous || strings.TrimSpace(part.ETag) == "" {
			return errors.New("multipart parts must have unique ascending numbers and ETags")
		}
		completed[index] = types.CompletedPart{PartNumber: aws.Int32(part.Number), ETag: aws.String(part.ETag)}
		previous = part.Number
	}
	input := &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(store.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}
	if isRaw(key) {
		input.IfNoneMatch = aws.String("*")
	}
	_, err := store.client.CompleteMultipartUpload(ctx, input)
	if err != nil {
		if isRaw(key) && (hasHTTPStatus(err, 409) || hasHTTPStatus(err, 412)) {
			return ErrImmutableRaw
		}
		return fmt.Errorf("complete R2 multipart upload %q: %w", key, err)
	}
	return nil
}

func (store *R2BlobStore) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if strings.TrimSpace(uploadID) == "" {
		return errors.New("multipart upload ID is required")
	}
	_, err := store.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(store.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	if err != nil {
		return fmt.Errorf("abort R2 multipart upload %q: %w", key, err)
	}
	return nil
}

func (store *R2BlobStore) PromoteMultipartUpload(ctx context.Context, sourceKey, destinationKey string) error {
	if err := validateKey(sourceKey); err != nil {
		return err
	}
	if err := validateKey(destinationKey); err != nil {
		return err
	}
	if isRaw(sourceKey) || !isRaw(destinationKey) {
		return errors.New("multipart promotion must move a staging object to immutable raw storage")
	}
	copySource := url.PathEscape(store.bucket + "/" + sourceKey)
	_, err := store.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(store.bucket), Key: aws.String(destinationKey), CopySource: aws.String(copySource),
		IfNoneMatch: aws.String("*"), MetadataDirective: types.MetadataDirectiveCopy,
	})
	if err != nil {
		if hasHTTPStatus(err, 409) || hasHTTPStatus(err, 412) {
			return ErrImmutableRaw
		}
		return fmt.Errorf("promote R2 multipart object %q to %q: %w", sourceKey, destinationKey, err)
	}
	_, err = store.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(sourceKey)})
	if err != nil {
		return fmt.Errorf("remove promoted R2 staging object %q: %w", sourceKey, err)
	}
	return nil
}

func validateDirectUploadHash(expectedSHA256 string) error {
	if len(expectedSHA256) != 64 {
		return errors.New("direct upload SHA-256 must be a lowercase hex digest")
	}
	if _, err := hex.DecodeString(expectedSHA256); err != nil || strings.ToLower(expectedSHA256) != expectedSHA256 {
		return errors.New("direct upload SHA-256 must be a lowercase hex digest")
	}
	return nil
}

func validateR2Config(config R2Config) (string, error) {
	if strings.TrimSpace(config.Endpoint) == "" || strings.TrimSpace(config.Bucket) == "" ||
		strings.TrimSpace(config.AccessKeyID) == "" || strings.TrimSpace(config.SecretKey) == "" {
		return "", errors.New("R2 endpoint, bucket, access key ID, and secret key are required")
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("R2 endpoint must be an HTTPS origin without credentials, a path, query, or fragment")
	}
	return strings.TrimSuffix(config.Endpoint, "/"), nil
}

func (store *R2BlobStore) Put(ctx context.Context, key string, reader io.Reader) (BlobInfo, error) {
	if err := validateKey(key); err != nil {
		return BlobInfo{}, err
	}
	hash := sha256.New()
	counted := &countingReader{reader: io.TeeReader(reader, hash)}
	input := &s3.PutObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key), Body: counted}
	if isRaw(key) {
		input.IfNoneMatch = aws.String("*")
	}
	_, err := store.client.PutObject(ctx, input)
	if err != nil {
		if isRaw(key) && (hasHTTPStatus(err, 409) || hasHTTPStatus(err, 412)) {
			return BlobInfo{}, ErrImmutableRaw
		}
		return BlobInfo{}, fmt.Errorf("put R2 object %q: %w", key, err)
	}
	return BlobInfo{Key: key, Size: counted.count, SHA256: hex.EncodeToString(hash.Sum(nil)), LastModified: time.Now().UTC()}, nil
}

func (store *R2BlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	output, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("get R2 object %q: %w", key, err)
	}
	if output.Body == nil {
		return nil, fmt.Errorf("get R2 object %q: response body is missing", key)
	}
	return output.Body, nil
}

func (store *R2BlobStore) Exists(ctx context.Context, key string) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	_, err := store.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
	if err == nil {
		return true, nil
	}
	if hasHTTPStatus(err, 404) {
		return false, nil
	}
	return false, fmt.Errorf("check R2 object %q: %w", key, err)
}

func (store *R2BlobStore) Stat(ctx context.Context, key string) (BlobInfo, error) {
	if err := validateKey(key); err != nil {
		return BlobInfo{}, err
	}
	head, err := store.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
	if err != nil {
		return BlobInfo{}, fmt.Errorf("stat R2 object %q: %w", key, err)
	}
	reader, err := store.Get(ctx, key)
	if err != nil {
		return BlobInfo{}, err
	}
	defer reader.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	if err != nil {
		return BlobInfo{}, fmt.Errorf("hash R2 object %q: %w", key, err)
	}
	if head.ContentLength != nil && *head.ContentLength != size {
		return BlobInfo{}, fmt.Errorf("R2 object %q changed while being inspected", key)
	}
	lastModified := time.Time{}
	if head.LastModified != nil {
		lastModified = head.LastModified.UTC()
	}
	return BlobInfo{Key: key, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), LastModified: lastModified}, nil
}

func (store *R2BlobStore) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if isRaw(key) {
		return ErrImmutableRaw
	}
	_, err := store.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("delete R2 object %q: %w", key, err)
	}
	return nil
}

func (store *R2BlobStore) Purge(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	_, err := store.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(store.bucket), Key: aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("purge R2 object %q: %w", key, err)
	}
	return nil
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	reader.count += int64(read)
	return read, err
}

func hasHTTPStatus(err error, status int) bool {
	var responseError *smithyhttp.ResponseError
	return errors.As(err, &responseError) && responseError.HTTPStatusCode() == status
}
