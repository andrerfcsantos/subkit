package pipeline

import (
	"fmt"
	"io"
	"sync"
)

type Stage string

const (
	StageQueued     Stage = "queued"
	StageAudio      Stage = "audio"
	StageTranscribe Stage = "transcribe"
	StageCues       Stage = "cues"
	StageRender     Stage = "render"
	StageWrite      Stage = "write"
	StageDone       Stage = "done"
	StageFailed     Stage = "failed"
)

type Event struct {
	Stage   Stage
	Message string
}

type Reporter interface {
	Report(Event)
}

type ReporterFunc func(Event)

func (f ReporterFunc) Report(event Event) {
	if f != nil {
		f(event)
	}
}

type WriterReporter struct {
	Out          io.Writer
	IncludeWrite bool
	mu           sync.Mutex
}

func (r *WriterReporter) Report(event Event) {
	if r == nil || r.Out == nil || event.Message == "" {
		return
	}
	if event.Stage == StageWrite && !r.IncludeWrite {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if event.Stage == "" {
		fmt.Fprintln(r.Out, event.Message)
		return
	}
	fmt.Fprintf(r.Out, "%s: %s\n", event.Stage, event.Message)
}
