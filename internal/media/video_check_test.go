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
	"time"
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

func TestCheckVideoStrictFailureThatPracticalDecodeRecoversIsWarning(t *testing.T) {
	path, opts := videoCheckFixture(t, 100)
	opts.StrictBitstream = true
	opts.Samples = 0
	t.Setenv("TEST_VIDEO_STRICT_FAIL", "1")

	result, err := CheckVideo(context.Background(), path, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != VideoStatusNonconformant {
		t.Fatalf("status = %s, want %s: %s", result.Status, VideoStatusNonconformant, result.Reason)
	}
	if !result.Valid() {
		t.Fatal("nonconformant result should be a warning, not a batch failure")
	}
	if !strings.Contains(result.Reason, "practical decoding succeeded") {
		t.Fatalf("reason = %q", result.Reason)
	}
}

func TestCheckVideoEmptyTailLocatesTruncationBoundary(t *testing.T) {
	path, opts := videoCheckFixture(t, 100)
	t.Setenv("TEST_VIDEO_ZERO_AT", "12")

	result, err := CheckVideo(context.Background(), path, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != VideoStatusTruncated {
		t.Fatalf("status = %s, want %s: %s", result.Status, VideoStatusTruncated, result.Reason)
	}
	if result.FailureEstimateSeconds == nil || *result.FailureEstimateSeconds < 12 || *result.FailureEstimateSeconds > 12.25 {
		t.Fatalf("failure estimate = %v, want 12.0..12.25", result.FailureEstimateSeconds)
	}
	if strings.Contains(result.EstimateNote, "first independently decoded frame") {
		t.Fatalf("obsolete locator claim remains: %q", result.EstimateNote)
	}
}

func TestCheckVideoDoesNotInventZeroTimestampWhenStartupWindowFails(t *testing.T) {
	path, opts := videoCheckFixture(t, 0)

	result, err := CheckVideo(context.Background(), path, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != VideoStatusInconclusive {
		t.Fatalf("status = %s, want %s: %s", result.Status, VideoStatusInconclusive, result.Reason)
	}
	if result.FailureEstimateSeconds != nil {
		t.Fatalf("failure estimate = %v, want no invented timestamp", result.FailureEstimateSeconds)
	}
	if !strings.Contains(result.EstimateNote, "localization unavailable") {
		t.Fatalf("estimate note = %q", result.EstimateNote)
	}
}

func TestCheckVideoUsesProgressTimestampForFailureEstimate(t *testing.T) {
	path, opts := videoCheckFixture(t, 18)
	t.Setenv("TEST_VIDEO_FAIL_PROGRESS_US", "750000")

	result, err := CheckVideo(context.Background(), path, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != VideoStatusCorrupt {
		t.Fatalf("status = %s, want %s: %s", result.Status, VideoStatusCorrupt, result.Reason)
	}
	if result.FailureEstimateSeconds == nil || *result.FailureEstimateSeconds != 18.75 {
		t.Fatalf("failure estimate = %v, want 18.75", result.FailureEstimateSeconds)
	}
	if !strings.Contains(result.EstimateNote, "last successfully emitted") {
		t.Fatalf("estimate note = %q", result.EstimateNote)
	}
}

func TestCheckVideoTimeoutNamesStageAndCanBeRetried(t *testing.T) {
	path, opts := videoCheckFixture(t, 100)
	opts.Timeout = 250 * time.Millisecond
	t.Setenv("TEST_VIDEO_SLEEP_MS", "1000")

	result, err := CheckVideo(context.Background(), path, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != VideoStatusInconclusive || !result.TimedOut() {
		t.Fatalf("result = %#v, want timed-out inconclusive", result)
	}
	if !strings.Contains(result.Reason, "tail decode exceeded") || !strings.Contains(result.Reason, "frame(s)") {
		t.Fatalf("reason = %q", result.Reason)
	}
}

func TestCheckVideoPrefersVideoStreamDuration(t *testing.T) {
	path, opts := videoCheckFixture(t, 25)
	t.Setenv("TEST_VIDEO_FORMAT_DURATION", "30.0")

	result, err := CheckVideo(context.Background(), path, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.DurationSeconds != 20 {
		t.Fatalf("duration = %v, want video stream duration 20", result.DurationSeconds)
	}
	if result.Status != VideoStatusLikelyOK {
		t.Fatalf("status = %s, want %s: %s", result.Status, VideoStatusLikelyOK, result.Reason)
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
		formatDuration := os.Getenv("TEST_VIDEO_FORMAT_DURATION")
		if formatDuration == "" {
			formatDuration = "20.0"
		}
		fmt.Printf(`{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","duration":"20.0","disposition":{"attached_pic":0}}],"format":{"duration":%q,"size":"1000"}}`, formatDuration)
		os.Exit(0)
	}
	if sleepMS, _ := strconv.Atoi(os.Getenv("TEST_VIDEO_SLEEP_MS")); sleepMS > 0 {
		time.Sleep(time.Duration(sleepMS) * time.Millisecond)
	}

	start := 0.0
	for i, arg := range os.Args {
		if arg == "-ss" && i+1 < len(os.Args) {
			start, _ = strconv.ParseFloat(os.Args[i+1], 64)
			break
		}
	}
	failAt, _ := strconv.ParseFloat(os.Getenv("TEST_VIDEO_FAIL_AT"), 64)
	strict := slicesContain(os.Args, "explode")
	if os.Getenv("TEST_VIDEO_STRICT_FAIL") == "1" && strict {
		fmt.Fprintln(os.Stderr, "[hevc] Could not find ref with POC 0")
		fmt.Fprintln(os.Stderr, "[hevc] Error constructing the frame RPS.")
		os.Exit(1)
	}
	zeroAt, _ := strconv.ParseFloat(os.Getenv("TEST_VIDEO_ZERO_AT"), 64)
	if zeroAt > 0 && start >= zeroAt {
		fmt.Print("frame=0\nprogress=end\n")
		os.Exit(0)
	}
	if start >= failAt {
		if progressUS, _ := strconv.ParseInt(os.Getenv("TEST_VIDEO_FAIL_PROGRESS_US"), 10, 64); progressUS > 0 {
			fmt.Printf("frame=45\nout_time_us=%d\nprogress=end\n", progressUS)
		}
		fmt.Fprintln(os.Stderr, "[hevc] Invalid NAL unit size (999 > 12).")
		fmt.Fprintln(os.Stderr, "[hevc] Error splitting the input into NAL units.")
		os.Exit(1)
	}
	fmt.Print("frame=1\nout_time_us=500000\nprogress=end\n")
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
