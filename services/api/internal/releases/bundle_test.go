package releases

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"testing"

	"trajectory.local/api/internal/blobstore"
)

func TestReleaseBundleIsDeterministicAndComplete(t *testing.T) {
	ctx := context.Background()
	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := "releases/rel_bundle/"
	entries := make([]FileEntry, 0, 2)
	for path, contents := range map[string]string{
		"manifest.json":      "{\"schema_version\":\"release/v1\"}\n",
		"trajectories.jsonl": "{\"schema_version\":\"trajectory/v1\"}\n",
	} {
		info, err := blobs.Put(ctx, prefix+path, bytes.NewBufferString(contents))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, FileEntry{Path: path, Size: info.Size, SHA256: info.SHA256, MediaType: "application/json"})
	}
	first, err := streamReleaseBundle(ctx, blobs, prefix, "bundles/first.zip", entries)
	if err != nil {
		t.Fatal(err)
	}
	second, err := streamReleaseBundle(ctx, blobs, prefix, "bundles/second.zip", entries)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 || first.Size != second.Size {
		t.Fatalf("bundle is not deterministic: first=%#v second=%#v", first, second)
	}
	reader, err := blobs.Get(ctx, "bundles/first.zip")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(contents), int64(len(contents)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 2 || archive.File[0].Name != "manifest.json" || archive.File[1].Name != "trajectories.jsonl" {
		t.Fatalf("bundle inventory = %#v", archive.File)
	}
	for _, file := range archive.File {
		if file.Modified.UTC().Year() != 1980 || file.Method != zip.Store {
			t.Fatalf("non-deterministic ZIP metadata for %s: time=%v method=%d", file.Name, file.Modified, file.Method)
		}
	}
}
