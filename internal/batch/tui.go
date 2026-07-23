package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/paginator"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrerfcsantos/subkit-codex/internal/pipeline"
)

type tuiReporter struct {
	program *tea.Program
	done    chan struct{}
}

func newTUIReporter(out io.Writer, inputs []string, concurrency int, cancel context.CancelFunc) Reporter {
	model := newProgressModel(inputs, concurrency, cancel)
	program := tea.NewProgram(model, tea.WithOutput(out), tea.WithInput(os.Stdin))
	reporter := &tuiReporter{program: program, done: make(chan struct{})}
	go func() {
		_, _ = program.Run()
		close(reporter.done)
	}()
	return reporter
}

func (r *tuiReporter) Report(event Event) {
	r.program.Send(progressEventMsg(event))
}

func (r *tuiReporter) Close() {
	select {
	case <-r.done:
		return
	default:
	}
	r.program.Send(progressDoneMsg{})
	<-r.done
}

type progressEventMsg Event
type progressDoneMsg struct{}

type jobState int

const (
	stateQueued jobState = iota
	stateRunning
	stateDone
	stateCached
	stateFailed

	numStates
)

type progressRow struct {
	Input  string
	State  jobState
	Stage  pipeline.Stage
	Detail string
}

type progressModel struct {
	rows  []progressRow
	index map[string]int

	counts      [numStates]int
	total       int
	concurrency int

	width  int
	height int

	progress  progress.Model
	paginator paginator.Model
	spinner   spinner.Model

	// Layout derived from the terminal size and current counts. Computed by
	// applyLayout so Update (page clamping) and View (rendering) stay in sync.
	activeShown int
	activeMore  int
	failedShown int
	failedMore  int

	cancel  context.CancelFunc
	closing bool
}

const (
	// headerLines: title, progress bar, summary, blank spacer.
	headerLines = 4
	// footerLines: blank spacer, help line.
	footerLines = 2
	// maxFailedRows bounds the pinned FAILED section.
	maxFailedRows = 4
	// maxPageDots is the largest page count for which we draw the dot strip;
	// beyond it the dots would be noise, so we show just "page X/Y".
	maxPageDots = 12
)

var (
	progressTitleStyle   = lipgloss.NewStyle().Bold(true)
	progressSectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	progressDimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	progressQueuedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	progressRunningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	progressDoneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	progressCachedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	progressFailedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

func newProgressModel(inputs []string, concurrency int, cancel context.CancelFunc) progressModel {
	rows := make([]progressRow, 0, len(inputs))
	index := map[string]int{}
	for _, input := range inputs {
		index[input] = len(rows)
		rows = append(rows, progressRow{Input: input, State: stateQueued, Detail: "queued"})
	}

	if concurrency < 1 {
		concurrency = 1
	}

	var counts [numStates]int
	counts[stateQueued] = len(rows)

	prog := progress.New(progress.WithDefaultBlend())

	pg := paginator.New()
	pg.Type = paginator.Dots
	pg.PerPage = 1

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(progressRunningStyle))

	m := progressModel{
		rows:        rows,
		index:       index,
		counts:      counts,
		total:       len(rows),
		concurrency: concurrency,
		width:       80,
		height:      24,
		progress:    prog,
		paginator:   pg,
		spinner:     sp,
		cancel:      cancel,
	}
	m.applyLayout()
	return m
}

