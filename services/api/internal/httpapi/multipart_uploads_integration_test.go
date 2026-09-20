package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/database"
)

type testMultipartStore struct {
	*blobstore.LocalBlobStore
	parts map[string]map[int32][]byte
}

func (store *testMultipartStore) CreateMultipartUpload(_ context.Context, _ string, _ string, _ string) (string, error) {
	id := fmt.Sprintf("storage-upload-%d", len(store.parts)+1)
	store.parts[id] = make(map[int32][]byte)
	return id, nil
}

func (store *testMultipartStore) AuthorizeMultipartPart(_ context.Context, _ string, uploadID string, number int32, _ int64, _ time.Duration) (blobstore.DirectUploadAuthorization, error) {
	return blobstore.DirectUploadAuthorization{URL: "https://uploads.example.test/" + uploadID + fmt.Sprintf("/%d", number), Method: http.MethodPut}, nil
}

func (store *testMultipartStore) CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []blobstore.MultipartPart) error {
	var assembled bytes.Buffer
	for _, part := range parts {
		contents, ok := store.parts[uploadID][part.Number]
		if !ok {
			return fmt.Errorf("missing part %d", part.Number)
		}
		assembled.Write(contents)
	}
	_, err := store.Put(ctx, key, &assembled)
	return err
}

func (store *testMultipartStore) AbortMultipartUpload(_ context.Context, _ string, uploadID string) error {
	delete(store.parts, uploadID)
	return nil
}

func (store *testMultipartStore) PromoteMultipartUpload(ctx context.Context, sourceKey, destinationKey string) error {
	reader, err := store.Get(ctx, sourceKey)
	if err != nil {
		return err
	}
	defer reader.Close()
	if _, err := store.Put(ctx, destinationKey, reader); err != nil {
		return err
	}
	return store.Delete(ctx, sourceKey)
}

func TestMultipartArtifactUploadResumesAndCommitsVerifiedBytes(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := database.Migrate(ctx, store.Pool(), "../../migrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `TRUNCATE organizations, contributors, consent_documents, audit_events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	_, err = store.Pool().Exec(ctx, `
		INSERT INTO organizations (id, name) VALUES ('org_upload', 'Upload test');
		INSERT INTO projects (id, organization_id, name, target_trajectories) VALUES ('proj_upload', 'org_upload', 'Upload test', 1);
		INSERT INTO task_templates (id, project_id, name, goal, category, difficulty, specification)
		  VALUES ('tmpl_upload', 'proj_upload', 'Upload', 'Upload', 'desktop.test', 'beginner', '{}');
		INSERT INTO tasks (id, template_id, goal) VALUES ('task_upload', 'tmpl_upload', 'Upload');
		INSERT INTO contributors (id, display_name) VALUES ('contrib_upload', 'Upload tester');
		INSERT INTO assignments (id, task_id, contributor_id) VALUES ('assign_upload', 'task_upload', 'contrib_upload');
		INSERT INTO sessions (id, assignment_id, state) VALUES ('sess_upload', 'assign_upload', 'UPLOADING');`)
	if err != nil {
		t.Fatal(err)
	}
	local, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blobs := &testMultipartStore{LocalBlobStore: local, parts: make(map[string]map[int32][]byte)}
	server := httptest.NewServer(NewWithBlobStore(store, blobs, []byte(`{"type":"object"}`)).Handler())
	defer server.Close()

	contents := append(bytes.Repeat([]byte("a"), int(multipartPartSize)), []byte("tail")...)
	digest := sha256.Sum256(contents)
	request := map[string]any{
		"path": "video/000001.mp4", "sha256": fmt.Sprintf("%x", digest),
		"size_bytes": len(contents), "media_type": "video/mp4",
	}
	startURL := server.URL + "/v1/sessions/sess_upload/multipart-uploads"
	started := postJSONAs(t, server.Client(), startURL, request, "contrib_upload", http.StatusCreated)
	uploadID := started["upload_id"].(string)
	if started["total_parts"] != float64(2) {
		t.Fatalf("unexpected multipart plan: %#v", started)
	}
	resumed := postJSONAs(t, server.Client(), startURL, request, "contrib_upload", http.StatusOK)
	if resumed["upload_id"] != uploadID {
		t.Fatalf("resume changed upload identity: %#v", resumed)
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE artifact_multipart_uploads SET expires_at=now()-interval '1 second' WHERE id=$1`, uploadID); err != nil {
		t.Fatal(err)
	}
	restarted := postJSONAs(t, server.Client(), startURL, request, "contrib_upload", http.StatusCreated)
	if restarted["upload_id"] != uploadID || len(restarted["uploaded_parts"].([]any)) != 0 {
		t.Fatalf("expired upload did not restart cleanly: %#v", restarted)
	}

	var storageUploadID string
	if err := store.Pool().QueryRow(ctx, `SELECT storage_upload_id FROM artifact_multipart_uploads WHERE id=$1`, uploadID).Scan(&storageUploadID); err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{contents[:multipartPartSize], contents[multipartPartSize:]}
	for index, part := range parts {
		number := index + 1
		partURL := fmt.Sprintf("%s/v1/sessions/sess_upload/multipart-uploads/%s/parts/%d", server.URL, uploadID, number)
		authorized := postJSONAs(t, server.Client(), partURL+"/authorize", map[string]any{"size_bytes": len(part)}, "contrib_upload", http.StatusCreated)
		if authorized["method"] != http.MethodPut {
			t.Fatalf("unexpected part authorization: %#v", authorized)
		}
		blobs.parts[storageUploadID][int32(number)] = append([]byte(nil), part...)
		postJSONAs(t, server.Client(), partURL+"/complete", map[string]any{
			"size_bytes": len(part), "etag": fmt.Sprintf("\"part-%d\"", number),
		}, "contrib_upload", http.StatusOK)
		if number == 1 {
			progress := postJSONAs(t, server.Client(), startURL, request, "contrib_upload", http.StatusOK)
			if len(progress["uploaded_parts"].([]any)) != 1 {
				t.Fatalf("uploaded part was not persisted for resume: %#v", progress)
			}
		}
	}
	completed := postJSONAs(t, server.Client(), fmt.Sprintf("%s/v1/sessions/sess_upload/multipart-uploads/%s/complete", server.URL, uploadID), map[string]any{}, "contrib_upload", http.StatusCreated)
	if completed["state"] != "completed" {
		t.Fatalf("unexpected completion: %#v", completed)
	}
	reader, err := blobs.Get(ctx, "raw/sessions/sess_upload/video/000001.mp4")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(stored, contents) {
		t.Fatalf("committed bytes differ: size=%d err=%v", len(stored), err)
	}
}
