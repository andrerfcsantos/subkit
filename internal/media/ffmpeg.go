package media

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type AudioOptions struct {
	Format     string `json:"format"`
	Stream     int    `json:"stream"`
	Channels   int    `json:"channels"`
	SampleRate int    `json:"sample_rate,omitempty"`
}

func DefaultAudioOptions() AudioOptions {
	return AudioOptions{
		Format:   "flac",
		Stream:   0,
		Channels: 1,
	}
}

func ExtractAudio(ctx context.Context, input string, output string, opts AudioOptions) error {
	if opts.Format == "" {
		opts.Format = "flac"
	}
	if opts.Channels <= 0 {
		opts.Channels = 1
	}
	if opts.Stream < 0 {
		opts.Stream = 0
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", input,
		"-map", fmt.Sprintf("0:a:%d", opts.Stream),
		"-vn",
		"-ac", strconv.Itoa(opts.Channels),
	}
	if opts.SampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(opts.SampleRate))
	}
	args = append(args, audioCodecArgs(opts.Format)...)
	args = append(args, output)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg extract audio failed: %w: %s", err, strings.TrimSpace(string(combined)))
	}
	return nil
}

func FFmpegVersion(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-version")
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	firstLine, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(firstLine)
}

func AudioContentType(format string) string {
	switch strings.ToLower(format) {
	case "flac":
		return "audio/flac"
	case "wav", "wave":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "m4a", "mp4":
		return "audio/mp4"
	default:
		return "application/octet-stream"
	}
}

func AudioExtension(format string) string {
	format = strings.ToLower(strings.TrimPrefix(format, "."))
	switch format {
	case "wave":
		return "wav"
	case "":
		return "flac"
	default:
		return format
	}
}

func audioCodecArgs(format string) []string {
	switch strings.ToLower(format) {
	case "flac":
		return []string{"-c:a", "flac"}
	case "wav", "wave":
		return []string{"-c:a", "pcm_s16le"}
	case "mp3":
		return []string{"-c:a", "libmp3lame"}
	case "m4a", "mp4":
		return []string{"-c:a", "aac"}
	default:
		return nil
	}
}
