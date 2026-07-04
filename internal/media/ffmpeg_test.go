package media

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestExtractAudioSpecifiesMuxerFormat(t *testing.T) {
	dir := t.TempDir()
	installFakeFFmpeg(t, dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logPath := filepath.Join(dir, "ffmpeg-args.txt")
	t.Setenv("TEST_FFMPEG_LOG", logPath)

	outputPath := filepath.Join(dir, ".artifact.flac.tmp-123")
	err := ExtractAudio(context.Background(), "input.mp4", outputPath, AudioOptions{Format: "flac", Channels: 1})
	if err != nil {
		t.Fatalf("ExtractAudio() error = %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	wantSuffix := []string{"-c:a", "flac", "-f", "flac", outputPath}
	if len(args) < len(wantSuffix) || !slices.Equal(args[len(args)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("ffmpeg args suffix = %#v, want %#v", args, wantSuffix)
	}
}

func installFakeFFmpeg(t *testing.T, dir string) {
	t.Helper()

	t.Setenv("TEST_FFMPEG_HELPER", os.Args[0])
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "ffmpeg.bat")
		script := "@echo off\r\n\"%TEST_FFMPEG_HELPER%\" -test.run=TestHelperProcess -- %*\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return
	}

	path := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nexec \"$TEST_FFMPEG_HELPER\" -test.run=TestHelperProcess -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("TEST_FFMPEG_LOG") == "" {
		return
	}

	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:]
	}

	if err := os.WriteFile(os.Getenv("TEST_FFMPEG_LOG"), []byte(strings.Join(args, "\n")), 0o644); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
