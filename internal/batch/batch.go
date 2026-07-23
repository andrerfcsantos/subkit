// Package batch provides the generic machinery for processing many inputs
// concurrently with progress reporting: a bounded worker pool and reporters
// that render progress as an interactive TUI, plain logs, or nothing.
package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
)

// DefaultConcurrency is the default number of inputs processed at once.
const DefaultConcurrency = 4

// Progress display modes accepted by the --progress flag.
const (
	ProgressAuto  = "auto"
	ProgressTUI   = "tui"
	ProgressPlain = "plain"
	ProgressOff   = "off"
)

// NormalizeProgressMode lowercases and trims mode, defaulting empty to auto.
func NormalizeProgressMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return ProgressAuto
	}
	return mode
}

// ValidProgressMode reports whether mode is one of the progress constants.
func ValidProgressMode(mode string) bool {
	switch mode {
	case ProgressAuto, ProgressTUI, ProgressPlain, ProgressOff:
		return true
	}
	return false
}

// Event is a progress update about one input.
type Event struct {
	Input   string
	Stage   pipeline.Stage
	Message string
	Err     error

	// Detail and Cached are only populated on the terminal StageDone event and
	// are consumed exclusively by the TUI reporter. Detail is a human summary of
	// the outcome (e.g. "wrote clip.srt", "wrote 2 files", "cached"); Cached is
	// true when nothing new was written for the input. Message is left
	// untouched so the plain reporter is unaffected.
	Detail string
	Cached bool
}

// Reporter renders per-input progress events.
type Reporter interface {
	Report(Event)
	Close()
}

// Run processes jobs with at most workers concurrent goroutines, calling fn
// for each job and collecting the results in input order. Jobs that have not
// started when ctx is cancelled are skipped; completed[i] reports whether
// jobs[i] actually ran.
func Run[J any, R any](ctx context.Context, jobs []J, workers int, fn func(context.Context, J) R) (results []R, completed []bool) {
	results = make([]R, len(jobs))
	completed = make([]bool, len(jobs))
	if len(jobs) == 0 || workers < 1 {
		return results, completed
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}

	indexes := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range indexes {
				if ctx.Err() != nil {
					continue
				}
				results[index] = fn(ctx, jobs[index])
				completed[index] = true
			}
		}()
	}

	go func() {
		defer close(indexes)
		for index := range jobs {
			select {
			case <-ctx.Done():
				return
			case indexes <- index:
			}
		}
	}()

	wg.Wait()
	return results, completed
}

// NewReporter picks a reporter for the requested progress mode: an interactive
// TUI for multi-input terminal runs, plain logs otherwise, or nothing at all.
func NewReporter(out io.Writer, mode string, inputs []string, concurrency int, cancel context.CancelFunc) Reporter {
	useTUI := false
	switch mode {
	case ProgressTUI:
		useTUI = true
	case ProgressAuto:
		useTUI = len(inputs) > 1 && isTerminal(out)
	}
	if useTUI {
		return newTUIReporter(out, inputs, concurrency, cancel)
	}
	if mode == ProgressOff {
		return noopReporter{}
	}
	return &plainReporter{out: out, prefix: len(inputs) > 1}
}

type noopReporter struct{}

func (noopReporter) Report(Event) {}
func (noopReporter) Close()       {}

type plainReporter struct {
	out    io.Writer
	prefix bool
	mu     sync.Mutex
}

func (r *plainReporter) Report(event Event) {
	if event.Message == "" || event.Stage == pipeline.StageWrite {
		return
	}
	if !r.prefix && event.Stage == pipeline.StageQueued {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.prefix {
		fmt.Fprintf(r.out, "[%s] %s: %s\n", filepath.Base(event.Input), event.Stage, event.Message)
		return
	}
	fmt.Fprintf(r.out, "%s: %s\n", event.Stage, event.Message)
}

func (r *plainReporter) Close() {}

func isTerminal(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
