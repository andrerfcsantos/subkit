package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
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

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
}
