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
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"trajectory.local/api/internal/blobstore"
	"trajectory.local/api/internal/domain"
)

const (
	SchemaVersion    = "trajectory/v1"
	ValidatorVersion = "trajectory-validator/0.2.0"
	maxTrajectory    = 32 << 20
)

type Validator struct {
	blobs     blobstore.BlobStore
	schema    *jsonschema.Schema
	inspector MediaInspector
}

func NewValidator(blobs blobstore.BlobStore, schemaDocument []byte) (*Validator, error) {
	if blobs == nil {
		return nil, errors.New("blob store is required")
	}
	var document any
	if err := json.Unmarshal(schemaDocument, &document); err != nil {
		return nil, fmt.Errorf("decode trajectory schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource("trajectory-v1.json", document); err != nil {
		return nil, fmt.Errorf("load trajectory schema: %w", err)
	}
	schema, err := compiler.Compile("trajectory-v1.json")
	if err != nil {
		return nil, fmt.Errorf("compile trajectory schema: %w", err)
	}
	return &Validator{blobs: blobs, schema: schema}, nil
}

func NewValidatorWithMediaInspector(blobs blobstore.BlobStore, schemaDocument []byte, inspector MediaInspector) (*Validator, error) {
	if inspector == nil {
		return nil, errors.New("media inspector is required")
	}
	validator, err := NewValidator(blobs, schemaDocument)
	if err != nil {
		return nil, err
	}
	validator.inspector = inspector
	return validator, nil
}

type validationDocument struct {
	SchemaVersion string `json:"schema_version"`
	Capture       struct {
		DurationNS    uint64 `json:"duration_ns"`
		VideoSegments []struct {
			Key       string `json:"key"`
			StartNS   uint64 `json:"start_ns"`
			EndNS     uint64 `json:"end_ns"`
			SHA256    string `json:"sha256"`
			Size      int64  `json:"size"`
			SegmentID string `json:"segment_id"`
		} `json:"video_segments"`
	} `json:"capture"`
	Actions []struct {
		TimestampNS uint64 `json:"timestamp_ns"`
		Type        string `json:"type"`
	} `json:"actions"`
	Outcome struct {
		OutputArtifacts []struct {
			Key       string `json:"key"`
			SHA256    string `json:"sha256"`
			Size      int64  `json:"size"`
			MediaType string `json:"media_type"`
		} `json:"output_artifacts"`
	} `json:"outcome"`
	Privacy struct {
		ConsentDocumentID string `json:"consent_document_id"`
		ConsentVersion    string `json:"consent_version"`
		ClipboardCaptured bool   `json:"clipboard_captured"`
		RecordingVisible  bool   `json:"recording_visible"`
	} `json:"privacy"`
	Provenance struct {
		SessionID string `json:"session_id"`
	} `json:"provenance"`
}

func (validator *Validator) Validate(ctx context.Context, sessionID string, artifacts []domain.Artifact) ([]byte, []string, error) {
	registered := make(map[string]domain.Artifact, len(artifacts))
	for _, artifact := range artifacts {
		info, err := validator.blobs.Stat(ctx, artifact.Key)
		if err != nil {
			return nil, nil, fmt.Errorf("stat artifact %s: %w", artifact.Key, err)
		}
		registered[artifact.Key] = artifact
		if info.Size != artifact.Size || info.SHA256 != artifact.SHA256 {
			return nil, []string{fmt.Sprintf("artifact metadata mismatch: %s", artifact.Key)}, nil
		}
	}
	trajectoryKey := "raw/sessions/" + sessionID + "/trajectory.json"
	trajectory, found := registered[trajectoryKey]
	if !found {
		return nil, []string{"required artifact is missing: " + trajectoryKey}, nil
	}
	reader, err := validator.blobs.Get(ctx, trajectory.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("read trajectory artifact: %w", err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, maxTrajectory+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read trajectory artifact: %w", err)
	}
	if len(raw) > maxTrajectory {
		return nil, []string{"trajectory.json exceeds 32 MiB"}, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, []string{"trajectory.json is not valid JSON: " + err.Error()}, nil
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, []string{"trajectory.json contains trailing JSON: " + err.Error()}, nil
	}
	if err := validator.schema.Validate(generic); err != nil {
		return nil, []string{"schema validation failed: " + err.Error()}, nil
	}
	var document validationDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, []string{"trajectory.json could not be inspected: " + err.Error()}, nil
	}
	problems := validateCrossReferences(sessionID, document, registered)
	if validator.inspector != nil {
		mediaProblems, err := validator.inspectMedia(ctx, document, registered)
		if err != nil {
			return nil, nil, err
		}
		problems = append(problems, mediaProblems...)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, problems, nil
	}
	if object, ok := generic.(map[string]any); ok {
		if actions, ok := object["actions"].([]any); ok {
			object["actions"] = normalizeActions(actions)
		}
	}
	normalized, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("normalize trajectory: %w", err)
	}
	normalized = append(normalized, '\n')
	return normalized, nil, nil
}

