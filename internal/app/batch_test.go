package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
)

const (
	appFFmpegHelperEnv = "SUBKIT_APP_TEST_FFMPEG_HELPER"
	appFFmpegExeEnv    = "SUBKIT_APP_TEST_FFMPEG_EXE"
)

func TestResolveInputsExpandsGlobsDeduplicatesAndPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldwd)
	})

	writeTestFile(t, "b.mp4")
	writeTestFile(t, "a.mp4")

	got, err := resolveInputs([]string{"b.mp4", "*.mp4", "a.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"b.mp4", "a.mp4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("inputs = %#v, want %#v", got, want)
	}
}

func TestSummarizeOutputs(t *testing.T) {
	cases := []struct {
		name       string
		outputs    []outputResult
		wantDetail string
		wantCached bool
	}{
		{
			name:       "no outputs",
			outputs:    nil,
			wantDetail: "done",
			wantCached: false,
		},
		{
			name:       "single written",
			outputs:    []outputResult{{Path: filepath.Join("out", "clip.srt"), Copied: true}},
			wantDetail: "wrote clip.srt",
			wantCached: false,
		},
		{
			name:       "single force-written",
			outputs:    []outputResult{{Path: "clip.srt", ForceWrote: true}},
			wantDetail: "wrote clip.srt",
			wantCached: false,
		},
		{
			name:       "single cached",
			outputs:    []outputResult{{Path: "clip.srt"}},
			wantDetail: "cached",
			wantCached: true,
		},
		{
			name:       "mixed counts as written",
			outputs:    []outputResult{{Path: "a.srt", Copied: true}, {Path: "b.vtt"}},
			wantDetail: "wrote 1 files",
			wantCached: false,
		},
		{
			name:       "artifact only is ready",
			outputs:    []outputResult{{Path: "cache/audio.wav", ArtifactOnly: true}},
			wantDetail: "ready",
			wantCached: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail, cached := summarizeOutputs(tc.outputs)
			if detail != tc.wantDetail || cached != tc.wantCached {
				t.Fatalf("summarizeOutputs = (%q, %v), want (%q, %v)", detail, cached, tc.wantDetail, tc.wantCached)
			}
		})
	}
}

