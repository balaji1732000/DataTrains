package blobstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLocalBlobStoreRoundTripAndHash(t *testing.T) {
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	info, err := store.Put(ctx, "derived/sessions/sess_001/trajectory.json", strings.NewReader("trajectory"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 10 || info.SHA256 != "c32662785b597e46c00684652c150705acb460d6c349e2ca831348c9f4f7ce74" {
		t.Fatalf("unexpected info: %#v", info)
	}
	reader, err := store.Get(ctx, info.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	contents, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "trajectory" {
		t.Fatalf("contents = %q", contents)
	}
}

func TestRawArtifactsCannotBeOverwrittenOrDeleted(t *testing.T) {
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "raw/sessions/sess_001/video/000001.mp4"
	if _, err := store.Put(ctx, key, strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, key, strings.NewReader("second")); !errors.Is(err, ErrImmutableRaw) {
		t.Fatalf("overwrite error = %v, want ErrImmutableRaw", err)
	}
	if err := store.Delete(ctx, key); !errors.Is(err, ErrImmutableRaw) {
		t.Fatalf("delete error = %v, want ErrImmutableRaw", err)
	}
	if err := store.Purge(ctx, key); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(ctx, key); err != nil || exists {
		t.Fatalf("purged raw object exists=%v err=%v", exists, err)
	}
}

func TestLocalBlobStoreRejectsTraversal(t *testing.T) {
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"../escape", "raw/../../escape", "/absolute", "raw\\escape"} {
		if _, err := store.Put(context.Background(), key, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q) error = %v, want ErrInvalidKey", key, err)
		}
	}
}
