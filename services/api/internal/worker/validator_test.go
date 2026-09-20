package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/domain"
)

func TestValidatorNormalizesVerifiedTrajectory(t *testing.T) {
	validator, artifacts := fixtureValidator(t, "sess_worker_valid", false, "en-US")
	normalized, problems, err := validator.Validate(context.Background(), "sess_worker_valid", artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("validation problems: %v", problems)
	}
	if !strings.HasSuffix(string(normalized), "\n") || !strings.Contains(string(normalized), `"schema_version": "trajectory/v1"`) {
		t.Fatalf("unexpected normalized output: %s", normalized)
	}
}

func TestValidatorAcceptsPOSIXCLocaleFromLinuxWebviews(t *testing.T) {
	validator, artifacts := fixtureValidator(t, "sess_worker_c_locale", false, "C")
	_, problems, err := validator.Validate(context.Background(), "sess_worker_c_locale", artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("validation problems: %v", problems)
	}
}

func TestValidatorRejectsNonMonotonicActions(t *testing.T) {
	validator, artifacts := fixtureValidator(t, "sess_worker_nonmonotonic", true, "en-US")
	_, problems, err := validator.Validate(context.Background(), "sess_worker_nonmonotonic", artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "not monotonic") {
		t.Fatalf("problems = %v", problems)
	}
}

func TestValidatorRejectsVideoThatFailsMediaInspection(t *testing.T) {
	validator, artifacts := fixtureValidator(t, "sess_worker_bad_media", false, "en-US")
	validator.inspector = rejectingMediaInspector{}
	_, problems, err := validator.Validate(context.Background(), "sess_worker_bad_media", artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "failed media inspection") {
		t.Fatalf("media validation problems = %v", problems)
	}
}

type rejectingMediaInspector struct{}

func (rejectingMediaInspector) InspectMP4(context.Context, io.Reader) (MediaInfo, error) {
	return MediaInfo{}, errors.New("corrupt fixture")
}

func TestNormalizeActionsDownsamplesMovementAndCreatesClicksWithoutReconstructingText(t *testing.T) {
	actions := []any{
		map[string]any{"timestamp_ns": json.Number("100000000"), "type": "mouse_move", "x": json.Number("10"), "y": json.Number("20")},
		map[string]any{"timestamp_ns": json.Number("110000000"), "type": "mouse_move", "x": json.Number("11"), "y": json.Number("21")},
		map[string]any{"timestamp_ns": json.Number("120000000"), "type": "mouse_down", "button": "left"},
		map[string]any{"timestamp_ns": json.Number("130000000"), "type": "mouse_up", "button": "left"},
		map[string]any{"timestamp_ns": json.Number("140000000"), "type": "key_down", "key": "VK_41"},
	}
	normalized := normalizeActions(actions)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if len(normalized) != 3 || !strings.Contains(text, `"type":"click"`) || strings.Contains(text, `"type":"mouse_down"`) || strings.Contains(text, "text_entry") {
		t.Fatalf("unexpected normalized actions: %s", encoded)
	}
}

func TestActionsJSONLHasOneCanonicalActionPerLine(t *testing.T) {
	encoded, err := actionsJSONL([]byte(`{"actions":[{"timestamp_ns":1,"type":"click"},{"timestamp_ns":2,"type":"scroll"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{\"timestamp_ns\":1,\"type\":\"click\"}\n{\"timestamp_ns\":2,\"type\":\"scroll\"}\n" {
		t.Fatalf("unexpected JSONL: %q", encoded)
	}
}

func fixtureValidator(t *testing.T, sessionID string, nonMonotonic bool, locale string) (*Validator, []domain.Artifact) {
	t.Helper()
	root := t.TempDir()
	store, err := blobstore.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	schemaDocument, err := os.ReadFile("../../../../packages/trajectory-schema/schemas/trajectory-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewValidator(store, schemaDocument)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	video := putFixture(t, store, ctx, "raw/sessions/"+sessionID+"/video/000001.mp4", "video bytes", "video/mp4", sessionID)
	events := putFixture(t, store, ctx, "raw/sessions/"+sessionID+"/events/000001.jsonl", "{}\n", "application/x-ndjson", sessionID)
	output := putFixture(t, store, ctx, "raw/sessions/"+sessionID+"/outputs/final.png", "png bytes", "image/png", sessionID)

	fixture, err := os.ReadFile("../../../../packages/trajectory-schema/fixtures/valid-trajectory.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(fixture, &document); err != nil {
		t.Fatal(err)
	}
	document["trajectory_id"] = "traj_" + sessionID
	document["provenance"].(map[string]any)["session_id"] = sessionID
	document["environment"].(map[string]any)["locale"] = locale
	segment := document["capture"].(map[string]any)["video_segments"].([]any)[0].(map[string]any)
	segment["key"], segment["sha256"], segment["size"] = video.Key, video.SHA256, video.Size
	outputReference := document["outcome"].(map[string]any)["output_artifacts"].([]any)[0].(map[string]any)
	outputReference["key"], outputReference["sha256"], outputReference["size"] = output.Key, output.SHA256, output.Size
	if nonMonotonic {
		actions := document["actions"].([]any)
		actions[0].(map[string]any)["timestamp_ns"] = float64(2_000_000)
		actions[1].(map[string]any)["timestamp_ns"] = float64(1_000_000)
	}
	trajectoryBytes, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	trajectory := putFixture(t, store, ctx, "raw/sessions/"+sessionID+"/trajectory.json", string(trajectoryBytes), "application/json", sessionID)
	return validator, []domain.Artifact{video, events, output, trajectory}
}

func putFixture(t *testing.T, store blobstore.BlobStore, ctx context.Context, key, contents, mediaType, sessionID string) domain.Artifact {
	t.Helper()
	info, err := store.Put(ctx, key, strings.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	return domain.Artifact{SessionID: sessionID, Key: key, SHA256: info.SHA256, Size: info.Size, MediaType: mediaType}
}
