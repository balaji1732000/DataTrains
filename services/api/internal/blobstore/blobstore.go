package blobstore

import (
	"context"
	"io"
	"time"
)

type BlobInfo struct {
	Key          string
	Size         int64
	SHA256       string
	LastModified time.Time
}

type BlobStore interface {
	Put(context.Context, string, io.Reader) (BlobInfo, error)
	Get(context.Context, string) (io.ReadCloser, error)
	Exists(context.Context, string) (bool, error)
	Stat(context.Context, string) (BlobInfo, error)
	Delete(context.Context, string) error
	// Purge is reserved for the audited retention/erasure worker. Unlike Delete,
	// it may remove immutable raw evidence after legal-hold checks have passed.
	Purge(context.Context, string) error
}

type DirectUploadAuthorization struct {
	URL       string
	Method    string
	Headers   map[string]string
	ExpiresAt time.Time
}

type MultipartPart struct {
	Number int32
	ETag   string
}

type MultipartUploadAuthorizer interface {
	CreateMultipartUpload(context.Context, string, string, string) (string, error)
	AuthorizeMultipartPart(context.Context, string, string, int32, int64, time.Duration) (DirectUploadAuthorization, error)
	CompleteMultipartUpload(context.Context, string, string, []MultipartPart) error
	AbortMultipartUpload(context.Context, string, string) error
	PromoteMultipartUpload(context.Context, string, string) error
}
