package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type MediaInfo struct {
	Codec           string
	Width, Height   int
	DurationSeconds float64
}

type MediaInspector interface {
	InspectMP4(context.Context, io.Reader) (MediaInfo, error)
}

type FFprobeInspector struct {
	executable string
	timeout    time.Duration
}

func NewFFprobeInspector(executable string) (*FFprobeInspector, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		executable = "ffprobe"
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("ffprobe is required for media inspection: %w", err)
	}
	return &FFprobeInspector{executable: path, timeout: 45 * time.Second}, nil
}

func (inspector *FFprobeInspector) InspectMP4(ctx context.Context, input io.Reader) (MediaInfo, error) {
	if inspector == nil || inspector.executable == "" || input == nil {
		return MediaInfo{}, errors.New("ffprobe inspector and media input are required")
	}
	probeContext, cancel := context.WithTimeout(ctx, inspector.timeout)
	defer cancel()
	command := exec.CommandContext(probeContext, inspector.executable,
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height,duration:format=duration,format_name",
		"-of", "json", "-i", "pipe:0")
	command.Stdin = input
	var stdout limitedBuffer
	var stderr limitedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if errors.Is(probeContext.Err(), context.DeadlineExceeded) {
			return MediaInfo{}, errors.New("ffprobe timed out")
		}
		return MediaInfo{}, fmt.Errorf("ffprobe rejected media: %s", safeProbeMessage(stderr.String(), err.Error()))
	}
	var result struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Duration  string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration   string `json:"duration"`
			FormatName string `json:"format_name"`
		} `json:"format"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return MediaInfo{}, fmt.Errorf("decode ffprobe result: %w", err)
	}
	if len(result.Streams) != 1 {
		return MediaInfo{}, errors.New("MP4 must contain one inspectable video stream")
	}
	stream := result.Streams[0]
	durationText := stream.Duration
	if durationText == "" || durationText == "N/A" {
		durationText = result.Format.Duration
	}
	duration, err := strconv.ParseFloat(durationText, 64)
	if err != nil || duration <= 0 || math.IsInf(duration, 0) || math.IsNaN(duration) {
		return MediaInfo{}, errors.New("MP4 has no finite positive duration")
	}
	if !strings.Contains(result.Format.FormatName, "mp4") && !strings.Contains(result.Format.FormatName, "mov") {
		return MediaInfo{}, fmt.Errorf("unsupported video container %q", result.Format.FormatName)
	}
	if stream.CodecName != "h264" {
		return MediaInfo{}, fmt.Errorf("unsupported video codec %q; H.264 is required", stream.CodecName)
	}
	if stream.Width < 1 || stream.Height < 1 {
		return MediaInfo{}, errors.New("MP4 video dimensions are missing")
	}
	return MediaInfo{Codec: stream.CodecName, Width: stream.Width, Height: stream.Height, DurationSeconds: duration}, nil
}

type limitedBuffer struct{ bytes.Buffer }

func (buffer *limitedBuffer) Write(input []byte) (int, error) {
	const limit = 64 << 10
	original := len(input)
	if buffer.Len() < limit {
		remaining := limit - buffer.Len()
		if len(input) > remaining {
			input = input[:remaining]
		}
		_, _ = buffer.Buffer.Write(input)
	}
	return original, nil
}

func safeProbeMessage(message, fallback string) string {
	message = strings.TrimSpace(strings.ReplaceAll(message, "\n", " "))
	if message == "" {
		message = fallback
	}
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}
