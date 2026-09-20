package blobstore

import (
	"context"
	"fmt"
)

type Config struct {
	Backend       string
	LocalRoot     string
	R2Endpoint    string
	R2Bucket      string
	R2AccessKeyID string
	R2SecretKey   string
}

func Open(ctx context.Context, config Config) (BlobStore, error) {
	switch config.Backend {
	case "local":
		return NewLocal(config.LocalRoot)
	case "r2":
		return NewR2(ctx, R2Config{
			Endpoint:    config.R2Endpoint,
			Bucket:      config.R2Bucket,
			AccessKeyID: config.R2AccessKeyID,
			SecretKey:   config.R2SecretKey,
		})
	default:
		return nil, fmt.Errorf("unsupported blob backend %q", config.Backend)
	}
}
