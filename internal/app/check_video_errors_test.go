package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrerfcsantos/subkit-codex/internal/media"
)

func TestCheckVideoErrorsCommandIsHidden(t *testing.T) {
	root := NewRootCommand()
	command, _, err := root.Find([]string{"check-video-errors"})
	if err != nil {
		t.Fatal(err)
	}
	if !command.Hidden {
		t.Fatal("check-video-errors command should be hidden")
	}
	var help bytes.Buffer
	root.SetOut(&help)
	if err := root.Help(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(help.Bytes(), []byte("check-video-errors")) {
		t.Fatal("hidden command appears in root help")
	}
}

func TestResolveVideoCheckInputsSupportsGlobstarAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "season", "disc")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(nested, "clip-1.mp4")
	if err := os.WriteFile(clip, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "ignore.txt"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	pattern := filepath.Join(root, "**", "clip-?.mp4")
	inputs, err := resolveVideoCheckInputs([]string{clip, pattern}, parseVideoExtensions("mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 {
		t.Fatalf("inputs = %v, want one deduplicated video", inputs)
	}
	if inputs[0] != clip {
		t.Fatalf("input = %q, want %q", inputs[0], clip)
	}
}

func TestRunVideoChecksHonorsConcurrency(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	checker := func(ctx context.Context, path string, opts media.VideoCheckOptions, reporter media.VideoCheckReporter) (media.VideoCheckResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		return media.VideoCheckResult{Path: path, Status: media.VideoStatusLikelyOK, Reason: "sampled"}, nil
	}

	flags := checkVideoFlags{Options: media.DefaultVideoCheckOptions(), Concurrency: 2, Progress: progressOff}
	inputs := []string{"a.mp4", "b.mp4", "c.mp4", "d.mp4", "e.mp4"}
	if err := runVideoChecksWithChecker(context.Background(), &bytes.Buffer{}, flags, inputs, checker); err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", got)
	}
}

func TestRunVideoChecksRetriesTimeoutsSerially(t *testing.T) {
	var calls atomic.Int32
	checker := func(ctx context.Context, path string, opts media.VideoCheckOptions, reporter media.VideoCheckReporter) (media.VideoCheckResult, error) {
		if calls.Add(1) == 1 {
			return media.VideoCheckResult{
				Path:   path,
				Status: media.VideoStatusInconclusive,
				Reason: "tail decode exceeded 2m0s",
				Checks: []media.VideoDecodeCheck{{Kind: "tail", State: "timeout"}},
			}, nil
		}
		return media.VideoCheckResult{Path: path, Status: media.VideoStatusLikelyOK, Reason: "sampled"}, nil
	}

	flags := checkVideoFlags{Options: media.DefaultVideoCheckOptions(), Concurrency: 1, Progress: progressOff, RetryTimeouts: true}
	var out bytes.Buffer
	if err := runVideoChecksWithChecker(context.Background(), &out, flags, []string{"a.mp4"}, checker); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("checker calls = %d, want initial attempt and serial retry", calls.Load())
	}
	if !strings.Contains(out.String(), "LIKELY_OK=1") || strings.Contains(out.String(), "[LIKELY_OK") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}

func TestRunVideoChecksSuppressesSuccessAndPrintsWarningsByDefault(t *testing.T) {
	checker := func(ctx context.Context, path string, opts media.VideoCheckOptions, reporter media.VideoCheckReporter) (media.VideoCheckResult, error) {
		if path == "warning.mp4" {
			return media.VideoCheckResult{Path: path, Status: media.VideoStatusNonconformant, Reason: "strict validation failed; practical decode succeeded"}, nil
		}
		return media.VideoCheckResult{Path: path, Status: media.VideoStatusLikelyOK, Reason: "sampled"}, nil
	}

	flags := checkVideoFlags{Options: media.DefaultVideoCheckOptions(), Concurrency: 2, Progress: progressOff}
	var out bytes.Buffer
	if err := runVideoChecksWithChecker(context.Background(), &out, flags, []string{"ok.mp4", "warning.mp4"}, checker); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "[LIKELY_OK") || !strings.Contains(out.String(), "[NONCONFORMANT") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}
