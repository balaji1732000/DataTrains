package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"trajectory.local/api/internal/domain"
)

func TestFFmpegRedactorProducesValidBlackoutVideo(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed on this test host")
	}
	redactor, err := NewFFmpegRedactor(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	inspector, err := NewFFprobeInspector("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed on this test host")
	}
	sourceCommand := exec.Command(ffmpeg,
		"-v", "error", "-f", "lavfi", "-i", "color=c=white:s=32x24:r=10:d=1",
		"-an", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-movflags", "frag_keyframe+empty_moov", "-f", "mp4", "pipe:1")
	source, err := sourceCommand.Output()
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	var output bytes.Buffer
	err = redactor.Redact(context.Background(), bytes.NewReader(source), &output, []domain.RedactionRegion{{
		StartNS: 0, EndNS: 1_000_000_000, X: 0, Y: 0, Width: 1, Height: 1, Kind: "personal_data",
	}})
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if sourceHash, outputHash := sha256.Sum256(source), sha256.Sum256(output.Bytes()); sourceHash == outputHash {
		t.Fatal("redacted output unexpectedly equals its source")
	}
	info, err := inspector.InspectMP4(context.Background(), bytes.NewReader(output.Bytes()))
	if err != nil || info.Width != 32 || info.Height != 24 {
		t.Fatalf("inspect redacted output = %#v, %v", info, err)
	}
	frameCommand := exec.Command(ffmpeg, "-v", "error", "-i", "pipe:0", "-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	frameCommand.Stdin = bytes.NewReader(output.Bytes())
	frame, err := frameCommand.Output()
	if err != nil {
		t.Fatalf("decode redacted frame: %v", err)
	}
	for index, value := range frame {
		if value > 8 {
			t.Fatalf("redacted frame pixel %d = %d, expected an irreversible black mask", index, value)
		}
	}
}

func TestRedactionFilterUsesOnlyBoundedNumericExpressions(t *testing.T) {
	filter, err := redactionFilter([]domain.RedactionRegion{{
		StartNS: 250_000_000, EndNS: 750_000_000,
		X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4, Kind: "credential",
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "drawbox=x=iw*0.100000000:y=ih*0.200000000:w=iw*0.300000000:h=ih*0.400000000:color=black:t=fill:enable='between(t,0.250000000,0.750000000)'"
	if filter != want {
		t.Fatalf("filter = %q, want %q", filter, want)
	}
	if _, err := redactionFilter(nil); err == nil {
		t.Fatal("empty filter was accepted")
	}
	if _, err := redactionFilter([]domain.RedactionRegion{{StartNS: 0, EndNS: 1, X: 0.9, Y: 0, Width: 0.2, Height: 1}}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("out-of-bounds filter error = %v", err)
	}
}

func TestFFmpegRedactorNormalizesApprovedVideoAndRemovesAudio(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed on this test host")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed on this test host")
	}
	redactor, err := NewFFmpegRedactor(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	sourceCommand := exec.Command(ffmpeg,
		"-v", "error", "-f", "lavfi", "-i", "color=c=white:s=32x24:r=10:d=1",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1", "-shortest",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-movflags", "frag_keyframe+empty_moov", "-f", "mp4", "pipe:1")
	source, err := sourceCommand.Output()
	if err != nil {
		t.Fatalf("generate source with audio: %v", err)
	}
	var output bytes.Buffer
	if err := redactor.Redact(context.Background(), bytes.NewReader(source), &output, nil); err != nil {
		t.Fatalf("normalize approved video: %v", err)
	}
	probe := exec.Command(ffprobe, "-v", "error", "-select_streams", "a", "-show_entries", "stream=index", "-of", "csv=p=0", "pipe:0")
	probe.Stdin = bytes.NewReader(output.Bytes())
	audioStreams, err := probe.Output()
	if err != nil {
		t.Fatalf("inspect normalized audio streams: %v", err)
	}
	if strings.TrimSpace(string(audioStreams)) != "" {
		t.Fatalf("approved derived video retained audio streams: %q", audioStreams)
	}
}

func TestFFmpegRedactorRequiresExecutable(t *testing.T) {
	if _, err := NewFFmpegRedactor("definitely-not-a-real-ffmpeg-binary"); err == nil {
		t.Fatal(fmt.Errorf("missing ffmpeg executable was accepted"))
	}
}