func (validator *Validator) inspectMedia(ctx context.Context, document validationDocument, registered map[string]domain.Artifact) ([]string, error) {
	problems := make([]string, 0)
	for _, segment := range document.Capture.VideoSegments {
		if _, ok := registered[segment.Key]; !ok || segment.EndNS < segment.StartNS {
			continue
		}
		reader, err := validator.blobs.Get(ctx, segment.Key)
		if err != nil {
			return nil, fmt.Errorf("read video segment %s for inspection: %w", segment.SegmentID, err)
		}
		info, inspectErr := validator.inspector.InspectMP4(ctx, reader)
		closeErr := reader.Close()
		if inspectErr != nil {
			problems = append(problems, fmt.Sprintf("video segment %s failed media inspection: %v", segment.SegmentID, inspectErr))
			continue
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close video segment %s: %w", segment.SegmentID, closeErr)
		}
		declaredSeconds := float64(segment.EndNS-segment.StartNS) / 1_000_000_000
		tolerance := math.Max(2, declaredSeconds*0.10)
		if math.Abs(info.DurationSeconds-declaredSeconds) > tolerance {
			problems = append(problems, fmt.Sprintf("video segment %s duration %.3fs differs from declared %.3fs", segment.SegmentID, info.DurationSeconds, declaredSeconds))
		}
	}
	return problems, nil
}

// normalizeActions preserves exact key events but reduces high-volume pointer
// movement to 10 Hz and folds an adjacent down/up pair into a semantic click.
// It deliberately does not reconstruct typed text from virtual key codes.
func normalizeActions(actions []any) []any {
	const mouseMoveIntervalNS = uint64(100_000_000)
	result := make([]any, 0, len(actions))
	var lastMouseMove uint64
	var pointerX, pointerY json.Number
	for index := 0; index < len(actions); index++ {
		action, ok := actions[index].(map[string]any)
		if !ok {
			result = append(result, actions[index])
			continue
		}
		timestamp := uintValue(action["timestamp_ns"])
		typeName, _ := action["type"].(string)
		if typeName == "mouse_move" {
			pointerX, _ = action["x"].(json.Number)
			pointerY, _ = action["y"].(json.Number)
			if lastMouseMove != 0 && timestamp-lastMouseMove < mouseMoveIntervalNS {
				continue
			}
			lastMouseMove = timestamp
			result = append(result, action)
			continue
		}
		if typeName == "mouse_down" && index+1 < len(actions) {
			next, nextOK := actions[index+1].(map[string]any)
			button, _ := action["button"].(string)
			nextButton, _ := next["button"].(string)
			if nextOK && next["type"] == "mouse_up" && button != "" && button == nextButton {
				click := map[string]any{
					"timestamp_ns": next["timestamp_ns"], "type": "click", "button": button,
				}
				if application, exists := next["application"]; exists {
					click["application"] = application
				}
				if pointerX != "" && pointerY != "" {
					click["x"], click["y"] = pointerX, pointerY
				}
				result = append(result, click)
				index++
				continue
			}
		}
		result = append(result, action)
	}
	return result
}

func uintValue(value any) uint64 {
	switch number := value.(type) {
	case json.Number:
		parsed, _ := number.Int64()
		if parsed > 0 {
			return uint64(parsed)
		}
	case float64:
		if number > 0 {
			return uint64(number)
		}
	}
	return 0
}

func actionsJSONL(trajectory []byte) ([]byte, error) {
	var document struct {
		Actions []json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal(trajectory, &document); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	for _, action := range document.Actions {
		output.Write(action)
		output.WriteByte('\n')
	}
	return output.Bytes(), nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateCrossReferences(sessionID string, document validationDocument, registered map[string]domain.Artifact) []string {
	problems := make([]string, 0)
	prefix := "raw/sessions/" + sessionID + "/"
	if document.SchemaVersion != SchemaVersion {
		problems = append(problems, "unsupported schema_version: "+document.SchemaVersion)
	}
	if document.Provenance.SessionID != sessionID {
		problems = append(problems, "provenance.session_id does not match submitted session")
	}
	var previousSegmentEnd uint64
	for index, segment := range document.Capture.VideoSegments {
		if !strings.HasPrefix(segment.Key, prefix) {
			problems = append(problems, fmt.Sprintf("video segment %s is outside the submitted session", segment.SegmentID))
		}
		artifact, ok := registered[segment.Key]
		if !ok {
			problems = append(problems, "video segment is not registered: "+segment.Key)
		} else if artifact.SHA256 != segment.SHA256 || artifact.Size != segment.Size || artifact.MediaType != "video/mp4" {
			problems = append(problems, "video segment metadata mismatch: "+segment.Key)
		}
		if index > 0 && segment.StartNS < previousSegmentEnd {
			problems = append(problems, fmt.Sprintf("video segment %s overlaps its predecessor", segment.SegmentID))
		}
		if segment.EndNS > document.Capture.DurationNS {
			problems = append(problems, fmt.Sprintf("video segment %s exceeds capture duration", segment.SegmentID))
		}
		previousSegmentEnd = segment.EndNS
	}
	var previousAction uint64
	for index, action := range document.Actions {
		if index > 0 && action.TimestampNS < previousAction {
			problems = append(problems, fmt.Sprintf("action %d timestamp is not monotonic", index))
		}
		if action.TimestampNS > document.Capture.DurationNS {
			problems = append(problems, fmt.Sprintf("action %d exceeds capture duration", index))
		}
		previousAction = action.TimestampNS
	}
	for _, output := range document.Outcome.OutputArtifacts {
		if !strings.HasPrefix(output.Key, prefix) {
			problems = append(problems, "output artifact is outside the submitted session: "+output.Key)
		}
		artifact, ok := registered[output.Key]
		if !ok {
			problems = append(problems, "output artifact is not registered: "+output.Key)
		} else if artifact.SHA256 != output.SHA256 || artifact.Size != output.Size || artifact.MediaType != output.MediaType {
			problems = append(problems, "output artifact metadata mismatch: "+output.Key)
		}
	}
	if document.Privacy.ClipboardCaptured || !document.Privacy.RecordingVisible {
		problems = append(problems, "privacy invariants are not satisfied")
	}
	return unique(problems)
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
