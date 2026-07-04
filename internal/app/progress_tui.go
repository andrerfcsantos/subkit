package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type tuiBatchReporter struct {
	program *tea.Program
	done    chan struct{}
}

func newTUIBatchReporter(out io.Writer, jobs []batchJob, cancel context.CancelFunc) batchReporter {
	model := newProgressModel(jobs, cancel)
	program := tea.NewProgram(model, tea.WithOutput(out), tea.WithInput(os.Stdin))
	reporter := &tuiBatchReporter{program: program, done: make(chan struct{})}
	go func() {
		_, _ = program.Run()
		close(reporter.done)
	}()
	return reporter
}

func (r *tuiBatchReporter) Report(event batchEvent) {
	r.program.Send(progressEventMsg(event))
}

func (r *tuiBatchReporter) Close() {
	select {
	case <-r.done:
		return
	default:
	}
	r.program.Send(progressDoneMsg{})
	<-r.done
}

type progressEventMsg batchEvent
type progressDoneMsg struct{}

type progressRow struct {
	Input   string
	Stage   pipeline.Stage
	Message string
	Failed  bool
	Done    bool
}

type progressModel struct {
	rows    []progressRow
	index   map[string]int
	width   int
	height  int
	cancel  context.CancelFunc
	closing bool
}

var (
	progressTitleStyle  = lipgloss.NewStyle().Bold(true)
	progressFailedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	progressDoneStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
)

func newProgressModel(jobs []batchJob, cancel context.CancelFunc) progressModel {
	rows := make([]progressRow, 0, len(jobs))
	index := map[string]int{}
	for _, job := range jobs {
		index[job.Input] = len(rows)
		rows = append(rows, progressRow{Input: job.Input, Stage: pipeline.StageQueued, Message: "queued"})
	}
	return progressModel{rows: rows, index: index, width: 80, height: 24, cancel: cancel}
}

func (m progressModel) Init() tea.Cmd {
	return nil
}

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			if m.cancel != nil {
				m.cancel()
			}
			m.closing = true
			return m, tea.Quit
		}
	case progressEventMsg:
		event := batchEvent(msg)
		if idx, ok := m.index[event.Input]; ok {
			row := &m.rows[idx]
			row.Stage = event.Stage
			row.Message = event.Message
			row.Failed = event.Stage == pipeline.StageFailed
			row.Done = event.Stage == pipeline.StageDone
		}
	case progressDoneMsg:
		m.closing = true
		return m, tea.Quit
	}
	return m, nil
}

func (m progressModel) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	height := m.height
	if height <= 0 {
		height = 24
	}

	lines := []string{
		progressTitleStyle.Render(truncateString("subkit batch progress", width)),
		truncateString(fmt.Sprintf("%d file(s)", len(m.rows)), width),
		"",
	}

	maxRows := height - len(lines)
	if maxRows < 0 {
		maxRows = 0
	}
	for i, row := range m.rows {
		if i >= maxRows {
			remaining := len(m.rows) - i
			lines = append(lines, truncateString(fmt.Sprintf("... %d more", remaining), width))
			break
		}
		lines = append(lines, renderProgressRow(row, width))
	}

	return strings.Join(lines, "\n")
}

func renderProgressRow(row progressRow, width int) string {
	status := "run"
	if row.Done {
		status = "done"
	}
	if row.Failed {
		status = "failed"
	}
	line := fmt.Sprintf("%-7s %-11s %-24s %s", status, row.Stage, filepath.Base(row.Input), row.Message)
	line = truncateString(line, width)
	if row.Failed {
		return progressFailedStyle.Render(line)
	}
	if row.Done {
		return progressDoneStyle.Render(line)
	}
	return line
}

func truncateString(value string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(value) <= maxLen {
		return value
	}
	if maxLen == 1 {
		return value[:1]
	}
	if maxLen <= 3 {
		return value[:maxLen]
	}
	return value[:maxLen-3] + "..."
}