func (m progressModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.applyLayout()
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			if m.cancel != nil {
				m.cancel()
			}
			m.closing = true
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.paginator, cmd = m.paginator.Update(msg)
		return m, cmd
	case progressEventMsg:
		m.applyEvent(Event(msg))
		m.applyLayout()
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case progressDoneMsg:
		m.closing = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *progressModel) applyEvent(event Event) {
	idx, ok := m.index[event.Input]
	if !ok {
		return
	}
	row := &m.rows[idx]
	m.counts[row.State]--

	switch event.Stage {
	case pipeline.StageFailed:
		row.State = stateFailed
		row.Detail = firstLine(event.Message)
	case pipeline.StageDone:
		if event.Cached {
			row.State = stateCached
		} else {
			row.State = stateDone
		}
		row.Detail = event.Detail
	case pipeline.StageQueued:
		row.State = stateQueued
		row.Detail = "queued"
	default:
		row.State = stateRunning
		row.Detail = firstLine(event.Message)
	}
	row.Stage = event.Stage
	m.counts[row.State]++
}

// applyLayout recomputes the pinned-section sizes and the paginator geometry
// from the terminal size and the current per-state counts.
func (m *progressModel) applyLayout() {
	width := m.usableWidth()
	m.progress.SetWidth(min(width, 48))

	running := m.counts[stateRunning]
	failed := m.counts[stateFailed]
	rest := m.total - running - failed

	avail := m.height - headerLines - footerLines
	if avail < 3 {
		avail = 3
	}

	// FAILED section (bounded so errors never scroll off, but never dominate).
	failedShown := min(failed, maxFailedRows)
	m.failedShown = failedShown
	m.failedMore = failed - failedShown
	failedLines := 0
	if failed > 0 {
		failedLines = 1 + failedShown // section header + rows
		if m.failedMore > 0 {
			failedLines++
		}
	}

	// Reserve a minimum body region when there are queued/done rows to page.
	minBody := 0
	if rest > 0 {
		minBody = 2 // body header + at least one row
	}

	// ACTIVE section gets what's left after failed rows and the reserved body.
	activeShown := running
	activeMore := 0
	if running > 0 {
		budget := avail - failedLines - minBody - 1 // -1 for the ACTIVE header
		if budget < 0 {
			budget = 0
		}
		if activeShown > budget {
			activeShown = budget
			activeMore = running - activeShown
		}
	}
	m.activeShown = activeShown
	m.activeMore = activeMore
	activeLines := 0
	if running > 0 {
		activeLines = 1 + activeShown
		if activeMore > 0 {
			activeLines++
		}
	}

	// The paginated body gets the remainder.
	perPage := avail - activeLines - failedLines - 1 // -1 for the body header
	if perPage < 1 {
		perPage = 1
	}
	m.paginator.PerPage = perPage
	if rest < 1 {
		m.paginator.TotalPages = 1
	} else {
		m.paginator.SetTotalPages(rest)
	}
	if m.paginator.Page >= m.paginator.TotalPages {
		m.paginator.Page = m.paginator.TotalPages - 1
	}
	if m.paginator.Page < 0 {
		m.paginator.Page = 0
	}
}

func (m progressModel) View() tea.View {
	width := m.usableWidth()

	completed := m.counts[stateDone] + m.counts[stateCached] + m.counts[stateFailed]
	percent := 0.0
	if m.total > 0 {
		percent = float64(completed) / float64(m.total)
	}

	lines := []string{
		progressTitleStyle.Render(truncateDisplay("subkit batch progress", width)),
		m.progress.ViewAs(percent),
		truncateDisplay(m.summaryLine(), width),
		"",
	}

	// Group rows by state, preserving job order within each group.
	var running, failed, queued, doneish []int
	for i := range m.rows {
		switch m.rows[i].State {
		case stateRunning:
			running = append(running, i)
		case stateFailed:
			failed = append(failed, i)
		case stateQueued:
			queued = append(queued, i)
		default:
			doneish = append(doneish, i)
		}
	}
	// Body order: queued first, then completed (done/cached).
	rest := append(queued, doneish...)

	// ACTIVE section (pinned).
	if len(running) > 0 {
		lines = append(lines, progressSectionStyle.Render("ACTIVE"))
		for k, idx := range running {
			if k >= m.activeShown {
				break
			}
			lines = append(lines, m.renderRow(m.rows[idx], width))
		}
		if m.activeMore > 0 {
			lines = append(lines, progressDimStyle.Render(fmt.Sprintf("  +%d more running", m.activeMore)))
		}
	}

	// FAILED section (pinned).
	if len(failed) > 0 {
		lines = append(lines, progressSectionStyle.Render("FAILED"))
		for k, idx := range failed {
			if k >= m.failedShown {
				break
			}
			lines = append(lines, m.renderRow(m.rows[idx], width))
		}
		if m.failedMore > 0 {
			lines = append(lines, progressDimStyle.Render(fmt.Sprintf("  +%d more failed", m.failedMore)))
		}
	}

	// QUEUED / DONE section (paginated).
	if len(rest) > 0 {
		lines = append(lines, m.bodyHeader(width))
		start, end := m.paginator.GetSliceBounds(len(rest))
		for _, idx := range rest[start:end] {
			lines = append(lines, m.renderRow(m.rows[idx], width))
		}
	}

	lines = append(lines, "", progressDimStyle.Render(truncateDisplay("←/→ page · q quit", width)))

	return tea.NewView(strings.Join(lines, "\n"))
}

