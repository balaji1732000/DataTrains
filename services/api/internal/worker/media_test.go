package worker

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestFFprobeInspectorAcceptsH264MP4AndRejectsCorruptMedia(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed on this test host")
	}
	inspector, err := NewFFprobeInspector("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed on this test host")
	}
	command := exec.Command(ffmpeg,
		"-v", "error", "-f", "lavfi", "-i", "color=c=black:s=32x24:r=10:d=1",
		"-an", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-movflags", "frag_keyframe+empty_moov", "-f", "mp4", "pipe:1")
	media, err := command.Output()
	if err != nil {
		t.Fatalf("generate H.264 fixture: %v", err)
	}
	info, err := inspector.InspectMP4(context.Background(), bytes.NewReader(media))
	if err != nil {
		t.Fatalf("inspect valid H.264 MP4: %v", err)
	}
	if info.Codec != "h264" || info.Width != 32 || info.Height != 24 || info.DurationSeconds < 0.9 || info.DurationSeconds > 1.1 {
		t.Fatalf("unexpected media info: %#v", info)
	}
	if _, err := inspector.InspectMP4(context.Background(), strings.NewReader("not an MP4")); err == nil || !strings.Contains(err.Error(), "rejected media") {
		t.Fatalf("corrupt media error = %v", err)
	}
}

func TestFFprobeInspectorRequiresExecutable(t *testing.T) {
	if _, err := NewFFprobeInspector("definitely-not-a-real-ffprobe-binary"); err == nil {
		t.Fatal("missing ffprobe executable was accepted")
	}
}
