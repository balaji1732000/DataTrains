package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"trajectory.local/api/internal/domain"
)

type VideoRedactor interface {
	Redact(context.Context, io.Reader, io.Writer, []domain.RedactionRegion) error
}

type FFmpegRedactor struct {
	executable string
	timeout    time.Duration
}

func NewFFmpegRedactor(executable string) (*FFmpegRedactor, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		executable = "ffmpeg"
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg is required for video redaction: %w", err)
	}
	return &FFmpegRedactor{executable: path, timeout: 15 * time.Minute}, nil
}

func (redactor *FFmpegRedactor) Redact(ctx context.Context, input io.Reader, output io.Writer, regions []domain.RedactionRegion) error {
	if redactor == nil || redactor.executable == "" || input == nil || output == nil {
		return errors.New("ffmpeg redactor and media input/output are required")
	}
	if len(regions) > 100 {
		return errors.New("no more than 100 redaction regions are allowed")
	}
	redactionContext, cancel := context.WithTimeout(ctx, redactor.timeout)
	defer cancel()
	arguments := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", "pipe:0", "-map", "0:v:0", "-an",
	}
	if len(regions) > 0 {
		filter, err := redactionFilter(regions)
		if err != nil {
			return err
		}
		arguments = append(arguments, "-vf", filter)
	}
	arguments = append(arguments,
		"-c:v", "libx264", "-preset", "medium", "-crf", "18", "-pix_fmt", "yuv420p",
		"-movflags", "frag_keyframe+empty_moov", "-f", "mp4", "pipe:1")
	command := exec.CommandContext(redactionContext, redactor.executable, arguments...)
	command.Stdin = input
	command.Stdout = output
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if errors.Is(redactionContext.Err(), context.DeadlineExceeded) {
			return errors.New("ffmpeg redaction timed out")
		}
		return fmt.Errorf("ffmpeg redaction failed: %s", safeProbeMessage(stderr.String(), err.Error()))
	}
	return nil
}

func redactionFilter(regions []domain.RedactionRegion) (string, error) {
	if len(regions) == 0 || len(regions) > 100 {
		return "", errors.New("between one and 100 redaction regions are required")
	}
	filters := make([]string, len(regions))
	for index, region := range regions {
		if region.StartNS < 0 || region.EndNS <= region.StartNS || region.X < 0 || region.Y < 0 ||
			region.Width <= 0 || region.Height <= 0 || region.X+region.Width > 1 || region.Y+region.Height > 1 {
			return "", fmt.Errorf("region %d has invalid time or geometry", index)
		}
		start := strconv.FormatFloat(float64(region.StartNS)/1e9, 'f', 9, 64)
		end := strconv.FormatFloat(float64(region.EndNS)/1e9, 'f', 9, 64)
		x := strconv.FormatFloat(region.X, 'f', 9, 64)
		y := strconv.FormatFloat(region.Y, 'f', 9, 64)
		width := strconv.FormatFloat(region.Width, 'f', 9, 64)
		height := strconv.FormatFloat(region.Height, 'f', 9, 64)
		filters[index] = "drawbox=x=iw*" + x + ":y=ih*" + y + ":w=iw*" + width + ":h=ih*" + height +
			":color=black:t=fill:enable='between(t," + start + "," + end + ")'"
	}
	return strings.Join(filters, ","), nil
}
