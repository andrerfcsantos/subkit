package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestCheckVideoQuickLikelyOK(t *testing.T) {
	path, opts := videoCheckFixture(t, 100)
	result, err := CheckVideo(context.Background(), path, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != VideoStatusLikelyOK {
		t.Fatalf("status = %s, want %s: %s", result.Status, VideoStatusLikelyOK, result.Reason)
	}
	if len(result.Checks) != 4 {
		t.Fatalf("checks = %d, want tail + 3 samples", len(result.Checks))
	}
}

func TestCheckVideoQuickFindsPersistentFailure(t *testing.T) {
	path, opts := videoCheckFixture(t, 10)
	var stages []string
	result, err := CheckVideo(context.Background(), path, opts, func(event VideoCheckEvent) {
		stages = append(stages, event.Stage)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != VideoStatusCorrupt {
		t.Fatalf("status = %s, want %s: %s", result.Status, VideoStatusCorrupt, result.Reason)
	}
	if result.FailureEstimateSeconds == nil || *result.FailureEstimateSeconds < 10 || *result.FailureEstimateSeconds > 10.25 {
		t.Fatalf("failure estimate = %v, want 10.0..10.25", result.FailureEstimateSeconds)
	}
	if !strings.Contains(result.Reason, "Invalid NAL unit size") {
		t.Fatalf("reason = %q", result.Reason)
	}
	if !slicesContain(stages, "locate") {
		t.Fatalf("stages = %v, want locate", stages)
	}
}

func TestFormatVideoTimestamp(t *testing.T) {
	if got := FormatVideoTimestamp(839.405); got != "13:59.405" {
		t.Fatalf("FormatVideoTimestamp() = %q", got)
	}
}

func videoCheckFixture(t *testing.T, failAt float64) (string, VideoCheckOptions) {
	t.Helper()
	dir := t.TempDir()
	input := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(input, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	ffprobe := installVideoCheckTool(t, dir, "ffprobe")
	ffmpeg := installVideoCheckTool(t, dir, "ffmpeg")
	t.Setenv("TEST_VIDEO_CHECK_HELPER", "1")
	t.Setenv("TEST_VIDEO_FAIL_AT", strconv.FormatFloat(failAt, 'f', -1, 64))

	opts := DefaultVideoCheckOptions()
	opts.FFprobePath = ffprobe
	opts.FFmpegPath = ffmpeg
	return input, opts
}

func installVideoCheckTool(t *testing.T, dir string, tool string) string {
	t.Helper()
	t.Setenv("TEST_VIDEO_CHECK_BINARY", os.Args[0])
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, tool+".bat")
		script := fmt.Sprintf("@echo off\r\nset TEST_VIDEO_TOOL=%s\r\n\"%%TEST_VIDEO_CHECK_BINARY%%\" -test.run=TestVideoCheckHelperProcess -- %%*\r\n", tool)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := filepath.Join(dir, tool)
	script := fmt.Sprintf("#!/bin/sh\nTEST_VIDEO_TOOL=%s exec \"$TEST_VIDEO_CHECK_BINARY\" -test.run=TestVideoCheckHelperProcess -- \"$@\"\n", tool)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVideoCheckHelperProcess(t *testing.T) {
	if os.Getenv("TEST_VIDEO_CHECK_HELPER") != "1" {
		return
	}
	if os.Getenv("TEST_VIDEO_TOOL") == "ffprobe" {
		fmt.Print(`{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","duration":"20.0","disposition":{"attached_pic":0}}],"format":{"duration":"20.0","size":"1000"}}`)
		os.Exit(0)
	}

	start := 0.0
	for i, arg := range os.Args {
		if arg == "-ss" && i+1 < len(os.Args) {
			start, _ = strconv.ParseFloat(os.Args[i+1], 64)
			break
		}
	}
	failAt, _ := strconv.ParseFloat(os.Getenv("TEST_VIDEO_FAIL_AT"), 64)
	if start >= failAt {
		fmt.Fprintln(os.Stderr, "[hevc] Invalid NAL unit size (999 > 12).")
		fmt.Fprintln(os.Stderr, "[hevc] Error splitting the input into NAL units.")
		os.Exit(1)
	}
	fmt.Print("frame=1\nprogress=end\n")
	os.Exit(0)
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