func (m progressModel) summaryLine() string {
	completed := m.counts[stateDone] + m.counts[stateCached] + m.counts[stateFailed]
	sep := progressDimStyle.Render(" · ")

	parts := []string{
		progressDoneStyle.Render(fmt.Sprintf("%d done", m.counts[stateDone])),
		progressCachedStyle.Render(fmt.Sprintf("%d cached", m.counts[stateCached])),
	}
	if m.counts[stateFailed] > 0 {
		parts = append(parts, progressFailedStyle.Render(fmt.Sprintf("%d failed", m.counts[stateFailed])))
	}
	parts = append(parts,
		progressRunningStyle.Render(fmt.Sprintf("%d running", m.counts[stateRunning])),
		progressQueuedStyle.Render(fmt.Sprintf("%d queued", m.counts[stateQueued])),
	)

	return fmt.Sprintf("%d/%d  ", completed, m.total) + strings.Join(parts, sep)
}

func (m progressModel) bodyHeader(width int) string {
	left := progressSectionStyle.Render("QUEUED / DONE")
	if m.paginator.TotalPages <= 1 {
		return left
	}
	right := progressDimStyle.Render(fmt.Sprintf("page %d/%d", m.paginator.Page+1, m.paginator.TotalPages))
	if m.paginator.TotalPages <= maxPageDots {
		right += " " + m.paginator.View()
	}
	// When the terminal is too narrow for both, keep the page indicator.
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > width {
		return truncateDisplay(right, width)
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	return left + strings.Repeat(" ", gap) + right
}

func (m progressModel) renderRow(row progressRow, width int) string {
	var glyph string
	switch row.State {
	case stateRunning:
		glyph = m.spinner.View()
	case stateQueued:
		glyph = progressQueuedStyle.Render("○")
	case stateDone:
		glyph = progressDoneStyle.Render("✓")
	case stateCached:
		glyph = progressCachedStyle.Render("✓")
	case stateFailed:
		glyph = progressFailedStyle.Render("✗")
	}

	name := filepath.Base(row.Input)
	detail := firstLine(row.Detail)
	detailStyle := progressDimStyle
	if row.State == stateFailed {
		detailStyle = progressFailedStyle
	}

	line := "  " + glyph + " " + name
	if detail != "" {
		line += "  " + detailStyle.Render(detail)
	}
	return truncateDisplay(line, width)
}

func (m progressModel) usableWidth() int {
	if m.width <= 0 {
		return 80
	}
	return m.width
}

func firstLine(value string) string {
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

// truncateDisplay clips value to a display width of maxLen, appending an
// ellipsis, while preserving embedded ANSI styling.
func truncateDisplay(value string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= maxLen {
		return value
	}
	return ansi.Truncate(value, maxLen, "…")
}
