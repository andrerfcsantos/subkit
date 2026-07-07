package app

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
)

func applyUpdate(t *testing.T, model progressModel, msg tea.Msg) progressModel {
	t.Helper()
	updated, _ := model.Update(msg)
	next, ok := updated.(progressModel)
	if !ok {
		t.Fatalf("Update returned %T, want progressModel", updated)
	}
	return next
}

func TestProgressModelUpdatesRowFromRunningEvent(t *testing.T) {
	model := newProgressModel([]batchJob{{Input: "movie.mp4"}}, 4, nil)
	model = applyUpdate(t, model, progressEventMsg(batchEvent{
		Input:   "movie.mp4",
		Stage:   pipeline.StageTranscribe,
		Message: "calling Deepgram",
	}))

	row := model.rows[0]
	if row.State != stateRunning {
		t.Fatalf("state = %v, want running", row.State)
	}
	if row.Detail != "calling Deepgram" {
		t.Fatalf("detail = %q", row.Detail)
	}
	if model.counts[stateRunning] != 1 || model.counts[stateQueued] != 0 {
		t.Fatalf("counts = %v", model.counts)
	}

	view := model.View().Content
	if !strings.Contains(view, "movie.mp4") || !strings.Contains(view, "calling Deepgram") {
		t.Fatalf("view missing row update: %q", view)
	}
	if !strings.Contains(view, "ACTIVE") {
		t.Fatalf("view missing ACTIVE section: %q", view)
	}
}

func TestProgressModelClassifiesDoneVsCached(t *testing.T) {
	model := newProgressModel([]batchJob{{Input: "a.mp4"}, {Input: "b.mp4"}}, 4, nil)

	model = applyUpdate(t, model, progressEventMsg(batchEvent{
		Input: "a.mp4", Stage: pipeline.StageDone, Message: "done", Detail: "wrote a.srt", Cached: false,
	}))
	model = applyUpdate(t, model, progressEventMsg(batchEvent{
		Input: "b.mp4", Stage: pipeline.StageDone, Message: "done", Detail: "cached", Cached: true,
	}))

	if model.rows[0].State != stateDone {
		t.Fatalf("a state = %v, want done", model.rows[0].State)
	}
	if model.rows[1].State != stateCached {
		t.Fatalf("b state = %v, want cached", model.rows[1].State)
	}
	if model.counts[stateDone] != 1 || model.counts[stateCached] != 1 || model.counts[stateQueued] != 0 {
		t.Fatalf("counts = %v", model.counts)
	}

	view := model.View().Content
	if !strings.Contains(view, "1 done") || !strings.Contains(view, "1 cached") {
		t.Fatalf("summary missing counts: %q", view)
	}
	if !strings.Contains(view, "wrote a.srt") || !strings.Contains(view, "cached") {
		t.Fatalf("view missing details: %q", view)
	}
}

func TestProgressModelPinsRunningAndFailedWhilePaging(t *testing.T) {
	jobs := []batchJob{{Input: "run.mp4"}, {Input: "fail.mp4"}}
	for i := 0; i < 8; i++ {
		jobs = append(jobs, batchJob{Input: queuedName(i)})
	}
	model := newProgressModel(jobs, 4, nil)
	model = applyUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 14})

	model = applyUpdate(t, model, progressEventMsg(batchEvent{Input: "run.mp4", Stage: pipeline.StageTranscribe, Message: "transcribing"}))
	model = applyUpdate(t, model, progressEventMsg(batchEvent{Input: "fail.mp4", Stage: pipeline.StageFailed, Message: "deepgram: 401"}))

	if model.paginator.TotalPages < 2 {
		t.Fatalf("expected multiple pages, got %d (perPage=%d)", model.paginator.TotalPages, model.paginator.PerPage)
	}

	firstPage := model.View().Content
	if !strings.Contains(firstPage, "run.mp4") || !strings.Contains(firstPage, "fail.mp4") {
		t.Fatalf("pinned rows missing on first page: %q", firstPage)
	}
	if !strings.Contains(firstPage, "deepgram: 401") {
		t.Fatalf("failed detail missing: %q", firstPage)
	}
	firstQueued := queuedName(0)
	if !strings.Contains(firstPage, firstQueued) {
		t.Fatalf("first queued row missing on page 1: %q", firstPage)
	}

	// Page forward; pinned rows must remain, and the body must advance.
	model = applyUpdate(t, model, tea.KeyPressMsg(tea.Key{Text: "l", Code: 'l'}))
	if model.paginator.Page == 0 {
		t.Fatalf("expected page to advance")
	}
	secondPage := model.View().Content
	if !strings.Contains(secondPage, "run.mp4") || !strings.Contains(secondPage, "fail.mp4") {
		t.Fatalf("pinned rows missing on second page: %q", secondPage)
	}
	if strings.Contains(secondPage, firstQueued) {
		t.Fatalf("first queued row should have paged out: %q", secondPage)
	}
}

func TestProgressModelCancelKeyCancelsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := newProgressModel([]batchJob{{Input: "movie.mp4"}}, 4, cancel)

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

func TestTruncateDisplayClipsToWidth(t *testing.T) {
	got := truncateDisplay("a very long message that should be clipped", 20)
	if lipgloss.Width(got) > 20 {
		t.Fatalf("width = %d, want <= 20: %q", lipgloss.Width(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
}

func queuedName(i int) string {
	return "q" + string(rune('0'+i)) + ".mp4"
}
