package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidKey   = errors.New("invalid blob key")
	ErrImmutableRaw = errors.New("raw artifact is immutable")
)

type LocalBlobStore struct{ root string }

func NewLocal(root string) (*LocalBlobStore, error) {
	if root == "" {
		return nil, errors.New("blob store root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve blob store root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("create blob store root: %w", err)
	}
	return &LocalBlobStore{root: absolute}, nil
}

func validateKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || path.Clean(key) != key {
		return ErrInvalidKey
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return ErrInvalidKey
		}
	}
	return nil
}

func (store *LocalBlobStore) filename(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	filename := filepath.Join(store.root, filepath.FromSlash(key))
	relative, err := filepath.Rel(store.root, filename)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", ErrInvalidKey
	}
	return filename, nil
}

func isRaw(key string) bool { return strings.HasPrefix(key, "raw/") }

func (store *LocalBlobStore) Put(ctx context.Context, key string, reader io.Reader) (BlobInfo, error) {
	if err := ctx.Err(); err != nil {
		return BlobInfo{}, err
	}
	filename, err := store.filename(key)
	if err != nil {
		return BlobInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return BlobInfo{}, fmt.Errorf("create blob directory: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(filename), ".blob-*")
	if err != nil {
		return BlobInfo{}, fmt.Errorf("create temporary blob: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)

	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), reader)
	closeErr := temporary.Close()
	if copyErr != nil {
		return BlobInfo{}, fmt.Errorf("write blob: %w", copyErr)
	}
	if closeErr != nil {
		return BlobInfo{}, fmt.Errorf("close blob: %w", closeErr)
	}

	if isRaw(key) {
		if err := os.Link(temporaryName, filename); err != nil {
			if errors.Is(err, os.ErrExist) {
				return BlobInfo{}, ErrImmutableRaw
			}
			return BlobInfo{}, fmt.Errorf("commit immutable raw blob: %w", err)
		}
	} else if err := os.Rename(temporaryName, filename); err != nil {
		return BlobInfo{}, fmt.Errorf("commit blob: %w", err)
	}

	stat, err := os.Stat(filename)
	if err != nil {
		return BlobInfo{}, fmt.Errorf("stat committed blob: %w", err)
	}
	return BlobInfo{Key: key, Size: written, SHA256: hex.EncodeToString(hash.Sum(nil)), LastModified: stat.ModTime().UTC()}, nil
}

func (store *LocalBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filename, err := store.filename(key)
	if err != nil {
		return nil, err
	}
	return os.Open(filename)
}

func (store *LocalBlobStore) Exists(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	filename, err := store.filename(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(filename)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (store *LocalBlobStore) Stat(ctx context.Context, key string) (BlobInfo, error) {
	reader, err := store.Get(ctx, key)
	if err != nil {
		return BlobInfo{}, err
	}
	defer reader.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	if err != nil {
		return BlobInfo{}, fmt.Errorf("hash blob: %w", err)
	}
	filename, _ := store.filename(key)
	stat, err := os.Stat(filename)
	if err != nil {
		return BlobInfo{}, err
	}
	return BlobInfo{Key: key, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), LastModified: stat.ModTime().UTC()}, nil
}

func (store *LocalBlobStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if isRaw(key) {
		return ErrImmutableRaw
	}
	filename, err := store.filename(key)
	if err != nil {
		return err
	}
	return os.Remove(filename)
}

func (store *LocalBlobStore) Purge(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	filename, err := store.filename(key)
	if err != nil {
		return err
	}
	if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
