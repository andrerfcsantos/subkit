package app

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
)

func TestProgressModelUpdatesRowsFromEvents(t *testing.T) {
	model := newProgressModel([]batchJob{{Input: "movie.mp4"}}, nil)
	updated, _ := model.Update(progressEventMsg(batchEvent{
		Input:   "movie.mp4",
		Stage:   pipeline.StageTranscribe,
		Message: "calling Deepgram",
	}))

	got := updated.(progressModel)
	row := got.rows[0]
	if row.Stage != pipeline.StageTranscribe || row.Message != "calling Deepgram" {
		t.Fatalf("row = %#v", row)
	}
	view := got.View().Content
	if !strings.Contains(view, "movie.mp4") || !strings.Contains(view, "calling Deepgram") {
		t.Fatalf("view missing row update: %q", view)
	}
}

func TestProgressModelTruncatesRows(t *testing.T) {
	row := progressRow{
		Input:   "very-long-movie-name-that-would-wrap.mp4",
		Stage:   pipeline.StageTranscribe,
		Message: "a very long message that should be clipped",
	}
	got := renderProgressRow(row, 24)
	if len(got) > 24 {
		t.Fatalf("row length = %d, want <= 24: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
}

func TestProgressModelCancelKeyCancelsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := newProgressModel([]batchJob{{Input: "movie.mp4"}}, cancel)

	updated, _ := model.Update(tea.KeyPressMsg(tea.Key{Text: "q", Code: 'q'}))
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected q to cancel context")
	}
	if !updated.(progressModel).closing {
		t.Fatal("expected model to be closing")
	}
}