func TestResolveInputsRejectsUnmatchedGlobAndDirectories(t *testing.T) {
	dir := t.TempDir()
	if _, err := resolveInputs([]string{filepath.Join(dir, "*.mp4")}); err == nil {
		t.Fatal("expected unmatched glob error")
	}
	if err := os.Mkdir(filepath.Join(dir, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInputs([]string{filepath.Join(dir, "media")}); err == nil {
		t.Fatal("expected directory rejection")
	}
}

func TestOutputTemplateAndCollisionPlanning(t *testing.T) {
	input := filepath.Join("media", "movie.mp4")
	template := filepath.Join("{dir}", "out", "{base}.{kind}.{format}")
	got := renderOutputTemplate(template, input, "subtitle", "srt")
	want := filepath.Join("media", "out", "movie.subtitle.srt")
	if got != want {
		t.Fatalf("template = %q, want %q", got, want)
	}

	dir := t.TempDir()
	first := filepath.Join(dir, "one", "movie.mp4")
	second := filepath.Join(dir, "two", "movie.mp4")
	if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(second), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := planSubtitleJobs([]string{first, second}, []string{"srt"}, filepath.Join(dir, "out"), "")
	if err == nil || !strings.Contains(err.Error(), "output path collision") {
		t.Fatalf("expected output collision, got %v", err)
	}
}

func TestArtifactOutRejectedForMultipleInputs(t *testing.T) {
	_, err := planArtifactJobs([]string{"a.mp4", "b.mp4"}, "audio", "flac", "one.flac", "", "")
	if err == nil || !strings.Contains(err.Error(), "--out cannot be used") {
		t.Fatalf("expected --out batch rejection, got %v", err)
	}
}

func TestRunBatchHonorsDefaultAndCustomConcurrency(t *testing.T) {
	tests := []struct {
		name        string
		concurrency int
		wantMax     int64
	}{
		{name: "default", concurrency: 0, wantMax: defaultConcurrency},
		{name: "custom", concurrency: 2, wantMax: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var active int64
			var maxActive int64
			process := func(ctx context.Context, job batchJob, opts pipeline.Options, reporter pipeline.Reporter) ([]outputResult, error) {
				current := atomic.AddInt64(&active, 1)
				for {
					observed := atomic.LoadInt64(&maxActive)
					if current <= observed || atomic.CompareAndSwapInt64(&maxActive, observed, current) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt64(&active, -1)
				return []outputResult{{Path: job.Input + ".out", Copied: true}}, nil
			}

			jobs := make([]batchJob, 8)
			for i := range jobs {
				jobs[i] = batchJob{Input: string(rune('a' + i))}
			}
			var out bytes.Buffer
			err := runBatchWithProcessor(context.Background(), &out, pipeline.DefaultOptions(), batchFlags{
				Concurrency: tt.concurrency,
				Progress:    progressOff,
			}, jobs, process)
			if err != nil {
				t.Fatal(err)
			}
			if maxActive != tt.wantMax {
				t.Fatalf("max concurrency = %d, want %d", maxActive, tt.wantMax)
			}
		})
	}
}

func TestRunBatchAggregatesFailuresAndContinues(t *testing.T) {
	var processed int64
	process := func(ctx context.Context, job batchJob, opts pipeline.Options, reporter pipeline.Reporter) ([]outputResult, error) {
		atomic.AddInt64(&processed, 1)
		if job.Input == "bad.mp4" {
			return nil, errors.New("decode failed")
		}
		return []outputResult{{Path: job.Input + ".srt", Copied: true}}, nil
	}

	jobs := []batchJob{{Input: "good-a.mp4"}, {Input: "bad.mp4"}, {Input: "good-b.mp4"}}
	var out bytes.Buffer
	err := runBatchWithProcessor(context.Background(), &out, pipeline.DefaultOptions(), batchFlags{
		Concurrency: 2,
		Progress:    progressOff,
	}, jobs, process)
	if err == nil {
		t.Fatal("expected batch error")
	}
	var batchErr batchError
	if !errors.As(err, &batchErr) || len(batchErr.Failures) != 1 {
		t.Fatalf("error = %#v, want one batch failure", err)
	}
	if processed != int64(len(jobs)) {
		t.Fatalf("processed = %d, want %d", processed, len(jobs))
	}
	if !strings.Contains(out.String(), "errors:") || !strings.Contains(out.String(), "bad.mp4: decode failed") {
		t.Fatalf("missing failure summary: %q", out.String())
	}
}

func TestPlainReporterPrefixesBatchEvents(t *testing.T) {
	var out bytes.Buffer
	reporter := &plainBatchReporter{out: &out, prefix: true}
	reporter.Report(batchEvent{Input: filepath.Join("media", "movie.mp4"), Stage: pipeline.StageAudio, Message: "extracting"})

	got := out.String()
	if !strings.Contains(got, "[movie.mp4] audio: extracting") {
		t.Fatalf("plain output = %q", got)
	}
}

func TestRootCommandRejectsOutWithMultipleInputs(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.mp4")
	second := filepath.Join(dir, "b.mp4")
	writeTestFile(t, first)
	writeTestFile(t, second)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"extract-audio", first, second, "--out", filepath.Join(dir, "one.flac"), "--progress", "off"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--out cannot be used") {
		t.Fatalf("expected --out rejection, got %v", err)
	}
}

func TestRootCommandRejectsExtractAudioWithoutOutputByDefault(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "movie.mp4")
	writeTestFile(t, input)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--cache-dir", filepath.Join(dir, "cache"), "extract-audio", input, "--progress", "off"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "extract-audio requires --out") {
		t.Fatalf("expected missing output rejection, got %v", err)
	}
}

func TestRootCommandExtractAudioExplicitOutputAvoidsPersistentAudioCache(t *testing.T) {
	dir := t.TempDir()
	installAppFakeFFmpeg(t, dir)
	input := filepath.Join(dir, "movie.mp4")
	output := filepath.Join(dir, "movie.flac")
	cacheDir := filepath.Join(dir, "cache")
	writeTestFile(t, input)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--cache-dir", cacheDir, "extract-audio", input, "--out", output, "--progress", "off"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "audio" {
		t.Fatalf("output audio = %q, want fake audio content", string(data))
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "audio")); !os.IsNotExist(err) {
		t.Fatalf("persistent audio cache dir exists unexpectedly: %v", err)
	}
}

func TestRootCommandExtractAudioCacheAudioAllowsArtifactOnly(t *testing.T) {
	dir := t.TempDir()
	installAppFakeFFmpeg(t, dir)
	input := filepath.Join(dir, "movie.mp4")
	cacheDir := filepath.Join(dir, "cache")
	writeTestFile(t, input)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--cache-dir", cacheDir, "--cache-audio", "extract-audio", input, "--progress", "off"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(cacheDir, "audio"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("persistent audio cache entries = %d, want 1", len(entries))
	}
	if !strings.Contains(out.String(), entries[0].Name()) {
		t.Fatalf("command output %q did not include cached artifact name %q", out.String(), entries[0].Name())
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installAppFakeFFmpeg(t *testing.T, dir string) {
	t.Helper()

	t.Setenv(appFFmpegHelperEnv, "1")
	t.Setenv(appFFmpegExeEnv, os.Args[0])
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "ffmpeg.bat")
		script := "@echo off\r\n\"%" + appFFmpegExeEnv + "%\" -test.run=TestHelperProcess -- %*\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		path := filepath.Join(dir, "ffmpeg")
		script := "#!/bin/sh\nexec \"$" + appFFmpegExeEnv + "\" -test.run=TestHelperProcess -- \"$@\"\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv(appFFmpegHelperEnv) != "1" {
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
		_, _ = os.Stdout.WriteString("ffmpeg version subkit-app-test\n")
		os.Exit(0)
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
