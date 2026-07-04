package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	pipelineFFmpegHelperEnv = "SUBKIT_PIPELINE_TEST_FFMPEG_HELPER"
	pipelineFFmpegExeEnv    = "SUBKIT_PIPELINE_TEST_FFMPEG_EXE"
	pipelineFFmpegLogEnv    = "SUBKIT_PIPELINE_TEST_FFMPEG_LOG"
)

func TestEnsureAudioUsesTemporaryArtifactByDefault(t *testing.T) {
	dir := t.TempDir()
	logPath := installPipelineFakeFFmpeg(t, dir)
	input := writePipelineTestFile(t, filepath.Join(dir, "movie.mp4"))
	cacheDir := filepath.Join(dir, "cache")

	opts := DefaultOptions()
	opts.Cache.Dir = cacheDir
	runner, err := NewRunner(opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	artifact, err := runner.EnsureAudio(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.FromCache {
		t.Fatal("temporary audio artifact was marked as cache hit")
	}
	if pathWithin(artifact.Path, filepath.Join(cacheDir, "audio")) {
		t.Fatalf("audio artifact path = %q, want outside persistent audio cache", artifact.Path)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "audio")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("persistent audio cache dir exists unexpectedly: %v", err)
	}
	if _, err := os.Stat(artifact.Path); err != nil {
		t.Fatalf("temporary audio artifact was not created: %v", err)
	}
	if got := countPipelineExtractions(t, logPath); got != 1 {
		t.Fatalf("ffmpeg extractions = %d, want 1", got)
	}

	tempPath := artifact.Path
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary audio artifact survived runner cleanup: %v", err)
	}
}

func TestEnsureAudioCachesWhenOptedIn(t *testing.T) {
	dir := t.TempDir()
	logPath := installPipelineFakeFFmpeg(t, dir)
	input := writePipelineTestFile(t, filepath.Join(dir, "movie.mp4"))
	cacheDir := filepath.Join(dir, "cache")

	opts := DefaultOptions()
	opts.Cache.Dir = cacheDir
	opts.Cache.CacheAudio = true

	runner, err := NewRunner(opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := runner.EnsureAudio(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if !pathWithin(artifact.Path, filepath.Join(cacheDir, "audio")) {
		t.Fatalf("audio artifact path = %q, want persistent audio cache", artifact.Path)
	}
	if artifact.FromCache {
		t.Fatal("first audio extraction was marked as cache hit")
	}
	if got := countPipelineExtractions(t, logPath); got != 1 {
		t.Fatalf("ffmpeg extractions after first run = %d, want 1", got)
	}

	secondRunner, err := NewRunner(opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	secondArtifact, err := secondRunner.EnsureAudio(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := secondRunner.Close(); err != nil {
		t.Fatal(err)
	}
	if !secondArtifact.FromCache {
		t.Fatal("second audio extraction did not hit persistent cache")
	}
	if secondArtifact.Path != artifact.Path {
		t.Fatalf("second artifact path = %q, want %q", secondArtifact.Path, artifact.Path)
	}
	if got := countPipelineExtractions(t, logPath); got != 1 {
		t.Fatalf("ffmpeg extractions after cache hit = %d, want 1", got)
	}
}

func TestEnsureTranscriptCacheHitDoesNotExtractAudioAgain(t *testing.T) {
	dir := t.TempDir()
	logPath := installPipelineFakeFFmpeg(t, dir)
	input := writePipelineTestFile(t, filepath.Join(dir, "movie.mp4"))
	cacheDir := filepath.Join(dir, "cache")

	var requests int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{
				"request_id": "req-1",
				"duration":   1.0,
				"channels":   1,
				"models":     []string{"model-1"},
				"model_info": map[string]any{
					"model-1": map[string]any{"name": "test-model", "version": "v1"},
				},
			},
			"results": map[string]any{
				"channels": []any{
					map[string]any{
						"detected_language": "en",
						"alternatives": []any{
							map[string]any{
								"transcript": "hello",
								"words": []any{
									map[string]any{"word": "hello", "start": 0, "end": 1},
								},
							},
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	opts := DefaultOptions()
	opts.Cache.Dir = cacheDir
	opts.Deepgram.Endpoint = server.URL
	opts.Deepgram.APIKeyEnvName = "SUBKIT_PIPELINE_TEST_DEEPGRAM_KEY"
	t.Setenv(opts.Deepgram.APIKeyEnvName, "test-key")

	runner, err := NewRunner(opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	artifact, _, err := runner.EnsureTranscript(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if artifact.FromCache {
		t.Fatal("first transcript call was marked as cache hit")
	}
	if got := countPipelineExtractions(t, logPath); got != 1 {
		t.Fatalf("ffmpeg extractions after first transcript = %d, want 1", got)
	}

	secondRunner, err := NewRunner(opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	secondArtifact, _, err := secondRunner.EnsureTranscript(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := secondRunner.Close(); err != nil {
		t.Fatal(err)
	}
	if !secondArtifact.FromCache {
		t.Fatal("second transcript call did not hit normalized transcript cache")
	}
	if got := countPipelineExtractions(t, logPath); got != 1 {
		t.Fatalf("ffmpeg extractions after transcript cache hit = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&requests); got != 1 {
		t.Fatalf("Deepgram requests = %d, want 1", got)
	}
}

func installPipelineFakeFFmpeg(t *testing.T, dir string) string {
	t.Helper()

	t.Setenv(pipelineFFmpegHelperEnv, "1")
	t.Setenv(pipelineFFmpegExeEnv, os.Args[0])
	logPath := filepath.Join(dir, "ffmpeg.log")
	t.Setenv(pipelineFFmpegLogEnv, logPath)

	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "ffmpeg.bat")
		script := "@echo off\r\n\"%" + pipelineFFmpegExeEnv + "%\" -test.run=TestHelperProcess -- %*\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		path := filepath.Join(dir, "ffmpeg")
		script := "#!/bin/sh\nexec \"$" + pipelineFFmpegExeEnv + "\" -test.run=TestHelperProcess -- \"$@\"\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv(pipelineFFmpegHelperEnv) != "1" {
		return
	}

	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:]
	}

	if len(args) > 0 && args[0] == "-version" {
		fmt.Fprintln(os.Stdout, "ffmpeg version subkit-pipeline-test")
		os.Exit(0)
	}

	if logPath := os.Getenv(pipelineFFmpegLogEnv); logPath != "" {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			os.Exit(1)
		}
		if _, err := file.WriteString("extract\n"); err != nil {
			_ = file.Close()
			os.Exit(1)
		}
		if err := file.Close(); err != nil {
			os.Exit(1)
		}
	}

	if len(args) == 0 {
		os.Exit(1)
	}
	output := args[len(args)-1]
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		os.Exit(1)
	}
	if err := os.WriteFile(output, []byte("audio"), 0o644); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func writePipelineTestFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func countPipelineExtractions(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "extract\n")
}

func pathWithin(path string, dir string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}
